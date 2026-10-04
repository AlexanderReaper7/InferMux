package muxui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/warden"
)

const base = `macros:
  llama-server: /bin/llama-server --host 127.0.0.1
healthCheckTimeout: 600
`

const qwen = `# Qwen, the everyday model.
models:
  qwen:
    cmd: |
      ${llama-server}
        --port ${PORT}
        --model GGUF
        --ctx-size 65536
        -fa on
        --jinja
        --temp -0.5
    proxy: http://127.0.0.1:${PORT}
    env:
      - CUDA_VISIBLE_DEVICES=0 # keep
`

type fixture struct {
	store *Store
	gguf  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	gguf := filepath.Join(dir, "gguf", "qwen.gguf")
	os.MkdirAll(filepath.Dir(gguf), 0o755)
	os.WriteFile(gguf, []byte("GGUF"), 0o644)
	other := filepath.Join(dir, "gguf", "other.gguf")
	os.WriteFile(other, []byte("GGUF"), 0o644)
	models := filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	os.WriteFile(filepath.Join(models, "qwen.yaml"), []byte(strings.ReplaceAll(qwen, "GGUF", gguf)), 0o644)
	os.WriteFile(filepath.Join(dir, "base.yaml"), []byte(base), 0o644)
	os.WriteFile(filepath.Join(dir, "warden.yaml"), []byte("# the warden\nhost: here\nkeys_file: keys.yaml\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "keys.yaml"), []byte("keys:\n  episteme-batch:\n    sha256: "+warden.HashKey("k")+"\n    class: batch\n"), 0o644)
	return fixture{
		store: &Store{ModelsDir: models, WardenFile: filepath.Join(dir, "warden.yaml"),
			BaseConfig: filepath.Join(dir, "base.yaml"), GGUFDirs: []string{filepath.Join(dir, "gguf")}},
		gguf: gguf,
	}
}

func (f fixture) model(t *testing.T, name string) Model {
	t.Helper()
	models, err := f.store.Models()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no model %s in %+v", name, models)
	return Model{}
}

func (f fixture) file(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.store.ModelsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func str(s string) *string { return &s }

func TestACmdInTheUIsFormSplitsIntoFlags(t *testing.T) {
	f := newFixture(t)
	m := f.model(t, "qwen")
	if m.Raw || m.Runtime != "llama-server" || m.GGUF != f.gguf || len(m.Flags) != 4 {
		t.Fatalf("%+v", m)
	}
	if m.Flags[2].Name != "--jinja" || m.Flags[2].Value != nil || *m.Flags[3].Value != "-0.5" {
		t.Fatalf("flags %+v", m.Flags)
	}
}

func TestEveryOtherCmdIsRawText(t *testing.T) {
	for _, cmd := range []string{
		"/bin/llama-server --port ${PORT} --model x.gguf",   // no runtime macro
		"${llama-server} --model x.gguf",                    // no port
		"${llama-server} --port ${PORT} --model x.gguf pos", // a positional
		"${llama-server} --port ${PORT}\n# note\n--model x.gguf",
	} {
		if _, _, _, ok := parseCmd(cmd); ok {
			t.Errorf("parsed %q", cmd)
		}
	}
}

func TestRenderingParsesBackToTheSameFlags(t *testing.T) {
	flags := []Flag{{"--ctx-size", str("262144")}, {"--jinja", nil}, {"--chat-template-kwargs", str(`{"enable_thinking": false}`)}, {"--temp", str("-1")}}
	runtime, gguf, got, ok := parseCmd(renderCmd("bonsai-server", "/srv/a b.gguf", flags))
	if !ok || runtime != "bonsai-server" || gguf != "/srv/a b.gguf" || len(got) != len(flags) {
		t.Fatalf("%v %s %s %+v", ok, runtime, gguf, got)
	}
	for i := range flags {
		if got[i].Name != flags[i].Name || (got[i].Value == nil) != (flags[i].Value == nil) ||
			(got[i].Value != nil && *got[i].Value != *flags[i].Value) {
			t.Errorf("flag %d: %+v != %+v", i, got[i], flags[i])
		}
	}
}

func TestAnEditKeepsCommentsAndKeysTheUIDoesNotEdit(t *testing.T) {
	f := newFixture(t)
	m := f.model(t, "qwen")
	m.Flags[0].Value = str("131072")
	ttl := 900
	m.TTL = &ttl
	if err := f.store.SaveModel("qwen", m); err != nil {
		t.Fatal(err)
	}
	out := f.file(t, "qwen.yaml")
	for _, want := range []string{"# Qwen, the everyday model.", "CUDA_VISIBLE_DEVICES=0", "# keep", "--ctx-size 131072", "ttl: 900"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q:\n%s", want, out)
		}
	}
}

func TestAnEditLlamaSwapWouldRefuseIsNotWritten(t *testing.T) {
	f := newFixture(t)
	before := f.file(t, "qwen.yaml")
	m := f.model(t, "qwen")
	m.Raw, m.Cmd = true, "${llama-server} --port ${PORT} --model x.gguf ${no-such-macro}"
	if err := f.store.SaveModel("qwen", m); err == nil {
		t.Fatal("an unknown macro was written")
	}
	if f.file(t, "qwen.yaml") != before {
		t.Fatal("the file changed")
	}
	m = f.model(t, "qwen")
	m.Flags = append(m.Flags, Flag{"--port", str("8080")})
	if err := f.store.SaveModel("qwen", m); err == nil {
		t.Fatal("--port as a flag was taken")
	}
}

func TestCreateRenameAndDelete(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(filepath.Dir(f.gguf), "other.gguf")
	m := Model{Name: "other", Runtime: "llama-server", GGUF: other, Flags: []Flag{{"--jinja", nil}}}
	if err := f.store.SaveModel("", m); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveModel("", m); err == nil {
		t.Fatal("created twice")
	}
	m = f.model(t, "other")
	m.Name = "renamed"
	if err := f.store.SaveModel("other", m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.store.ModelsDir, "other.yaml")); !os.IsNotExist(err) {
		t.Fatal("the old file is still there")
	}
	if !strings.Contains(f.file(t, "renamed.yaml"), "renamed:") {
		t.Fatal("not renamed")
	}
	if err := f.store.SaveModel("renamed", Model{Name: "qwen", Runtime: "llama-server", GGUF: other}); err == nil {
		t.Fatal("renamed onto an existing model")
	}
	if err := f.store.DeleteModel("renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.store.ModelsDir, "renamed.yaml")); !os.IsNotExist(err) {
		t.Fatal("an empty file was left")
	}
	ggufs, _ := f.store.GGUFs()
	if len(ggufs) != 2 || len(ggufs[1].UsedBy) != 1 || ggufs[1].UsedBy[0] != "qwen" {
		t.Fatalf("%+v", ggufs)
	}
}

func TestTheWardenFileKeepsItsCommentAndRefusesWhatTheWardenWould(t *testing.T) {
	f := newFixture(t)
	cfg, err := f.store.Warden()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Policy.GPUBusyPercent = 40
	if err := f.store.SaveWarden(cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.store.WardenFile)
	if !strings.Contains(string(raw), "# the warden") || !strings.Contains(string(raw), "gpu_busy_percent: 40") {
		t.Fatalf("%s", raw)
	}
	cfg.Consumers = append(cfg.Consumers, cfg.Consumers...)
	cfg.Consumers = append(cfg.Consumers, struct {
		Name           string  `yaml:"name" json:"name"`
		URL            string  `yaml:"url" json:"url"`
		AnnouncePath   string  `yaml:"announce_path" json:"announce_path"`
		TimeoutSeconds float64 `yaml:"timeout_seconds" json:"timeout_seconds"`
	}{Name: "no-url"})
	if err := f.store.SaveWarden(cfg); err == nil {
		t.Fatal("a consumer without a url was written")
	}
}

// 0005: the UI edits through the node tree, so comments and keys it does not
// know survive a save. A struct encode kept only the leading comment, and a
// save through the UI on 2026-10-04 wiped every comment in nixcfg's file.
func TestAWardenSaveKeepsEveryCommentAndUnknownKey(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(f.store.WardenFile, []byte(`# the warden

# ours, a line above
our_units:
  - llama-embed.service # the embedder
desktop_processes:
  - cosmic-comp
  # drawn by T3 Code
  - electron
host: here
keys_file: keys.yaml # hashes only
future_setting: kept
consumers:
  # the one that pauses
  - name: episteme
    url: http://127.0.0.1:8200 # loopback
  - name: gone
    url: http://127.0.0.1:8300
`), 0o644)
	cfg, err := f.store.Warden()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Policy.GPUBusyPercent = 40
	cfg.Consumers = cfg.Consumers[:1]
	cfg.DesktopProcesses = append(cfg.DesktopProcesses, "firefox")
	if err := f.store.SaveWarden(cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.store.WardenFile)
	out := string(raw)
	for _, want := range []string{
		"# the warden", "# ours, a line above", "# the embedder", "# drawn by T3 Code",
		"# hashes only", "# the one that pauses", "# loopback",
		"future_setting: kept", "gpu_busy_percent: 40", "- firefox",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gone") {
		t.Errorf("a removed consumer survived:\n%s", out)
	}
	if strings.Index(out, "our_units") > strings.Index(out, "host:") {
		t.Errorf("the file's key order changed:\n%s", out)
	}
	again, err := f.store.Warden()
	if err != nil || again.Policy.GPUBusyPercent != 40 || len(again.Consumers) != 1 || len(again.DesktopProcesses) != 3 {
		t.Fatalf("%v %+v", err, again)
	}
}

func TestACommitTakesInferMuxsFilesAndNothingElse(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	f := newFixture(t)
	repo := filepath.Dir(f.store.ModelsDir)
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-qm", "start")
	os.WriteFile(filepath.Join(repo, "base.yaml"), []byte(base+"# the user's own edit\n"), 0o644)
	git("add", "base.yaml") // staged by the user, not for the UI to commit

	m := f.model(t, "qwen")
	m.Description = "everyday"
	if err := f.store.SaveModel("qwen", m); err != nil {
		t.Fatal(err)
	}
	st, err := f.store.Git()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 1 || !strings.Contains(st.Diff, "+    description: everyday") {
		t.Fatalf("%+v", st)
	}
	os.Setenv("GIT_AUTHOR_NAME", "t")
	os.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	os.Setenv("GIT_COMMITTER_NAME", "t")
	os.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	if _, err := f.store.Commit("qwen: describe it", ""); err != nil {
		t.Fatal(err)
	}
	if staged := git("diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "base.yaml" {
		t.Fatalf("the user's staged file was touched: %q", staged)
	}
	if files := git("show", "--name-only", "--format="); strings.TrimSpace(files) != "models/qwen.yaml" {
		t.Fatalf("committed %q", files)
	}
}

// --- HTTP -------------------------------------------------------------------

func TestTheUIAnswersOnLoopbackAndTakesOnlyMarkedSameOriginWrites(t *testing.T) {
	f := newFixture(t)
	h := Handler(f.store, &Builder{}, &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, "", "")
	do := func(method, host string, header map[string]string) int {
		req := httptest.NewRequest(method, "/api/models/qwen", strings.NewReader("{}"))
		req.Host = host
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("GET", "evil.example:5010", nil); c != http.StatusMisdirectedRequest {
		t.Errorf("rebound name got %d", c)
	}
	if c := do("DELETE", "127.0.0.1:5010", map[string]string{"Origin": "https://evil.example", "X-InferMux": "1"}); c != http.StatusForbidden {
		t.Errorf("foreign origin got %d", c)
	}
	if c := do("DELETE", "127.0.0.1:5010", map[string]string{"Origin": "http://127.0.0.1:5010"}); c != http.StatusForbidden {
		t.Errorf("unmarked write got %d", c)
	}
	if c := do("DELETE", "127.0.0.1:5010", map[string]string{"Origin": "http://127.0.0.1:5010", "X-InferMux": "1"}); c != http.StatusOK {
		t.Errorf("own write got %d", c)
	}
}

func TestTheUIAnswersOnATrustedHostFromTheWardenFile(t *testing.T) {
	f := newFixture(t)
	h := Handler(f.store, &Builder{}, &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, "", "")
	get := func() int {
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = "box.tail.ts.net:5010"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := get(); c != http.StatusMisdirectedRequest {
		t.Fatalf("before trusting it: %d", c)
	}
	cfg, err := f.store.Warden()
	if err != nil {
		t.Fatal(err)
	}
	cfg.TrustedHosts = []string{"box.tail.ts.net"}
	if err := f.store.SaveWarden(cfg); err != nil {
		t.Fatal(err)
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("after trusting it: %d", c)
	}
}

func TestTheDaemonSeesInferMuxUIAndNotTheBrowser(t *testing.T) {
	var got *http.Request
	daemon := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		got = r
		json.NewEncoder(rw).Encode(map[string]bool{"ok": true})
	}))
	defer daemon.Close()
	u, _ := url.Parse(daemon.URL)
	h := Handler(newFixture(t).store, &Builder{}, u, "ui-key", "")
	req := httptest.NewRequest("POST", "/daemon/warden/forgive", nil)
	req.Host = "127.0.0.1:5010"
	req.Header.Set("Origin", "http://127.0.0.1:5010")
	req.Header.Set("X-InferMux", "1")
	req.Header.Set("Authorization", "Bearer the-browsers")
	req.Header.Set("X-Api-Key", "the-browsers")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || got.URL.Path != "/warden/forgive" || got.Header.Get("Origin") != "" || got.Header.Get("X-InferMux") == "" {
		t.Fatalf("%d %s %v", rec.Code, got.URL.Path, got.Header)
	}
	if got.Header.Get("Authorization") != "Bearer ui-key" || got.Header.Get("X-Api-Key") != "" {
		t.Fatalf("the daemon saw the key %q, %q, not the UI's", got.Header.Get("Authorization"), got.Header.Get("X-Api-Key"))
	}
}

// llama-swap's own UI on its listener: the daemon gets the UI's key and none
// of the browser's, and the browser gets the daemon's guard.
func TestTheLlamaSwapListenerAddsTheKeyAndKeepsTheGuard(t *testing.T) {
	var got *http.Request
	daemon := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		got = r
	}))
	defer daemon.Close()
	u, _ := url.Parse(daemon.URL)
	h := SwapHandler(newFixture(t).store, u, "ui-key")
	send := func(method, path, host, origin string, header map[string]string) int {
		got = nil
		req := httptest.NewRequest(method, path, nil)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := send("POST", "/api/models/unload", "127.0.0.1:5011", "http://127.0.0.1:5011", map[string]string{"Authorization": "Bearer the-browsers", "X-Api-Key": "the-browsers"}); c != 200 || got == nil {
		t.Fatalf("a same-origin write: %d", c)
	}
	if got.URL.Path != "/api/models/unload" || got.Header.Get("Authorization") != "Bearer ui-key" || got.Header.Get("X-Api-Key") != "" || got.Header.Get("Origin") != "" || got.Header.Get("X-InferMux") != "" {
		t.Fatalf("the daemon saw %s %v", got.URL.Path, got.Header)
	}
	if c := send("GET", "/ui/", "127.0.0.1:5011", "", nil); c != 200 || got == nil {
		t.Fatalf("the page: %d", c)
	}
	for name, c := range map[string]int{
		"another origin's write":     send("POST", "/v1/chat/completions", "127.0.0.1:5011", "https://evil.example", nil),
		"another origin's WebSocket": send("GET", "/v1/realtime", "127.0.0.1:5011", "https://evil.example", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}),
		"a rebound name":             send("GET", "/ui/", "evil.example:5011", "", nil),
	} {
		if c < 400 || got != nil {
			t.Errorf("%s: %d, reached the daemon %v", name, c, got != nil)
		}
	}
}

// --- the prebuild ------------------------------------------------------------

func TestOneBuildAtATimeWithItsLogAndPaths(t *testing.T) {
	release := make(chan struct{})
	prepared := 0
	var got []string
	b := &Builder{
		Installable: "flake#unit",
		Prepare:     func() error { prepared++; return nil },
		run: func(args []string, stdout, stderr io.Writer) error {
			got = args
			fmt.Fprintln(stderr, "building llama-cpp")
			<-release
			fmt.Fprintln(stdout, "/nix/store/x-unit")
			return nil
		},
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); !errors.Is(err, ErrBuildRunning) {
		t.Fatalf("second start: %v", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for b.State().Running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	st := b.State()
	if st.Running || st.OK == nil || !*st.OK || st.Output != "/nix/store/x-unit" || prepared != 1 {
		t.Fatalf("state %+v, prepared %d", st, prepared)
	}
	if len(st.Log) != 1 || st.Log[0] != "building llama-cpp" {
		t.Fatalf("log %q", st.Log)
	}
	if got[len(got)-1] != "flake#unit" {
		t.Fatalf("args %q", got)
	}
}

func TestAFailedPrepareIsAFailedBuild(t *testing.T) {
	ran := false
	b := &Builder{
		Installable: "flake#unit",
		Prepare:     func() error { return errors.New("git add: no") },
		run:         func([]string, io.Writer, io.Writer) error { ran = true; return nil },
	}
	b.Start()
	for b.State().Running {
		time.Sleep(time.Millisecond)
	}
	if st := b.State(); ran || st.OK == nil || *st.OK || st.Error != "git add: no" {
		t.Fatalf("ran %v, state %+v", ran, st)
	}
}

func TestIntentToAddMakesANewModelVisibleToGit(t *testing.T) {
	f := newFixture(t)
	repo := filepath.Dir(f.store.ModelsDir)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "start"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	m := f.model(t, "qwen")
	m.Name = "fresh"
	if err := f.store.SaveModel("", m); err != nil {
		t.Fatal(err)
	}
	if out, _ := f.store.git("ls-files", "--", f.store.ModelsDir); strings.Contains(out, "fresh") {
		t.Fatalf("tracked before: %q", out)
	}
	if err := f.store.IntentToAdd(); err != nil {
		t.Fatal(err)
	}
	if out, _ := f.store.git("ls-files", "--", f.store.ModelsDir); !strings.Contains(out, "fresh.yaml") {
		t.Fatalf("not tracked after: %q", out)
	}
	// Only the path is recorded: the content is still the user's to commit.
	if staged, _ := f.store.git("diff", "--cached", "--name-only"); strings.Contains(staged, "fresh") {
		t.Fatalf("content staged: %q", staged)
	}
}

func TestAModelSaveMakesTheLastBuildStale(t *testing.T) {
	f := newFixture(t)
	b := &Builder{Installable: "flake#unit", run: func([]string, io.Writer, io.Writer) error { return nil }}
	h := Handler(f.store, b, &url.URL{Scheme: "http", Host: "127.0.0.1:1"}, "", "")
	b.Start()
	for b.State().Running {
		time.Sleep(time.Millisecond)
	}
	if b.State().Stale {
		t.Fatal("stale before any change")
	}
	m := f.model(t, "qwen")
	m.Description = "changed"
	body, _ := json.Marshal(m)
	req := httptest.NewRequest("PUT", "/api/models/qwen", strings.NewReader(string(body)))
	req.Host = "127.0.0.1:5010"
	req.Header.Set("X-InferMux", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if !b.State().Stale {
		t.Fatal("not stale after a save")
	}
	b.Start()
	if b.State().Stale {
		t.Fatal("a new build is still stale")
	}
}
