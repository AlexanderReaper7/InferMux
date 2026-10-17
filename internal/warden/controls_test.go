package warden

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- the manual verdict -----------------------------------------------------

func TestAManualPauseHoldsUntilTheWardensOwnDecisionChanges(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	if _, err := h.w.SetManual("pause"); err != nil {
		t.Fatal(err)
	}
	if v := h.w.State().Verdict; !v.Yielded || v.Reason != "paused by hand" {
		t.Fatalf("verdict %+v", v)
	}
	if len(h.posts) != 2 || h.posts[1]["action"] != "pause" || h.models.unloads != 1 {
		t.Fatalf("a manual pause is not a yield: posts %v unloads %d", h.posts, h.models.unloads)
	}
	h.at(time.Hour).w.Tick()
	if !h.w.State().Verdict.Yielded {
		t.Fatal("a quiet card ended the manual pause")
	}
	// The warden's own decision moves to pause: the hold ends, and the
	// verdict is now the warden's, still a pause.
	h.reading = busy(60, "game")
	h.at(time.Hour + 5*time.Second).w.Tick()
	s := h.w.State()
	if s.Manual != nil || !s.Verdict.Yielded || !strings.Contains(s.Verdict.Reason, "game") {
		t.Fatalf("state %+v", s)
	}
	h.reading = quiet()
	h.at(2 * time.Hour).w.Tick()
	if h.w.State().Verdict.Yielded {
		t.Fatal("the manual pause outlived the transition")
	}
}

func TestAManualResumeLastsUntilTheContentionEnds(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60, "game")
	h.w.Tick()
	if _, err := h.w.SetManual("resume"); err != nil {
		t.Fatal(err)
	}
	if h.w.State().Verdict.Yielded || h.posts[len(h.posts)-1]["action"] != "resume" {
		t.Fatal("manual resume not applied and announced")
	}
	h.at(time.Minute).w.Tick()
	if h.w.State().Verdict.Yielded {
		t.Fatal("the game still running ended the manual resume")
	}
	h.reading = quiet()
	h.at(10 * time.Minute).w.Tick()
	if s := h.w.State(); s.Manual != nil || s.Verdict.Yielded {
		t.Fatalf("state %+v", s)
	}
	// A new game is contention again, on the warden's own terms.
	h.reading = busy(60, "game")
	h.at(11 * time.Minute).w.Tick()
	if !h.w.State().Verdict.Yielded {
		t.Fatal("contention after the hold ended did not yield")
	}
}

func TestAutoAndAManualVerdictEqualToTheOwnClearTheHold(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	h.w.SetManual("resume")
	if h.w.State().Manual != nil {
		t.Fatal("a manual resume on a quiet card holds")
	}
	h.w.SetManual("pause")
	h.w.SetManual("auto")
	if s := h.w.State(); s.Manual != nil || s.Verdict.Yielded {
		t.Fatalf("auto left %+v", s)
	}
	if _, err := h.w.SetManual("later"); err == nil {
		t.Fatal("an unknown action was taken")
	}
}

// --- actions ----------------------------------------------------------------

// An unload by hand, and so infermux-hold, is never refused and never waits
// (the user, 2026-10-17; 0024, 4): the user's own request on the GPU is cut
// and answered 503 gpu_held with the GPU's window as Retry-After, and the
// models stop under it.
func TestAnUnloadByHandCutsTheUsersRequestOnTheGPU(t *testing.T) {
	h := newHarness(t, nil)
	inside := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-r.Context().Done()
	}))
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- post(handler, "/v1/chat/completions", nil) }()
	<-inside
	unloaded := h.w.UnloadNow()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the request was not cut; unloaded %v", unloaded)
	}
	if !slices.Equal(unloaded, []string{"qwen"}) || h.models.Running()["qwen"] != "" {
		t.Fatalf("unloaded %v, running %v", unloaded, h.models.Running())
	}
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"type":"gpu_held"`) || rec.Header().Get("Retry-After") != "300" {
		t.Fatalf("%d, Retry-After %q: %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

// The CPU holds no VRAM, so an interactive request in flight there keeps
// the CPU's models through an unload by hand, and the GPU's stop all the
// same (0004).
func TestAnUnloadByHandLeavesTheCPUUnderTheUsersRequest(t *testing.T) {
	h := newHarness(t, nil)
	h.models.running = map[string]string{"qwen": "ready", "embed": "ready"}
	h.w.SetPlacement(Placement{"qwen": {Devices: []string{DeviceGPU}}, "embed": {Devices: []string{DeviceCPU}}})
	cut := false
	id, _ := h.w.traffic.begin(Interactive, "/v1/embeddings", []string{DeviceCPU}, func(string, string) { cut = true }, nil)
	if unloaded := h.w.UnloadNow(); !slices.Equal(unloaded, []string{"qwen"}) || cut {
		t.Fatalf("unloaded %v, the CPU's request cut %v", unloaded, cut)
	}
	h.w.traffic.end(id)
	if unloaded := h.w.UnloadNow(); !slices.Equal(unloaded, []string{"embed"}) {
		t.Fatalf("unloaded %v", unloaded)
	}
}

func TestForgiveDropsAnOwedUnload(t *testing.T) {
	h := newHarness(t, nil)
	h.w.traffic.begin(Interactive, "/v1/messages", gpu, func(string, string) {}, nil)
	h.reading = busy(60)
	h.w.Tick()
	if !h.w.Forgive() || h.w.State().PendingUnload {
		t.Fatal("not forgiven")
	}
}

// --- reloads ----------------------------------------------------------------

func TestAReloadKeepsTheVerdictAndWhatConsumersHeard(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60)
	h.w.Tick()
	cfg := h.w.config()
	cfg.Keys = &KeyFile{Keys: map[string]Key{"new": {SHA256: HashKey("new-key"), Class: Batch}}}
	cfg.Policy.GPUBusyPercent = 90
	h.w.Reload(cfg)
	if !h.w.State().Verdict.Yielded {
		t.Fatal("a reload moved the verdict")
	}
	h.at(5 * time.Second).w.Tick()
	if len(h.posts) != 1 {
		t.Fatalf("an unchanged consumer was told again: %v", h.posts)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer new-key")
	if key, _, _ := h.w.traffic.identify(req); key.Class != Batch {
		t.Fatal("the new batch key is not read")
	}
	h.reading = busy(60)
	h.at(10 * time.Minute).w.Tick()
	if h.w.State().Verdict.Yielded {
		t.Fatal("the new threshold is not used")
	}
}

func TestDisablingTheWardenLiftsAnAnnouncedPause(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60)
	h.w.Tick()
	cfg := h.w.config()
	cfg.Policy.Enabled = false
	h.w.Reload(cfg)
	h.at(5 * time.Second).w.Tick()
	if h.w.State().Verdict.Yielded || h.posts[len(h.posts)-1]["action"] != "resume" {
		t.Fatalf("posts %v", h.posts)
	}
	h.at(time.Hour).w.Tick()
	if len(h.posts) != 2 {
		t.Fatalf("a disabled warden kept announcing: %v", h.posts)
	}
}

func TestADeferredReloadWaitsForTheLastInteractiveRequest(t *testing.T) {
	h := newHarness(t, nil)
	ran := 0
	inside := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-release
	}))
	go func() { post(handler, "/v1/messages", nil); close(done) }()
	<-inside
	h.w.WhenNoInteractive("llama-swap config", func() { ran++ })
	h.w.Tick()
	if ran != 0 || len(h.w.State().WaitingQuiet) != 1 {
		t.Fatal("reloaded under an interactive request")
	}
	close(release)
	<-done
	if ran != 1 || len(h.w.State().WaitingQuiet) != 0 {
		t.Fatalf("ran %d after the request ended", ran)
	}
	h.w.WhenNoInteractive("llama-swap config", func() { ran++ })
	if ran != 2 {
		t.Fatal("a quiet warden did not run it at once")
	}
}

// --- the guard --------------------------------------------------------------

func TestWritesFromAnotherOriginAndUnmarkedControlsAreRefused(t *testing.T) {
	h := newHarness(t, nil)
	served := 0
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { served++ }))
	cases := []struct {
		path   string
		header map[string]string
		want   int
	}{
		{"/api/models/unload", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"/v1/chat/completions", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"/v1/chat/completions", map[string]string{"Origin": "http://example.com"}, http.StatusForbidden}, // same origin, rebound DNS
		{"/v1/chat/completions", map[string]string{"Host": "127.0.0.1:5001", "Origin": "http://127.0.0.1:5001"}, http.StatusOK},
		{"/v1/chat/completions", nil, http.StatusOK},
		{"/warden/forgive", nil, http.StatusForbidden},
		{"/warden/forgive", map[string]string{"X-InferMux": "1"}, http.StatusOK},
	}
	for _, c := range cases {
		if rec := postHost(handler, c.path, c.header); rec.Code != c.want {
			t.Errorf("%s %v: got %d want %d", c.path, c.header, rec.Code, c.want)
		}
	}
	if served != 2 {
		t.Fatalf("served %d", served)
	}
}

func TestATrustedHostTakesBrowserWritesAfterAReload(t *testing.T) {
	h := newHarness(t, nil)
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {}))
	ts := map[string]string{"Host": "box.tail.ts.net:5001", "Origin": "https://box.tail.ts.net:5001"}
	if rec := postHost(handler, "/v1/chat/completions", ts); rec.Code != http.StatusForbidden {
		t.Fatalf("untrusted tailnet name got %d", rec.Code)
	}
	cfg := h.w.config()
	cfg.TrustedHosts = []string{"Box.tail.ts.net."}
	h.w.Reload(cfg)
	if rec := postHost(handler, "/v1/chat/completions", ts); rec.Code != http.StatusOK {
		t.Fatalf("trusted tailnet name got %d", rec.Code)
	}
	other := map[string]string{"Host": "box.tail.ts.net:5001", "Origin": "https://evil.example"}
	if rec := postHost(handler, "/v1/chat/completions", other); rec.Code != http.StatusForbidden {
		t.Fatalf("another origin on the trusted name got %d", rec.Code)
	}
	if rec := postHost(handler, "/v1/chat/completions", map[string]string{"Host": "evil.ts.net", "Origin": "https://evil.ts.net"}); rec.Code != http.StatusForbidden {
		t.Fatalf("another tailnet name got %d", rec.Code)
	}
}

// postHost is post, with a Host header setting the request's host.
func postHost(handler http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"qwen"}`))
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAPolicyNumberNoOneCouldMeanIsRefused(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) error {
		path := filepath.Join(dir, "w.yaml")
		os.WriteFile(path, []byte(body), 0o644)
		_, err := LoadConfig(path)
		return err
	}
	for _, bad := range []string{
		"gpu_busy_percent: -1", "gpu_busy_percent: 100.5", "poll_seconds: 0", "poll_seconds: 0.5",
		"min_free_vram_mb: -1", "resume_quiet_seconds: -1", "comfyui_idle_seconds: -1", "interactive_recent_seconds: -1",
	} {
		if err := load("policy:\n  " + bad + "\n"); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
	for _, edge := range []string{
		"gpu_busy_percent: 0", "gpu_busy_percent: 100", "poll_seconds: 1", "min_free_vram_mb: 0",
		"resume_quiet_seconds: 0", "interactive_recent_seconds: 0",
	} {
		if err := load("policy:\n  " + edge + "\n"); err != nil {
			t.Errorf("%s was refused: %v", edge, err)
		}
	}
}
