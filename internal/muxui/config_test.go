package muxui

import (
	"cmp"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/configstore"
)

// fakeDaemon keeps the versions it was given, checked with the trusted key,
// as the daemon's /warden/config does, and refuses what refuse names.
type fakeDaemon struct {
	*httptest.Server
	mu       sync.Mutex
	pub      ed25519.PublicKey
	current  *configstore.Bundle
	versions map[string]*configstore.Bundle
	refuse   string
	result   string
	headers  []string
}

func newFakeDaemon(t *testing.T, pub ed25519.PublicKey) *fakeDaemon {
	d := &fakeDaemon{pub: pub, versions: map[string]*configstore.Bundle{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /warden/config", func(rw http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.current == nil {
			writeJSON(rw, http.StatusNotFound, map[string]string{"detail": "no signed config yet"})
			return
		}
		writeJSON(rw, http.StatusOK, d.current.Wire())
	})
	mux.HandleFunc("GET /warden/config/version/{hash}", func(rw http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		b, ok := d.versions[r.PathValue("hash")]
		if !ok {
			writeJSON(rw, http.StatusNotFound, map[string]string{"detail": "no such version"})
			return
		}
		writeJSON(rw, http.StatusOK, b.Wire())
	})
	mux.HandleFunc("POST /warden/config", func(rw http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.headers = append(d.headers, r.Header.Get("X-InferMux")+" "+r.Header.Get("Authorization"))
		var w configstore.Wire
		json.NewDecoder(r.Body).Decode(&w)
		b, err := w.Bundle()
		if err == nil {
			_, err = b.Verify([]ed25519.PublicKey{d.pub}, time.Now())
		}
		if err == nil && d.refuse != "" && strings.Contains(w.Files["infermux.yaml"], d.refuse) {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			writeJSON(rw, http.StatusUnprocessableEntity, map[string]string{"detail": err.Error()})
			return
		}
		d.versions[b.Hash()] = b
		result := cmp.Or(d.result, "applied")
		if result == "applied" {
			d.current = b
		}
		writeJSON(rw, http.StatusOK, map[string]any{"result": result, "state": map[string]any{}})
	})
	d.Server = httptest.NewServer(mux)
	t.Cleanup(d.Close)
	return d
}

func (d *fakeDaemon) running() *configstore.Bundle {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

type configUI struct {
	ui
	daemon *fakeDaemon
}

func newConfigUI(t *testing.T) configUI {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	dir := filepath.Dir(f.store.WardenFile)
	encrypted, err := configstore.EncryptKey(priv, "the passphrase")
	if err != nil {
		t.Fatal(err)
	}
	f.store.ConfigKey = filepath.Join(dir, "config.key")
	os.WriteFile(f.store.ConfigKey, encrypted, 0o600)
	d := newFakeDaemon(t, pub)
	u, _ := url.Parse(d.URL)
	return configUI{ui: ui{t: t, f: f, h: Handler(f.store, &Builder{}, u, "ui-key", "")}, daemon: d}
}

func draft(gguf, extra string) map[string]string {
	return map[string]string{
		"infermux.yaml":    "hosts:\n  here:\n    devices:\n      gpu: {kind: gpu}\n      cpu: {kind: cpu}\nbalance:\n  qwen: [here/gpu]\nfailover:\n  qwen: [here/cpu]\n" + extra,
		"models/qwen.yaml": "cmd: llama-server --port ${PORT} --model " + gguf + "\nfiles:\n  " + gguf + ": {sha256: " + strings.Repeat("a", 64) + "}\n",
	}
}

func TestCheckIsTheDaemonsLoader(t *testing.T) {
	u := newConfigUI(t)
	code, out := u.call("POST", "/api/config/check", map[string]any{"files": draft(u.f.gguf, "")})
	u.want(code, 200, out, "check")
	if out["error"] != nil {
		t.Fatalf("error %v", out["error"])
	}
	routes := out["routes"].(map[string]any)["qwen"].([]any)
	if len(routes) != 2 || routes[1].(map[string]any)["failover"] != true {
		t.Errorf("routes %v", routes)
	}
	if devices := out["devices"].(map[string]any)["here"].(map[string]any); devices["cpu"] != "cpu" || devices["gpu"] != "gpu" {
		t.Errorf("devices %v", devices)
	}
	broken := draft(u.f.gguf, "")
	broken["models/qwen.yaml"] = "cmd: x\nfiles:\n  /a.gguf: {sha256: [\n"
	_, out = u.call("POST", "/api/config/check", map[string]any{"files": broken})
	if p, _ := out["error"].(map[string]any); p == nil || p["file"] != "models/qwen.yaml" || p["line"] != float64(3) {
		t.Errorf("a YAML error: %v", out["error"])
	}
	// A config that loads but that a host could not run.
	tpu := draft(u.f.gguf, "")
	tpu["infermux.yaml"] = strings.Replace(tpu["infermux.yaml"], "[here/gpu]", "[here/tpu]", 1)
	_, out = u.call("POST", "/api/config/check", map[string]any{"files": tpu})
	if p, _ := out["error"].(map[string]any); p == nil || !strings.Contains(p["message"].(string), `here has no device "tpu"`) || p["file"] != "" {
		t.Errorf("a placement error: %v", out["error"])
	}
	// One only the host's own part raises, which the routes do not.
	env := draft(u.f.gguf, "")
	env["models/qwen.yaml"] += "env: [CUDA_VISIBLE_DEVICES=1]\n"
	_, out = u.call("POST", "/api/config/check", map[string]any{"files": env})
	if p, _ := out["error"].(map[string]any); p == nil || !strings.Contains(p["message"].(string), "here: qwen on here/gpu: env sets CUDA_VISIBLE_DEVICES") {
		t.Errorf("a host's error: %v", out["error"])
	}
}

func TestPublishTakesThePassphraseFromTheCommandWhenTheBrowserSendsNone(t *testing.T) {
	u := newConfigUI(t)
	dir := t.TempDir()
	lookup := filepath.Join(dir, "lookup")
	os.WriteFile(lookup, []byte("#!/bin/sh\nprintf 'the passphrase\\n'\n"), 0o755)
	u.f.store.ConfigPassphrase = lookup
	if code, out := u.call("GET", "/api/state", nil); code != 200 || out["config_passphrase"] != true {
		t.Fatalf("the state does not say Publish needs no passphrase: %d %v", code, out["config_passphrase"])
	}
	code, out := u.call("POST", "/api/config/publish", map[string]any{"files": draft(u.f.gguf, ""), "base": ""})
	u.want(code, 200, out, "a publish with the command's passphrase")
	if u.daemon.running() == nil {
		t.Fatal("the daemon got nothing")
	}

	// A locked or missing keyring is the command's failure, and says so.
	os.WriteFile(lookup, []byte("#!/bin/sh\necho 'no such secret' >&2\nexit 1\n"), 0o755)
	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": draft(u.f.gguf, "# again\n"), "base": u.daemon.running().Hash()})
	if code == 200 || !strings.Contains(out["detail"].(string), "no such secret") {
		t.Errorf("a failing command: %d %v", code, out)
	}
}

func TestPublishSignsHandsOverAndWritesTheCopy(t *testing.T) {
	u := newConfigUI(t)
	files := draft(u.f.gguf, "")
	code, out := u.call("POST", "/api/config/publish", map[string]any{"files": files, "base": "", "passphrase": "wrong"})
	u.want(code, http.StatusUnprocessableEntity, out, "a wrong passphrase")
	if !strings.Contains(out["detail"].(string), "does not open the config key") || u.daemon.running() != nil {
		t.Fatalf("%v", out)
	}

	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": files, "base": "", "passphrase": "the passphrase"})
	u.want(code, 200, out, "the first publish")
	first := u.daemon.running()
	if first == nil || first.Info().Host != "here" || first.Info().Revision != 1 {
		t.Fatalf("the daemon got %+v", first)
	}
	if got := out["published"].(map[string]any)["hash"]; got != first.Hash() {
		t.Errorf("published %v, the daemon runs %s", got, first.Hash())
	}
	copied, err := configstore.ReadBundle(u.f.store.SignedConfig)
	if err != nil || copied.Hash() != first.Hash() {
		t.Fatalf("nixcfg's copy: %v", err)
	}
	if !slices.Equal(u.daemon.headers, []string{"infermux-ui Bearer ui-key"}) {
		t.Errorf("the daemon saw %q", u.daemon.headers)
	}

	files["models/qwen.yaml"] += "ttl: 300\n"
	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": files, "base": first.Hash(), "passphrase": "the passphrase"})
	u.want(code, 200, out, "an edit")
	second := u.daemon.running()
	if m := second.Info(); m.Revision != 2 || m.Parent != first.Hash() {
		t.Errorf("the edit: %+v", m)
	}

	// A draft begun from the first version, after the second.
	files["models/qwen.yaml"] += "unlisted: true\n"
	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": files, "base": first.Hash(), "passphrase": "the passphrase"})
	u.want(code, http.StatusConflict, out, "a stale draft")
	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": files, "base": first.Hash(), "passphrase": "the passphrase", "force": true})
	u.want(code, 200, out, "a stale draft, forced")
	if m := u.daemon.running().Info(); m.Parent != first.Hash() {
		t.Errorf("forced: the parent is %s, so the daemon would not see the conflict", m.Parent)
	}

	// Neither a draft that does not load nor one the daemon refuses
	// reaches nixcfg.
	before := u.daemon.running().Hash()
	broken := draft(u.f.gguf, "  nothing: [here/gpu]\n")
	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": broken, "base": before, "passphrase": "the passphrase"})
	u.want(code, http.StatusUnprocessableEntity, out, "a broken draft")
	if out["problem"] == nil || u.daemon.running().Hash() != before {
		t.Errorf("a broken draft: %v", out)
	}
	u.daemon.refuse = "# refuse me"
	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": draft(u.f.gguf, "# refuse me\n"), "base": before, "passphrase": "the passphrase"})
	u.want(code, http.StatusUnprocessableEntity, out, "a draft the daemon refuses")
	if copied, _ := configstore.ReadBundle(u.f.store.SignedConfig); copied.Hash() != before {
		t.Error("nixcfg's copy took a version the daemon refused")
	}
	// Nor one it keeps without running it.
	u.daemon.refuse, u.daemon.result = "", "lost"
	code, out = u.call("POST", "/api/config/publish", map[string]any{"files": draft(u.f.gguf, "# lost\n"), "base": before, "passphrase": "the passphrase"})
	u.want(code, http.StatusUnprocessableEntity, out, "a draft the daemon keeps as a conflict's loser")
	if copied, _ := configstore.ReadBundle(u.f.store.SignedConfig); copied.Hash() != before {
		t.Error("nixcfg's copy took a version the daemon does not run")
	}
}

func TestCopyBringsTheRunningVersionToNixcfg(t *testing.T) {
	u := newConfigUI(t)
	code, out := u.call("GET", "/api/config/copy", nil)
	u.want(code, 200, out, "no copy")
	if out["copy"] != nil {
		t.Fatalf("%v", out)
	}
	code, out = u.call("POST", "/api/config/copy", nil)
	u.want(code, http.StatusUnprocessableEntity, out, "nothing running")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	u.daemon.pub = priv.Public().(ed25519.PublicKey)
	b, err := configstore.Sign(toBytes(draft(u.f.gguf, "")), priv, "zbox", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	u.daemon.current = b
	code, out = u.call("POST", "/api/config/copy", nil)
	u.want(code, 200, out, "copy")
	if got := out["copy"].(map[string]any); got["hash"] != b.Hash() || got["host"] != "zbox" {
		t.Errorf("copy %v", got)
	}
	if !slices.Contains(u.f.store.paths(), u.f.store.SignedConfig) {
		t.Errorf("Commit would not take the copy: %q", u.f.store.paths())
	}
}

func TestHashAndHuggingFace(t *testing.T) {
	u := newConfigUI(t)
	code, out := u.call("POST", "/api/config/hash", map[string]any{"path": u.f.gguf})
	u.want(code, 200, out, "hash")
	// sha256("GGUF")
	if out["sha256"] != "b83633aa785344791618f2fddf131b010ea04912a60430760b070bad293f65bd" || out["bytes"] != float64(4) {
		t.Errorf("%v", out)
	}
	code, out = u.call("POST", "/api/config/hash", map[string]any{"path": u.f.store.WardenFile})
	u.want(code, http.StatusUnprocessableEntity, out, "a file outside the GGUF directories")

	lfs := strings.Repeat("b", 64)
	hf := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/repo/resolve/main/m.gguf":
			rw.Header().Set("X-Linked-Etag", `"`+lfs+`"`)
			rw.Header().Set("X-Linked-Size", "123")
		case "/org/repo/resolve/main/small.gguf":
			rw.Header().Set("Etag", `"0123456789012345678901234567890123456789"`)
		default:
			http.NotFound(rw, r)
		}
	}))
	defer hf.Close()
	u.f.store.HF = &HuggingFace{BaseURL: hf.URL}
	code, out = u.call("GET", "/api/config/hf?source=org/repo/m.gguf", nil)
	u.want(code, 200, out, "an LFS file")
	if out["sha256"] != lfs || out["bytes"] != float64(123) {
		t.Errorf("%v", out)
	}
	code, out = u.call("GET", "/api/config/hf?source=org/repo/small.gguf", nil)
	u.want(code, http.StatusUnprocessableEntity, out, "a file in git")
	if !strings.Contains(out["detail"].(string), "not an LFS file") {
		t.Errorf("%v", out)
	}
}

func TestAPublishMakesTheLastBuildStale(t *testing.T) {
	u := newConfigUI(t)
	b := &Builder{Installable: "flake#unit", run: func([]string, io.Writer, io.Writer) error { return nil }}
	d, _ := url.Parse(u.daemon.URL)
	u.h = Handler(u.f.store, b, d, "ui-key", "")
	b.Start()
	for b.State().Running {
		time.Sleep(time.Millisecond)
	}
	if b.State().Stale {
		t.Fatal("stale before any change")
	}
	code, out := u.call("POST", "/api/config/publish", map[string]any{"files": draft(u.f.gguf, ""), "base": "", "passphrase": "the passphrase"})
	u.want(code, 200, out, "publish")
	if !b.State().Stale {
		t.Fatal("not stale after a Publish")
	}
	b.Start()
	if b.State().Stale {
		t.Fatal("a new build is still stale")
	}
}
