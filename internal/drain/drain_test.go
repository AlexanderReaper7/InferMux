package drain

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestARunningRequestFinishesAndANewOneIsRefused(t *testing.T) {
	d := &Drain{RetryAfter: 5}
	inside := make(chan struct{})
	release := make(chan struct{})
	h := d.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(inside)
		<-release
		rw.Write([]byte("the whole reply"))
	}))
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		done <- rec
	}()
	<-inside
	d.Start()
	if d.Running() != 1 {
		t.Fatalf("running %d", d.Running())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" || !strings.Contains(rec.Body.String(), `"draining"`) {
		t.Fatalf("new: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}

	close(release)
	if rec := <-done; rec.Code != http.StatusOK || rec.Body.String() != "the whole reply" {
		t.Fatalf("running: %d %s", rec.Code, rec.Body.String())
	}
	if d.Running() != 0 {
		t.Fatalf("running %d after it ended", d.Running())
	}
}

// A GET is not waited for, and the cut ends it with everything else.
func TestTheCutEndsWhatIsLeftAndAReadIsNotWaitedFor(t *testing.T) {
	d := &Drain{}
	started := make(chan struct{}, 2)
	h := d.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	done := make(chan struct{}, 2)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		go func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/x", nil))
			done <- struct{}{}
		}()
	}
	<-started
	<-started
	d.Start()
	if d.Running() != 1 {
		t.Fatalf("running %d, want the POST only", d.Running())
	}
	if d.Cut() != 2 {
		t.Fatal("not both cut")
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("a request outlived the cut")
		}
	}
}

func TestWaitReturnsAtZeroOrTheDeadline(t *testing.T) {
	poll = time.Millisecond
	n := 2
	if !Wait(time.Second, func() int { n--; return n }) {
		t.Fatal("not zero")
	}
	start := time.Now()
	if Wait(20*time.Millisecond, func() int { return 0 }, func() int { return 1 }) {
		t.Fatal("zero while one runs")
	}
	if waited := time.Since(start); waited < 20*time.Millisecond || waited > time.Second {
		t.Fatalf("waited %s", waited)
	}
}
