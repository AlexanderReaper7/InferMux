package warden

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A safety stop (0025): the local models off, the user's own request with
// them, and everything else through.

func TestOffCancelsTheUsersOwnRequestAndStopsTheModelsAtOnce(t *testing.T) {
	h := newHarness(t, nil)
	inside := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-r.Context().Done()
	}))
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- post(handler, "/v1/chat/completions", nil) }()
	<-inside

	h.w.SetOff("too hot: GPU 91 C")
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(time.Second):
		t.Fatal("the interactive request still runs with the models off")
	}
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"off"`) || !strings.Contains(rec.Body.String(), "too hot: GPU 91 C") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// No interactive quiet is waited for: the request ended a moment ago.
	if h.models.unloads != 1 || len(h.models.Running()) != 0 {
		t.Fatalf("unloads %d, running %v", h.models.unloads, h.models.Running())
	}
}

func TestOffRefusesEveryLocalRequestAndPassesTheRest(t *testing.T) {
	h := newHarness(t, nil)
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {}))
	h.w.SetOff("quiet hours")
	for _, key := range []string{"my-key", "batch-key"} {
		if rec := send(handler, "qwen", key); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "300" {
			t.Fatalf("%s: %d, Retry-After %q, %s", key, rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
		}
		for _, model := range []string{"openrouter/free", "strix/qwen"} {
			if rec := send(handler, model, key); rec.Code != http.StatusOK {
				t.Fatalf("%s for %s: %d %s", key, model, rec.Code, rec.Body.String())
			}
		}
	}

	h.w.SetOff("")
	if rec := send(handler, "qwen", "my-key"); rec.Code != http.StatusOK {
		t.Fatalf("on again: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(handler, "qwen", "batch-key"); rec.Code != http.StatusOK {
		t.Fatalf("on again, batch: %d %s", rec.Code, rec.Body.String())
	}
}

// The user's words, 2026-10-14: "cloud jobs dont need to be cut off since
// they dont use our resources."
func TestACloudRequestInFlightOutlivesTheStop(t *testing.T) {
	h := newHarness(t, nil)
	inside := make(chan struct{})
	release := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		select {
		case <-release:
			rw.Write([]byte("the whole reply"))
		case <-r.Context().Done():
		}
	}))
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- send(handler, "openrouter/free", "batch-key") }()
	<-inside
	h.w.SetOff("quiet hours")
	close(release)
	if rec := <-done; rec.Code != http.StatusOK || rec.Body.String() != "the whole reply" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// A GET to /upstream/<model>/ starts a model past every gate. The next read
// of the off file stops it.
func TestOffStopsAModelThatStartsAnyway(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetOff("quiet hours")
	h.models.mu.Lock()
	h.models.running["qwen"] = "starting"
	h.models.mu.Unlock()
	h.w.SetOff("quiet hours")
	if h.models.unloads != 2 || len(h.models.Running()) != 0 {
		t.Fatalf("unloads %d, running %v", h.models.unloads, h.models.Running())
	}
	h.w.SetOff("quiet hours")
	if h.models.unloads != 2 {
		t.Fatalf("stopped nothing again: %d unloads", h.models.unloads)
	}
}

// A job's request is paused, not cancelled, and held back for nothing: its
// next attempt goes through failover to a place that is on.
func TestOffPausesAJobWithoutHoldingIt(t *testing.T) {
	h := newHarness(t, nil)
	told := make(chan Pause, 1)
	inside := make(chan struct{})
	release := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), jobRequest(func(p Pause) { told <- p }))
		close(done)
	}()
	<-inside
	h.w.SetOff("too hot")
	p := <-told
	close(release)
	<-done
	if p.Device != "off" || p.Hold != 0 || !strings.Contains(p.Reason, "too hot") {
		t.Fatalf("%+v", p)
	}
}

func TestOnAgainWakesWhoWaitsAndTheVerdictShowsIt(t *testing.T) {
	h := newHarness(t, nil)
	woken := 0
	h.w.OnResume(func() { woken++ })
	h.w.SetOff("")
	if woken != 0 {
		t.Fatal("woken while nothing was off")
	}
	h.w.SetOff("quiet hours")
	h.at(time.Minute).w.SetOff("too hot")
	raw, _ := json.Marshal(h.w.State())
	var state struct{ Off *Off }
	json.Unmarshal(raw, &state)
	if state.Off == nil || state.Off.Reason != "too hot" || !state.Off.Since.Equal(t0) {
		t.Fatalf("%s", raw)
	}
	h.w.SetOff("")
	if h.w.State().Off != nil {
		t.Fatal("still off in the verdict")
	}
	if woken != 1 {
		t.Fatalf("woken %d times", woken)
	}
}

// A start llama-swap's processes refuse answers as the gate does (0025).
func TestARefusedStartAnswersAsTheGate(t *testing.T) {
	h := newHarness(t, nil)
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {}))
	h.w.SetOff("quiet hours")
	gate := send(handler, "qwen", "my-key")
	e := h.w.OffError("quiet hours")
	if e.StatusCode() != gate.Code || e.Header().Get("Retry-After") != gate.Header().Get("Retry-After") || string(e.Body()) != gate.Body.String() {
		t.Fatalf("start: %d %v %s\ngate: %d %v %s", e.StatusCode(), e.Header(), e.Body(), gate.Code, gate.Header(), gate.Body.String())
	}
}
