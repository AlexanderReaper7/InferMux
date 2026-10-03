package warden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type nopLog struct{}

func (nopLog) Infof(string, ...any) {}
func (nopLog) Warnf(string, ...any) {}

type fakeModels struct {
	mu       sync.Mutex
	running  map[string]string
	unloads  int
	unloaded []string
}

func (m *fakeModels) Running() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.running {
		out[k] = v
	}
	return out
}

func (m *fakeModels) Qualify(r *http.Request) (string, bool) {
	var body struct{ Model string }
	if r.Body == nil || json.NewDecoder(r.Body).Decode(&body) != nil || body.Model == "" {
		return "", false
	}
	if strings.Contains(body.Model, "/") {
		return body.Model, true
	}
	return "this/" + body.Model, true
}

func (m *fakeModels) UnloadAll() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unloads++
	var ids []string
	for id := range m.running {
		ids = append(ids, id)
	}
	m.running = map[string]string{}
	m.unloaded = ids
	return ids
}

type harness struct {
	w        *Warden
	models   *fakeModels
	clock    time.Time
	reading  Resources
	probeErr error
	posts    []map[string]any
	postErr  error
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Keys = testKeys()
	cfg.Consumers = []Consumer{{Name: "episteme", URL: "http://episteme", AnnouncePath: "/announce", TimeoutSeconds: 1}}
	if mutate != nil {
		mutate(&cfg)
	}
	h := &harness{models: &fakeModels{running: map[string]string{"qwen": "ready"}}, clock: t0, reading: quiet()}
	h.w = New(cfg, h.models, nopLog{})
	h.w.now = func() time.Time { return h.clock }
	h.w.comfy = ComfyIdle{BusyAt: t0}
	h.w.probe = func() (Resources, error) { return h.reading, h.probeErr }
	h.w.announcer.now = h.w.now
	h.w.announcer.post = func(_ context.Context, _ string, payload any) (any, error) {
		if h.postErr != nil {
			return nil, h.postErr
		}
		h.posts = append(h.posts, payload.(map[string]any))
		return map[string]any{"applied": true}, nil
	}
	return h
}

func (h *harness) at(d time.Duration) *harness {
	h.clock = t0.Add(d)
	return h
}

// --- the tick ---------------------------------------------------------------

func TestATickMeasuresDecidesAndAnnounces(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60, "blender")
	v := h.w.Tick()
	if !v.Yielded || len(h.posts) != 1 || h.posts[0]["action"] != "pause" {
		t.Fatalf("verdict %+v posts %v", v, h.posts)
	}
}

func TestTheModelsAreUnloadedOnTheTransitionOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60)
	h.w.Tick()
	h.models.running = map[string]string{"qwen": "ready"} // a client loaded it again
	h.at(5 * time.Second).w.Tick()
	if h.models.unloads != 1 {
		t.Fatalf("unloaded %d times", h.models.unloads)
	}
}

func TestResumingUnloadsNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	if h.models.unloads != 0 {
		t.Fatal("unloaded without contention")
	}
}

func TestAFailedProbeLeavesTheVerdictAlone(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60)
	h.w.Tick()
	h.probeErr = errors.New("nvml gone")
	v := h.at(time.Hour).w.Tick()
	if !v.Yielded || h.w.State().ProbeError == nil {
		t.Fatalf("got %+v", v)
	}
}

// --- interactive priority (0004) ---------------------------------------------

func TestAnInteractiveRequestInFlightDefersTheUnload(t *testing.T) {
	h := newHarness(t, nil)
	id, _ := h.w.traffic.begin(Interactive, "/v1/responses", func() {})
	h.reading = busy(60)
	h.w.Tick()
	if h.models.unloads != 0 || !h.w.State().PendingUnload {
		t.Fatal("unloaded under an interactive request")
	}
	h.w.traffic.end(id)
	h.at(9 * time.Minute).w.Tick()
	if h.models.unloads != 0 {
		t.Fatal("unloaded within the recent window")
	}
	h.at(10 * time.Minute).w.Tick()
	if h.models.unloads != 1 || h.w.State().PendingUnload {
		t.Fatalf("owed unload not paid: %d", h.models.unloads)
	}
	h.at(11 * time.Minute).w.Tick()
	if h.models.unloads != 1 {
		t.Fatal("paid twice")
	}
}

func TestAResumeForgivesAnOwedUnload(t *testing.T) {
	h := newHarness(t, nil)
	h.w.traffic.begin(Interactive, "/v1/messages", func() {})
	h.reading = busy(60)
	h.w.Tick()
	h.reading = quiet()
	h.at(6 * time.Minute).w.Tick()
	if h.w.State().Verdict.Yielded || h.w.State().PendingUnload || h.models.unloads != 0 {
		t.Fatal("owed unload outlived the pause")
	}
}

func TestAYieldCancelsBatchButNotInteractive(t *testing.T) {
	h := newHarness(t, nil)
	var batchCancelled, interactiveCancelled bool
	h.w.traffic.begin(Batch, "/v1/chat/completions", func() { batchCancelled = true })
	h.w.traffic.begin(Interactive, "/v1/responses", func() { interactiveCancelled = true })
	h.reading = busy(60)
	h.w.Tick()
	if !batchCancelled || interactiveCancelled {
		t.Fatalf("batch %v interactive %v", batchCancelled, interactiveCancelled)
	}
}

// --- the handler ------------------------------------------------------------

func post(handler http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"qwen"}`))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// testKeys is the harness's keys.yaml: one batch key, one interactive.
func testKeys() *KeyFile {
	return &KeyFile{Keys: map[string]Key{
		"batch": {SHA256: HashKey("batch-key"), Class: Batch},
		"me":    {SHA256: HashKey("my-key"), Class: Interactive},
	}}
}

// wrap is the warden in front of next, for a test that is not about keys: a
// request that presents none is sent as the interactive key.
func (h *harness) wrap(next http.Handler) http.Handler {
	wrapped := h.w.Wrap(next)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if len(presentedKeys(r)) == 0 {
			r.Header.Set("Authorization", "Bearer my-key")
		}
		wrapped.ServeHTTP(rw, r)
	})
}

func TestTheKeyDecidesTheClass(t *testing.T) {
	tr := newTraffic(testKeys(), time.Now)
	cases := map[string]Class{
		"Bearer batch-key":            Batch,
		"Bearer my-key":               Interactive,
		"Basic " + "dTpiYXRjaC1rZXk=": Batch, // u:batch-key
	}
	for auth, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", auth)
		key, required, ok := tr.identify(req)
		if !required || !ok || key.Class != want {
			t.Errorf("%q: got %s %v %v, want %s", auth, key.Class, required, ok, want)
		}
	}
	for name, set := range map[string]func(*http.Request){
		"x-api-key": func(r *http.Request) { r.Header.Set("x-api-key", "batch-key") },
		"subprotocol": func(r *http.Request) {
			r.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.batch-key")
		},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
		set(req)
		if key, _, ok := tr.identify(req); !ok || key.Class != Batch {
			t.Errorf("%s not read", name)
		}
	}
	for _, auth := range []string{"", "Bearer other", "Bearer " + HashKey("my-key")} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", auth)
		if _, _, ok := tr.identify(req); ok {
			t.Errorf("%q was taken as a key", auth)
		}
	}
	if key, required, ok := newTraffic(nil, time.Now).identify(httptest.NewRequest(http.MethodPost, "/v1/x", nil)); required || !ok || key.Class != Interactive {
		t.Error("without a keys file, a request should be an unnamed interactive one")
	}
}

func TestBatchIsRefusedDuringAPauseAndInteractivePasses(t *testing.T) {
	h := newHarness(t, nil)
	served := 0
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { served++ }))
	h.reading = busy(60)
	h.w.Tick()

	rec := post(handler, "/v1/chat/completions", map[string]string{"Authorization": "Bearer batch-key"})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "300" {
		t.Fatalf("batch got %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := post(handler, "/v1/chat/completions", nil); rec.Code != http.StatusOK {
		t.Fatalf("interactive got %d", rec.Code)
	}
	if served != 1 {
		t.Fatalf("served %d", served)
	}
	// Management stays open to a batch client: it may want to unload.
	if rec := post(handler, "/api/models/unload", map[string]string{"Authorization": "Bearer batch-key"}); rec.Code != http.StatusOK {
		t.Fatalf("management got %d", rec.Code)
	}
}

func TestTheHandlerTracksARequestUntilItsResponseEnds(t *testing.T) {
	h := newHarness(t, nil)
	inside := make(chan struct{})
	release := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-release
	}))
	go post(handler, "/v1/responses", nil)
	<-inside
	if n := len(h.w.traffic.state().InFlight); n != 1 {
		t.Fatalf("in flight %d", n)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for len(h.w.traffic.state().InFlight) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never ended")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAYieldCancelsTheBatchRequestsContext(t *testing.T) {
	h := newHarness(t, nil)
	inside := make(chan struct{})
	ended := make(chan error, 1)
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-r.Context().Done()
		ended <- r.Context().Err()
	}))
	go post(handler, "/v1/chat/completions", map[string]string{"Authorization": "Bearer batch-key"})
	<-inside
	h.reading = busy(60)
	h.w.Tick()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("batch request still running after the yield")
	}
}

// Seen live 2026-10-03: llama-swap returns without writing when the context
// ends, and the cancelled request reached the client as an empty 200.
func TestACancelledBatchRequestIsA503NotAnEmpty200(t *testing.T) {
	h := newHarness(t, nil)
	inside := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-r.Context().Done()
	}))
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		done <- post(handler, "/v1/chat/completions", map[string]string{"Authorization": "Bearer batch-key"})
	}()
	<-inside
	h.reading = busy(60)
	h.w.Tick()
	rec := <-done
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestACancelledStreamBreaksTheConnection(t *testing.T) {
	h := newHarness(t, nil)
	inside := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte("data: {}\n\n"))
		rw.(http.Flusher).Flush()
		close(inside)
		<-r.Context().Done()
	}))
	panicked := make(chan any)
	go func() {
		defer func() { panicked <- recover() }()
		post(handler, "/v1/chat/completions", map[string]string{"Authorization": "Bearer batch-key"})
	}()
	<-inside
	h.reading = busy(60)
	h.w.Tick()
	if p := <-panicked; p != http.ErrAbortHandler {
		t.Fatalf("got %v", p)
	}
}

func TestAClientHangingUpIsNotReportedAsAYield(t *testing.T) {
	h := newHarness(t, nil)
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer batch-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestVerdictIsServedUnderWarden(t *testing.T) {
	h := newHarness(t, nil)
	handler := h.wrap(http.NotFoundHandler())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/warden/verdict", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action": "resume"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "batch-key") {
		t.Fatal("batch keys leaked into /warden/verdict")
	}
}

// --- ComfyUI ----------------------------------------------------------------

func TestComfyUIIsFreedAfterTheIdleWindowAndRetriedWhenItFails(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ComfyUIURL = "http://comfy" })
	jobs := 0
	h.w.comfyQueue = func() *int { return &jobs }
	frees, fail := 0, true
	h.w.comfyFree = func() error {
		frees++
		if fail {
			return errors.New("down")
		}
		return nil
	}
	h.at(10 * time.Minute).w.Tick()
	if frees != 1 || h.w.State().ComfyUI.Freed {
		t.Fatalf("frees %d", frees)
	}
	fail = false
	h.at(10*time.Minute + 5*time.Second).w.Tick()
	h.at(11 * time.Minute).w.Tick()
	if frees != 2 || !h.w.State().ComfyUI.Freed {
		t.Fatalf("frees %d", frees)
	}
}

func TestAQueuedComfyUIJobYieldsAndUnloads(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ComfyUIURL = "http://comfy" })
	h.w.comfyQueue = func() *int { return ip(1) }
	v := h.w.Tick()
	if !v.Yielded || h.models.unloads != 1 {
		t.Fatalf("verdict %+v unloads %d", v, h.models.unloads)
	}
}

// --- consumers --------------------------------------------------------------

func TestTheTickIsTheRetryAndTheStateIsRepeated(t *testing.T) {
	h := newHarness(t, nil)
	h.postErr = fmt.Errorf("connection refused")
	h.w.Tick()
	h.postErr = nil
	h.at(5 * time.Second).w.Tick()
	if len(h.posts) != 1 {
		t.Fatalf("posts %d", len(h.posts))
	}
	h.at(10 * time.Second).w.Tick()
	if len(h.posts) != 1 {
		t.Fatal("re-announced before the interval")
	}
	h.at(5*time.Second + reannounceInterval).w.Tick()
	if len(h.posts) != 2 {
		t.Fatalf("not repeated: %d", len(h.posts))
	}
}

func TestAPauseCarriesItsStartTime(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60)
	h.w.Tick()
	h.at(reannounceInterval).w.Tick()
	if len(h.posts) != 2 || *h.posts[0]["since"].(*string) != *h.posts[1]["since"].(*string) {
		t.Fatalf("since moved: %v", h.posts)
	}
}
