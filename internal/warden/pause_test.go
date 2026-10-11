package warden

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A job's request (0023): the warden tells the job, and the job stops it.

func jobRequest(pause func(Pause)) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`))
	req.Header.Set("Authorization", "Bearer batch-key")
	return req.WithContext(WithPauser(req.Context(), pause))
}

func TestAYieldTellsAJobInsteadOfCancellingIt(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	told := make(chan Pause, 1)
	inside := make(chan struct{})
	release := make(chan struct{})
	var cancelled bool
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		select {
		case <-r.Context().Done():
			cancelled = true
		case <-release:
		}
	}))
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, jobRequest(func(p Pause) { told <- p }))
		done <- rec
	}()
	<-inside
	h.reading = busy(60, "blender")
	h.w.Tick()
	p := <-told
	if p.Device != DeviceGPU || !strings.Contains(p.Reason, "the GPU was yielded") || !strings.Contains(p.Reason, "blender") || p.Hold != 0 {
		t.Fatalf("told %+v", p)
	}
	close(release)
	rec := <-done
	if cancelled || rec.Code != http.StatusOK {
		t.Fatalf("the warden cancelled the job's request itself: %v, %d %s", cancelled, rec.Code, rec.Body)
	}
}

func TestACancelByHandHoldsAJobForTheResumeWindow(t *testing.T) {
	h := newHarness(t, nil)
	h.w.Tick()
	told := make(chan Pause, 1)
	inside := make(chan struct{})
	release := make(chan struct{})
	handler := h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-release
	}))
	go handler.ServeHTTP(httptest.NewRecorder(), jobRequest(func(p Pause) { told <- p }))
	<-inside
	h.w.CancelBatch()
	if p := <-told; p.Device != "" || p.Reason != "cancelled by hand" || p.Hold != 300*time.Second {
		t.Fatalf("told %+v", p)
	}
	close(release)
}

func TestAJobsBatchRequestIsStillRefusedWhileYielded(t *testing.T) {
	h := newHarness(t, nil)
	h.reading = busy(60)
	h.w.Tick()
	rec := httptest.NewRecorder()
	h.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("served") })).ServeHTTP(rec, jobRequest(func(Pause) { t.Fatal("told") }))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "gpu_yielded") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestAResumeWakesWhoAsked(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy.ResumeQuietSeconds = 60 })
	woken := 0
	h.w.OnResume(func() { woken++ })
	h.reading = busy(60)
	h.w.Tick()
	h.reading = quiet()
	h.at(30 * time.Second).w.Tick()
	if woken != 0 {
		t.Fatal("woken before the quiet")
	}
	h.at(61 * time.Second).w.Tick()
	if woken != 1 {
		t.Fatalf("woken %d times", woken)
	}
}
