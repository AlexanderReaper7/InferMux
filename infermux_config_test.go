package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/configstore"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/remote"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
)

// configHost is one host's side of the one config, served over HTTP with
// none of the warden's guard in front.
type configHost struct {
	one     *oneConfig
	url     string
	reloads atomic.Int32
}

func newConfigHosts(t *testing.T, trusted []ed25519.PublicKey, names ...string) map[string]*configHost {
	t.Helper()
	logs := logmon.NewGroup(io.Discard, true, true, true)
	hosts := map[string]*configHost{}
	for _, name := range names {
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		dir := t.TempDir()
		one, err := openConfig(name, filepath.Join(dir, "state"), filepath.Join(dir, "derived"), trusted, logs.ProxyLogs)
		if err != nil {
			t.Fatal(err)
		}
		h := &configHost{one: one, url: srv.URL}
		one.reload = func() { h.reloads.Add(1) }
		one.hostKey = func() (string, error) { return "host", nil }
		one.handle(mux.Handle)
		if !one.start("") {
			t.Fatalf("%s: the first start wrote no derived file", name)
		}
		hosts[name] = h
	}
	return hosts
}

func (h *configHost) derived(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.one.derivedDir, derivedFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (h *configHost) running() string {
	if b := h.one.store.Current(); b != nil {
		return b.Hash()
	}
	return ""
}

// send is a request as another host or a client makes it.
func (h *configHost) send(t *testing.T, method, path string, b *configstore.Bundle, hop bool) (int, string) {
	t.Helper()
	var body io.Reader
	if b != nil {
		raw, err := json.Marshal(b.Wire())
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.url+path, body)
	if hop {
		req.Header.Set(remote.HopHeader, "config")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

func twoHostFiles(a, b, balance, model string) map[string][]byte {
	return map[string][]byte{
		"infermux.yaml": []byte(fmt.Sprintf("hosts:\n  a:\n    url: %s\n    devices:\n      gpu: {kind: gpu}\n  b:\n    url: %s\n    devices:\n      gpu: {kind: gpu}\nbalance:\n  m: %s\n", a, b, balance)),
		"models/m.yaml": []byte(model),
	}
}

// A Publish reaches every host, a host that missed one fetches it, two
// versions edited apart end as one running and a warning on both hosts, and
// a config that does not run here is never stored over one that does.
func TestOneConfigBetweenTwoHosts(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hosts := newConfigHosts(t, []ed25519.PublicKey{pub}, "a", "b")
	a, b := hosts["a"], hosts["b"]
	model := "cmd: llama-server --model /m.gguf --port ${PORT}\n"
	base := time.Now()
	sign := func(files map[string][]byte, key ed25519.PrivateKey, host string, parent *configstore.Bundle, at time.Duration) *configstore.Bundle {
		t.Helper()
		bundle, err := configstore.Sign(files, key, host, parent, base.Add(at))
		if err != nil {
			t.Fatal(err)
		}
		return bundle
	}
	for name, h := range hosts {
		if d := h.derived(t); strings.Contains(d, "models:") {
			t.Errorf("%s runs a model before any config:\n%s", name, d)
		}
	}

	// Published at a: a runs it and pushes it to b.
	rev1 := sign(twoHostFiles(a.url, b.url, "[a/gpu, b/gpu]", model), priv, "a", nil, 0)
	if code, body := a.send(t, "POST", "/warden/config", rev1, false); code != 200 || !strings.Contains(body, `"result": "applied"`) {
		t.Fatalf("publish: %d %s", code, body)
	}
	eventually(t, "b runs revision 1", func() bool { return b.running() == rev1.Hash() })
	for name, h := range hosts {
		if d := h.derived(t); !strings.Contains(d, "m:") || !strings.Contains(d, "CUDA_VISIBLE_DEVICES=0") {
			t.Errorf("%s's derived file:\n%s", name, d)
		}
		if n := h.reloads.Load(); n != 1 {
			t.Errorf("%s reloaded llama-swap %d times, want 1", name, n)
		}
	}

	// Not signed by a trusted key, or changed after signing: refused, and
	// nothing changes.
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	tampered := sign(twoHostFiles(a.url, b.url, "[b/gpu]", model), priv, "a", rev1, time.Second)
	tampered.Files["models/m.yaml"] = []byte("cmd: something else\n")
	for name, bad := range map[string]*configstore.Bundle{
		"stranger": sign(twoHostFiles(a.url, b.url, "[b/gpu]", model), stranger, "a", rev1, time.Second),
		"tampered": tampered,
	} {
		if code, body := a.send(t, "POST", "/warden/config", bad, false); code != 422 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if a.running() != rev1.Hash() || a.reloads.Load() != 1 {
		t.Errorf("a refused config changed what runs")
	}

	// Published at b while a missed it: a's poll fetches it.
	rev2 := sign(twoHostFiles(a.url, b.url, "[b/gpu]", model), priv, "b", rev1, 2*time.Second)
	if code, body := b.send(t, "POST", "/warden/config", rev2, true); code != 200 {
		t.Fatalf("rev2 at b: %d %s", code, body)
	}
	// A push is a goroutine: give one that should not happen time to land.
	time.Sleep(200 * time.Millisecond)
	if a.running() != rev1.Hash() {
		t.Fatal("a hop-marked config was pushed on")
	}
	if err := a.one.pull("b", b.url, "host"); err != nil {
		t.Fatal(err)
	}
	if a.running() != rev2.Hash() {
		t.Fatal("a did not fetch b's newer config")
	}
	if d := a.derived(t); strings.Contains(d, "m:") {
		t.Errorf("a still runs m after it moved to b:\n%s", d)
	}

	// Edited apart: each host publishes from revision 2 while the other is
	// out of reach. The later one runs everywhere, and both hosts keep the
	// earlier one and say so.
	rev3a := sign(twoHostFiles(a.url, b.url, "[a/gpu]", model), priv, "a", rev2, 3*time.Second)
	rev3b := sign(twoHostFiles(a.url, b.url, "[a/gpu, b/gpu]", model), priv, "b", rev2, 4*time.Second)
	if code, body := a.send(t, "POST", "/warden/config", rev3a, true); code != 200 {
		t.Fatalf("rev3a: %d %s", code, body)
	}
	if code, body := b.send(t, "POST", "/warden/config", rev3b, true); code != 200 {
		t.Fatalf("rev3b: %d %s", code, body)
	}
	if err := a.one.pull("b", b.url, "host"); err != nil {
		t.Fatal(err)
	}
	if a.running() != rev3b.Hash() {
		t.Fatal("a does not run the later of the two")
	}
	eventually(t, "b hears it won", func() bool { return b.one.store.Conflict() != nil })
	for name, h := range hosts {
		c := h.one.store.Conflict()
		if c == nil || c.Kept.Hash != rev3b.Hash() || c.Lost.Hash != rev3a.Hash() {
			t.Errorf("%s's conflict: %+v", name, c)
		}
		if h.running() != rev3b.Hash() {
			t.Errorf("%s runs %s", name, h.running())
		}
	}
	if code, _ := a.send(t, "GET", "/warden/config/version/"+rev3a.Hash(), nil, false); code != 200 {
		t.Errorf("the lost version is not kept at a: %d", code)
	}
	if code, body := a.send(t, "DELETE", "/warden/config/conflict", nil, false); code != 204 {
		t.Fatalf("dismiss: %d %s", code, body)
	}
	eventually(t, "the dismissal reaches b", func() bool { return b.one.store.Conflict() == nil })
	if a.one.store.Conflict() != nil {
		t.Error("a still shows the conflict")
	}

	// Signed, but a cannot run it: refused before a's store sees it, and a's
	// poll does not fetch it again from b, which can.
	broken := sign(twoHostFiles(a.url, b.url, "[a/gpu, b/gpu]", "cmd: x\nenv: [CUDA_VISIBLE_DEVICES=1]\n"), priv, "b", rev3b, 5*time.Second)
	if code, body := a.send(t, "POST", "/warden/config", broken, true); code != 422 || !strings.Contains(body, "does not load here") {
		t.Fatalf("broken at a: %d %s", code, body)
	}
	if code, body := b.send(t, "POST", "/warden/config", broken, true); code != 422 {
		t.Fatalf("broken at b: %d %s", code, body)
	}
	// One that only b cannot run reaches a by the poll, and the other way.
	onlyA := sign(twoHostFiles(a.url, b.url, "[b/gpu]", "cmd: x\nenv: [CUDA_VISIBLE_DEVICES=1]\n"), priv, "a", rev3b, 6*time.Second)
	if code, body := a.send(t, "POST", "/warden/config", onlyA, true); code != 200 {
		t.Fatalf("onlyA at a: %d %s", code, body)
	}
	if err := b.one.pull("a", a.url, "host"); err == nil || !strings.Contains(err.Error(), "does not load here") {
		t.Fatalf("b's first pull: %v", err)
	}
	reloads := b.reloads.Load()
	if err := b.one.pull("a", a.url, "host"); err != nil {
		t.Fatalf("b fetched a refused config again: %v", err)
	}
	if b.running() != rev3b.Hash() || b.reloads.Load() != reloads {
		t.Error("b runs what it refused")
	}
	_, state := b.send(t, "GET", "/warden/config/state", nil, false)
	if !strings.Contains(state, onlyA.Hash()) || !strings.Contains(state, "which the device decides") {
		t.Errorf("b's state does not say what it refused:\n%s", state)
	}
}

// startWarden under the one config: the seed runs, the other hosts and the
// routes come from it, the derived file is written, and llama-swap reloads
// once it can hear the signal.
func TestProxy_OneConfigRuns(t *testing.T) {
	var discovery, fileKey atomic.Value
	var zboxDown atomic.Bool
	weights := bytes.Repeat([]byte("weights."), 1000)
	release := make(chan struct{})
	zbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			discovery.Store(r.Header.Get("Authorization"))
			// Unlisted: only the config's route can send embed here.
			w.Write([]byte(`{"data":[]}`))
		case strings.HasPrefix(r.URL.Path, "/warden/files/"):
			fileKey.Store(r.Header.Get("Authorization"))
			<-release
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(weights))
		case zboxDown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.Write([]byte("zbox"))
		}
	}))
	defer zbox.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	dir := t.TempDir()
	modelFile := filepath.Join(dir, "models", "e.gguf")
	t.Setenv("STATE_DIRECTORY", dir)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(dir, "seed")
	bundle, err := configstore.Sign(map[string][]byte{
		"infermux.yaml":     []byte(fmt.Sprintf("hosts:\n  test:\n    url: http://127.0.0.1:1\n    devices:\n      gpu: {kind: gpu}\n  zbox:\n    url: %s\n    devices:\n      gpu: {kind: gpu}\nbalance:\n  embed: [zbox/gpu]\nfailover:\n  embed: [test/gpu]\n", zbox.URL)),
		"models/embed.yaml": []byte(fmt.Sprintf("cmd: llama-server --model %s --port ${PORT}\nfiles:\n  %s: {sha256: %x}\n", modelFile, modelFile, sha256.Sum256(weights))),
	}, priv, "test", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := configstore.WriteBundle(seed, bundle); err != nil {
		t.Fatal(err)
	}
	keys := write("trusted", configstore.PublicKeyString(pub)+"\n")
	hostKey := write("hostkey", "host-secret\n")
	wfile := write("warden.yaml", fmt.Sprintf("host: test\nremote_key_file: %s\npolicy:\n  enabled: false\n", hostKey))
	*flagConfigTrustedKeys, *flagConfigSeed, *flagFetchDirs = keys, seed, filepath.Dir(modelFile)
	t.Cleanup(func() { *flagConfigTrustedKeys, *flagConfigSeed, *flagFetchDirs = "", "", "" })
	var reloads atomic.Int32
	reloadSignal = func() { reloads.Add(1) }
	t.Cleanup(func() { reloadSignal = func() { syscall.Kill(os.Getpid(), syscall.SIGHUP) } })

	// embed as the derived file has it, which the reload above only counts.
	cfg, err := config.LoadConfigFromReader(strings.NewReader("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  embed:\n    cmd: llama-server --model " + modelFile + " --port ${PORT}\n"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs := logmon.NewGroup(io.Discard, true, true, true)
	srv, err := server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(5 * time.Second)
	httpServer := &http.Server{Handler: srv}
	derivedDir := filepath.Join(dir, "derived")
	startWarden(wfile, "", derivedDir, httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()

	derived, err := os.ReadFile(filepath.Join(derivedDir, derivedFile))
	if err != nil || !strings.Contains(string(derived), "embed:") {
		t.Fatalf("derived file: %v\n%s", err, derived)
	}
	if httpServer.BaseContext == nil {
		t.Fatal("the derived file changed and no reload waits for llama-swap")
	}
	if reloads.Load() != 0 {
		t.Fatal("reloaded before llama-swap serves")
	}
	httpServer.BaseContext(nil)
	httpServer.BaseContext(nil)
	eventually(t, "llama-swap reloads", func() bool { return reloads.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := reloads.Load(); n != 1 {
		t.Errorf("reloaded %d times", n)
	}

	resp, err := http.Get(front.URL + "/warden/config/state")
	if err != nil {
		t.Fatal(err)
	}
	state, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(state), bundle.Hash()) || !strings.Contains(string(state), `"failover": true`) {
		t.Errorf("state: %d\n%s", resp.StatusCode, state)
	}
	eventually(t, "zbox's models read with this host's key", func() bool { return discovery.Load() == "Bearer host-secret" })
	resp, err = http.Post(front.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"embed","input":["hello"]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "zbox" {
		t.Errorf("embed went to %d %q, want zbox, the config's balance lane", resp.StatusCode, body)
	}

	// The model's file is not here, so it comes from zbox, asked with this
	// host's key, and until it is here a request that fails over to this
	// host is refused before llama-swap starts anything.
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	eventually(t, "the file asked of zbox with this host's key", func() bool { return fileKey.Load() == "Bearer host-secret" })
	if _, state := get("/warden/config/state"); !strings.Contains(state, `"state": "fetching"`) || !strings.Contains(state, modelFile) {
		t.Errorf("state while fetching:\n%s", state)
	}
	zboxDown.Store(true)
	resp, err = http.Post(front.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"embed","input":["hello"]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "embed is not here yet: "+modelFile+" fetching") {
		t.Errorf("embed with its file missing: %d %q", resp.StatusCode, body)
	}
	for _, path := range []string{modelFile, hostKey} {
		if status, _ := get("/warden/files" + path); status != http.StatusNotFound {
			t.Errorf("%s served: %d", path, status)
		}
	}
	close(release)
	eventually(t, "the file arrives", func() bool {
		got, _ := os.ReadFile(modelFile)
		return bytes.Equal(got, weights)
	})
	eventually(t, "the file counts as here", func() bool {
		_, state := get("/warden/config/state")
		return strings.Contains(state, `"state": "present"`)
	})
	if status, served := get("/warden/files" + modelFile); status != http.StatusOK || served != string(weights) {
		t.Errorf("serving the file: %d, %d bytes", status, len(served))
	}
}

// A CPU device's model starts through the runner when there is one, and
// says where it runs when there is not.
func TestCPUModelsGoThroughTheRunner(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"infermux.yaml": []byte("hosts:\n  a:\n    url: http://a\n    devices:\n      cpu: {kind: cpu}\n      gpu: {kind: gpu}\nbalance:\n  m: [a/cpu]\n  n: [a/gpu]\n"),
		"models/m.yaml": []byte("cmd: llama-server --model /m.gguf --port ${PORT}\n"),
		"models/n.yaml": []byte("cmd: llama-server --model /n.gguf --port ${PORT}\n"),
	}
	bundle, err := configstore.Sign(files, priv, "a", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, runner := range [][]string{nil, {"/bin/infermux-cpu", "run", "-socket", "/run/infermux-cpu/socket", "--"}} {
		h := newConfigHosts(t, []ed25519.PublicKey{pub}, "a")["a"]
		h.one.options.CPURunner = runner
		if _, err := h.one.accept(bundle, "test"); err != nil {
			t.Fatal(err)
		}
		derived := h.derived(t)
		warned := slices.ContainsFunc(h.one.state().Warnings, func(w string) bool { return strings.Contains(w, "m runs in the daemon's own cgroup") })
		if runner == nil {
			if strings.Contains(derived, "infermux-cpu") || !warned {
				t.Errorf("no runner: warned %v\n%s", warned, derived)
			}
			continue
		}
		if strings.Count(derived, "/bin/infermux-cpu run -socket /run/infermux-cpu/socket -- llama-server") != 1 || warned {
			t.Errorf("runner: warned %v\n%s", warned, derived)
		}
	}
}

// The warden counts a model on every device its place occupies, on the GPU
// NVML reads only, and the config says so about a model on another GPU.
func TestTheWardenSeesEachModelsDevices(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"infermux.yaml": []byte("hosts:\n  a:\n    url: http://a\n    devices:\n      cpu: {kind: cpu}\n      gpu: {kind: gpu}\n      second: {kind: gpu, index: 1}\nbalance:\n  m: [a/cpu]\n  n: [{place: a/gpu, occupies: [cpu]}]\n  o: [a/second]\n"),
		"models/m.yaml": []byte("cmd: llama-server --model /m.gguf --port ${PORT}\n"),
		"models/n.yaml": []byte("cmd: llama-server --model /n.gguf --port ${PORT}\n"),
		"models/o.yaml": []byte("cmd: llama-server --model /o.gguf --port ${PORT}\n"),
	}
	bundle, err := configstore.Sign(files, priv, "a", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := newConfigHosts(t, []ed25519.PublicKey{pub}, "a")["a"]
	h.one.options.CPURunner = []string{"/bin/infermux-cpu", "run", "-socket", "/s", "--"}
	if _, err := h.one.accept(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	got := devicesOf(h.one.current())
	want := map[string][]string{"m": {"cpu"}, "n": {"gpu", "cpu"}, "o": {}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for name, devices := range want {
		if !slices.Equal(got[name], devices) {
			t.Errorf("%s: %v, want %v", name, got[name], devices)
		}
	}
	var gpus []string
	for _, w := range h.one.state().Warnings {
		if strings.Contains(w, "CUDA index") {
			gpus = append(gpus, w)
		}
	}
	if len(gpus) != 1 || !strings.Contains(gpus[0], "o is on second, CUDA index 1, which the warden does not measure") {
		t.Fatalf("warnings %v", gpus)
	}
}
