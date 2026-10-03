package warden

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// The gate in front of every route (0006, 4), sent with the headers given
// and nothing added.
func gated(h *harness, method, path, body string, header map[string]string) (*httptest.ResponseRecorder, *int) {
	served := 0
	handler := h.w.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { served++ }))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, &served
}

func TestEveryRouteButTheHealthChecksNeedsAKey(t *testing.T) {
	h := newHarness(t, nil)
	for _, r := range []struct{ method, path string }{
		{"POST", "/v1/chat/completions"}, {"GET", "/v1/models"}, {"GET", "/ui/"},
		{"GET", "/running"}, {"GET", "/warden/verdict"}, {"POST", "/api/models/unload"},
		{"GET", "/upstream/qwen/health"},
	} {
		for name, header := range map[string]map[string]string{
			"no key":       {},
			"unknown key":  {"Authorization": "Bearer nope"},
			"the hash":     {"Authorization": "Bearer " + HashKey("my-key")},
			"empty bearer": {"Authorization": "Bearer "},
		} {
			rec, served := gated(h, r.method, r.path, `{"model":"qwen"}`, header)
			if rec.Code != http.StatusUnauthorized || *served != 0 {
				t.Errorf("%s %s, %s: %d, served %d", r.method, r.path, name, rec.Code, *served)
			}
			if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
				t.Errorf("%s %s: no Basic challenge, so a browser would not ask", r.method, r.path)
			}
		}
	}
	for _, path := range []string{"/health", "/wol-health", "/favicon.ico", "/ui/site.webmanifest", "/ui/web-app-manifest-512x512.png"} {
		if rec, served := gated(h, "GET", path, "", nil); *served != 1 {
			t.Errorf("%s needs no key: %d", path, rec.Code)
		}
	}
	if rec, served := gated(h, "OPTIONS", "/v1/chat/completions", "", nil); *served != 1 {
		t.Errorf("a CORS preflight carries no key: %d", rec.Code)
	}
	if rec, served := gated(h, "GET", "/warden/verdict", "", map[string]string{"Authorization": "Basic dTpteS1rZXk="}); rec.Code != 200 || *served != 0 {
		t.Errorf("the key as Basic's password (a browser's login): %d", rec.Code)
	}
}

func TestWithoutAKeysFileNoKeyIsNeeded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Keys = nil })
	if rec, served := gated(h, "POST", "/v1/chat/completions", `{"model":"qwen"}`, nil); *served != 1 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAKeyMayUseOnlyWhatItsAllowListMatches(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Keys.Keys["narrow"] = Key{SHA256: HashKey("narrow-key"), Class: Interactive, Allow: []string{"this/*", "openrouter/cheap-*"}}
		c.Keys.Keys["none"] = Key{SHA256: HashKey("none-key"), Class: Interactive, Allow: []string{}}
	})
	auth := map[string]string{"Authorization": "Bearer narrow-key"}
	for body, want := range map[string]int{
		`{"model":"qwen"}`:                   1,
		`{"model":"openrouter/cheap-model"}`: 1,
		`{"model":"openrouter/dear-model"}`:  0,
		`{"model":"zbox/embed"}`:             0,
		`{}`:                                 0, // names no model: refused, not waved through
	} {
		rec, served := gated(h, "POST", "/v1/chat/completions", body, auth)
		if *served != want {
			t.Errorf("%s: served %d want %d (%d)", body, *served, want, rec.Code)
		}
		if want == 0 && rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, not 403", body, rec.Code)
		}
	}
	if _, served := gated(h, "POST", "/v1/chat/completions", `{"model":"qwen"}`, map[string]string{"Authorization": "Bearer none-key"}); *served != 0 {
		t.Error("allow: [] let a request through")
	}
	if _, served := gated(h, "POST", "/v1/chat/completions", `{"model":"zbox/embed"}`, map[string]string{"Authorization": "Bearer my-key"}); *served != 1 {
		t.Error("a key without allow was limited")
	}
	if _, served := gated(h, "GET", "/v1/models", "", auth); *served != 1 {
		t.Error("listing the models is not using one")
	}
}

func TestAWebSocketIsInferenceAndCrossOriginOnesAreRefused(t *testing.T) {
	h := newHarness(t, nil)
	ws := map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Protocol": "realtime, openai-insecure-api-key.batch-key"}

	inFlight := 0
	handler := h.w.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		inFlight = len(h.w.traffic.state().InFlight)
	}))
	req := httptest.NewRequest("GET", "/v1/realtime?model=qwen", nil)
	for k, v := range ws {
		req.Header.Set(k, v)
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if inFlight != 1 {
		t.Fatal("a WebSocket session was not tracked as a request in flight")
	}

	h.w.traffic.pause()
	if rec, served := gated(h, "GET", "/v1/realtime?model=qwen", "", ws); *served != 0 || rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a batch WebSocket while paused: %d", rec.Code)
	}
	h.w.traffic.resume()

	crossOrigin := map[string]string{"Origin": "https://evil.example", "Authorization": "Bearer my-key"}
	for k, v := range ws {
		crossOrigin[k] = v
	}
	if rec, served := gated(h, "GET", "/v1/realtime?model=qwen", "", crossOrigin); *served != 0 || rec.Code != http.StatusForbidden {
		t.Errorf("a WebSocket from another origin: %d", rec.Code)
	}
}

func TestAKeySeesOnlyTheModelsItMayUse(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Keys.Keys["narrow"] = Key{SHA256: HashKey("narrow-key"), Class: Interactive, Allow: []string{"this/*", "openrouter/cheap-*"}}
		c.Keys.Keys["none"] = Key{SHA256: HashKey("none-key"), Class: Interactive, Allow: []string{}}
	})
	list := `{"object":"list","data":[{"id":"qwen"},{"id":"openrouter/cheap-model"},{"id":"openrouter/dear-model"},{"id":"zbox/embed"},{"id":"nobody-knows"}]}`
	handler := h.w.Wrap(h.w.FilterModels(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Length", "999")
		rw.Write([]byte(list))
	})))
	listed := func(key string) []string {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var got struct {
			Object string
			Data   []struct{ ID string }
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Object != "list" {
			t.Fatalf("%s: %d %q", key, rec.Code, rec.Body)
		}
		ids := []string{}
		for _, m := range got.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}
	if got := listed("narrow-key"); !slices.Equal(got, []string{"qwen", "openrouter/cheap-model"}) {
		t.Errorf("narrow sees %v", got)
	}
	if got := listed("none-key"); len(got) != 0 {
		t.Errorf("allow: [] sees %v", got)
	}
	if got := listed("my-key"); len(got) != 5 {
		t.Errorf("a key without allow sees %v, not the whole list", got)
	}
}
