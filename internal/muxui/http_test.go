package muxui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every route of infermux-ui, through the real handler and guard, as the web
// app calls it: its success and the refusal it owes.

type ui struct {
	t *testing.T
	f fixture
	h http.Handler
}

func newUI(t *testing.T) ui {
	f := newFixture(t)
	return ui{t: t, f: f, h: Handler(f.store, &Builder{}, &url.URL{Scheme: "http", Host: "127.0.0.1:5001"}, "")}
}

// call sends what the web app sends: a loopback Host, its own Origin on a
// write, and X-InferMux.
func (u ui) call(method, path string, body any) (int, map[string]any) {
	u.t.Helper()
	var reader *strings.Reader
	switch b := body.(type) {
	case nil:
		reader = strings.NewReader("")
	case string:
		reader = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "127.0.0.1:5010"
	if method != http.MethodGet {
		req.Header.Set("Origin", "http://127.0.0.1:5010")
	}
	req.Header.Set("X-InferMux", "test")
	rec := httptest.NewRecorder()
	u.h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (u ui) want(code, want int, out map[string]any, what string) {
	u.t.Helper()
	if code != want {
		u.t.Fatalf("%s: got %d want %d: %v", what, code, want, out)
	}
}

func TestStateCarriesWhatTheAppReads(t *testing.T) {
	u := newUI(t)
	u.f.store.KVKernels = map[string][]string{"llama-server": {"f16-f16"}}
	code, out := u.call("GET", "/api/state", nil)
	u.want(code, 200, out, "state")
	for _, key := range []string{"models", "runtimes", "warden", "kv_kernels", "daemon_port", "paths"} {
		if _, ok := out[key]; !ok {
			t.Errorf("no %s in %v", key, out)
		}
	}
	if out["daemon_port"] != "5001" {
		t.Errorf("daemon_port %v", out["daemon_port"])
	}
	if rt := out["runtimes"].(map[string]any); rt["llama-server"] == nil {
		t.Errorf("runtimes %v", rt)
	}
}

func TestStateFailsLoudlyOnABrokenFile(t *testing.T) {
	u := newUI(t)
	os.WriteFile(filepath.Join(u.f.store.ModelsDir, "broken.yaml"), []byte("models: [\n"), 0o644)
	code, out := u.call("GET", "/api/state", nil)
	u.want(code, http.StatusUnprocessableEntity, out, "broken model file")
	if !strings.Contains(out["detail"].(string), "broken.yaml") {
		t.Errorf("detail does not name the file: %v", out)
	}
}

func TestModelRoutes(t *testing.T) {
	u := newUI(t)
	m := u.f.model(t, "qwen")

	fresh := m
	fresh.Name, fresh.File, fresh.Comment = "fresh", "", ""
	code, out := u.call("POST", "/api/models", fresh)
	u.want(code, 200, out, "create")
	if _, err := os.Stat(filepath.Join(u.f.store.ModelsDir, "fresh.yaml")); err != nil {
		t.Fatalf("no file: %v", err)
	}
	code, out = u.call("POST", "/api/models", fresh)
	u.want(code, http.StatusUnprocessableEntity, out, "create a name that exists")

	bad := fresh
	bad.Name = "has space"
	code, out = u.call("POST", "/api/models", bad)
	u.want(code, http.StatusUnprocessableEntity, out, "a bad name")

	code, out = u.call("POST", "/api/models", `{"name":"x","bogus":1}`)
	u.want(code, http.StatusBadRequest, out, "an unknown field")
	code, out = u.call("POST", "/api/models", `{`)
	u.want(code, http.StatusBadRequest, out, "broken JSON")

	missing := fresh
	missing.Name, missing.GGUF = "missing", "/nowhere/x.gguf"
	code, out = u.call("POST", "/api/models", missing)
	u.want(code, http.StatusUnprocessableEntity, out, "a GGUF that does not exist")

	relative := fresh
	relative.Name, relative.GGUF = "relative", "x.gguf"
	code, out = u.call("POST", "/api/models", relative)
	u.want(code, http.StatusUnprocessableEntity, out, "a relative GGUF")

	renamed := u.f.model(t, "fresh")
	renamed.Name = "renamed"
	code, out = u.call("PUT", "/api/models/fresh", renamed)
	u.want(code, 200, out, "rename")
	if _, err := os.Stat(filepath.Join(u.f.store.ModelsDir, "renamed.yaml")); err != nil {
		t.Fatalf("the file did not follow the name: %v", err)
	}
	gone := renamed
	gone.Name = "unused"
	code, out = u.call("PUT", "/api/models/fresh", gone)
	u.want(code, http.StatusNotFound, out, "save under the old name")

	clash := u.f.model(t, "renamed")
	clash.Name = "qwen"
	code, out = u.call("PUT", "/api/models/renamed", clash)
	u.want(code, http.StatusUnprocessableEntity, out, "rename onto another model")

	code, out = u.call("DELETE", "/api/models/renamed", nil)
	u.want(code, 200, out, "delete")
	code, out = u.call("DELETE", "/api/models/renamed", nil)
	u.want(code, http.StatusNotFound, out, "delete twice")
	if _, err := os.Stat(filepath.Join(u.f.store.ModelsDir, "renamed.yaml")); !os.IsNotExist(err) {
		t.Fatalf("the emptied file is still there: %v", err)
	}
}

func TestAFlagLlamaSwapWouldReadDifferentlyIsRefused(t *testing.T) {
	u := newUI(t)
	m := u.f.model(t, "qwen")
	before := u.f.file(t, "qwen.yaml")
	for _, fl := range []Flag{
		{Name: "--chat-template", Value: str("line\n# a comment to llama-swap")},
		{Name: `--x\`, Value: nil},
		{Name: "--port", Value: str("1")},
		{Name: "--temp", Value: str("--oops")},
	} {
		edited := m
		edited.Flags = append(append([]Flag{}, m.Flags...), fl)
		code, out := u.call("PUT", "/api/models/qwen", edited)
		u.want(code, http.StatusUnprocessableEntity, out, "flag "+fl.Name)
	}
	if after := u.f.file(t, "qwen.yaml"); after != before {
		t.Fatalf("a refused save changed the file:\n%s", after)
	}
}

func TestGGUFAndWardenRoutes(t *testing.T) {
	u := newUI(t)
	req := httptest.NewRequest("GET", "/api/gguf", nil)
	req.Host = "127.0.0.1:5010"
	rec := httptest.NewRecorder()
	u.h.ServeHTTP(rec, req)
	var files []GGUF
	if err := json.Unmarshal(rec.Body.Bytes(), &files); rec.Code != 200 || err != nil || len(files) != 2 {
		t.Fatalf("gguf: %d %v %s", rec.Code, err, rec.Body)
	}

	_, state := u.call("GET", "/api/state", nil)
	cfg := state["warden"].(map[string]any)
	cfg["trusted_hosts"] = []string{"box.ts.net"}
	code, out := u.call("PUT", "/api/warden", cfg)
	u.want(code, 200, out, "save the warden")
	if raw, _ := os.ReadFile(u.f.store.WardenFile); !strings.Contains(string(raw), "box.ts.net") || !strings.HasPrefix(string(raw), "# the warden") {
		t.Fatalf("warden file:\n%s", raw)
	}

	policy := cfg["policy"].(map[string]any)
	policy["poll_seconds"] = 0
	code, out = u.call("PUT", "/api/warden", cfg)
	u.want(code, http.StatusUnprocessableEntity, out, "a policy the warden refuses")
	code, out = u.call("PUT", "/api/warden", `{"nonsense":true}`)
	u.want(code, http.StatusBadRequest, out, "an unknown key")
}

func TestGitRoutesOutsideARepositoryFail(t *testing.T) {
	u := newUI(t)
	code, out := u.call("GET", "/api/git", nil)
	u.want(code, http.StatusUnprocessableEntity, out, "git outside a repository")
	code, out = u.call("POST", "/api/git/commit", map[string]string{"message": " "})
	u.want(code, http.StatusUnprocessableEntity, out, "an empty message")
}

func TestBuildRoutes(t *testing.T) {
	u := newUI(t)
	code, out := u.call("GET", "/api/build", nil)
	u.want(code, 200, out, "build state")
	if out["configured"] != false {
		t.Fatalf("configured without an installable: %v", out)
	}
	code, out = u.call("POST", "/api/build", nil)
	u.want(code, http.StatusConflict, out, "a build with nothing configured")
}

// Every write route, sent without the header or from another origin.
func TestEveryWriteRouteIsGuarded(t *testing.T) {
	u := newUI(t)
	before := u.f.file(t, "qwen.yaml")
	routes := []struct{ method, path string }{
		{"POST", "/api/models"}, {"PUT", "/api/models/qwen"}, {"DELETE", "/api/models/qwen"},
		{"PUT", "/api/warden"}, {"POST", "/api/git/commit"}, {"POST", "/api/build"},
		{"POST", "/daemon/warden/manual"},
	}
	for _, r := range routes {
		for name, header := range map[string]map[string]string{
			"no header":            {"Origin": "http://127.0.0.1:5010"},
			"other origin":         {"Origin": "https://evil.example", "X-InferMux": "1"},
			"null origin":          {"Origin": "null", "X-InferMux": "1"},
			"no header, no origin": {},
		} {
			req := httptest.NewRequest(r.method, r.path, strings.NewReader("{}"))
			req.Host = "127.0.0.1:5010"
			for k, v := range header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			u.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s, %s: %d", r.method, r.path, name, rec.Code)
			}
		}
	}
	if after := u.f.file(t, "qwen.yaml"); after != before {
		t.Fatal("a refused write changed the file")
	}
}

func TestOtherMethodsOnTheAppAreNotFound(t *testing.T) {
	u := newUI(t)
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		code, _ := u.call(method, "/index.html", nil)
		if code != http.StatusNotFound {
			t.Errorf("%s /index.html: %d", method, code)
		}
	}
}
