package muxui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	return ui{t: t, f: f, h: Handler(f.store, &Builder{}, &url.URL{Scheme: "http", Host: "127.0.0.1:5001"}, "", "")}
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
	for _, key := range []string{"runtimes", "warden", "kv_kernels", "daemon_port", "swap_port", "paths"} {
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
	os.WriteFile(u.f.store.BaseConfig, []byte("macros: [\n"), 0o644)
	code, out := u.call("GET", "/api/state", nil)
	u.want(code, http.StatusUnprocessableEntity, out, "a broken base config")
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

	// The one config replaced remotes, and the daemon refuses them (0022).
	policy["poll_seconds"] = 2
	cfg["remotes"] = []map[string]string{{"name": "zbox", "url": "https://zbox.ts.net:5001", "key_file": "/k"}}
	code, out = u.call("PUT", "/api/warden", cfg)
	u.want(code, http.StatusUnprocessableEntity, out, "remotes under the one config")
	if !strings.Contains(out["detail"].(string), "remotes come from the config's hosts") {
		t.Errorf("%v", out)
	}
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
	before := mustRead(t, u.f.store.WardenFile)
	routes := []struct{ method, path string }{
		{"PUT", "/api/warden"}, {"POST", "/api/git/commit"}, {"POST", "/api/build"},
		{"POST", "/api/config/publish"}, {"POST", "/api/config/copy"},
		{"POST", "/api/keys/phone"}, {"DELETE", "/api/keys/episteme-batch"},
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
	if after := mustRead(t, u.f.store.WardenFile); string(after) != string(before) {
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
