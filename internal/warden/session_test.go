package warden

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/stream/wstest"
)

// --- sessions (0018) -----------------------------------------------------------

// lockedClock is the harness's time for a test whose proxy goroutines read
// it while the test moves it.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) at(d time.Duration) {
	c.mu.Lock()
	c.t = t0.Add(d)
	c.mu.Unlock()
}

func openSession(t *testing.T, front *httptest.Server, key string) *wstest.Client {
	t.Helper()
	c, resp := wstest.Open(t, front.URL, "/v1/realtime?model=qwen", http.Header{"Authorization": {"Bearer " + key}})
	if c == nil {
		t.Fatalf("upgrade: %s", resp.Status)
	}
	return c
}

func sessionHarness(t *testing.T) (*harness, *lockedClock, *httptest.Server) {
	t.Helper()
	h := newHarness(t, nil)
	c := &lockedClock{t: t0}
	h.w.now = c.now
	h.w.announcer.now = c.now
	echo := httptest.NewServer(wstest.Echo())
	t.Cleanup(echo.Close)
	backend, _ := url.Parse(echo.URL)
	front := httptest.NewServer(h.w.Wrap(httputil.NewSingleHostReverseProxy(backend)))
	t.Cleanup(front.Close)
	return h, c, front
}

func (h *harness) sessions() []FlightState {
	var out []FlightState
	for _, f := range h.w.traffic.state().InFlight {
		if f.Session != nil {
			out = append(out, f)
		}
	}
	return out
}

func TestAnIdleSessionNoLongerHoldsTheCard(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	if op, p := client.RoundTrip(2, []byte("audio")); op != 2 || string(p) != "audio" {
		t.Fatalf("echo %d %q", op, p)
	}
	h.reading = busy(60)
	c.at(2 * time.Second)
	h.w.Tick()
	if h.models.unloads != 0 || !h.w.State().PendingUnload {
		t.Fatal("unloaded under a session that just moved data")
	}
	if s := h.sessions(); len(s) != 1 || !s[0].Session.Moving {
		t.Fatalf("sessions %+v", s)
	}
	// Open, and idle: it counts from its last data, as a request's end.
	c.at(time.Second + 599*time.Second)
	h.w.Tick()
	if h.models.unloads != 0 {
		t.Fatal("unloaded within interactive_recent_seconds of the last data")
	}
	if s := h.sessions(); len(s) != 1 || s[0].Session.Moving || s[0].Session.IdleSeconds != 599 {
		t.Fatalf("sessions %+v", s)
	}
	c.at(time.Second + 600*time.Second)
	h.w.Tick()
	if h.models.unloads != 1 {
		t.Fatal("an open, idle session kept the models loaded")
	}
	code, reason := client.Closed()
	if code != 1013 || reason != "infermux: the GPU was yielded: "+h.w.State().Verdict.Reason {
		t.Fatalf("closed with %d %q", code, reason)
	}
}

func TestAPingDoesNotKeepASessionMoving(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	client.RoundTrip(1, []byte(`{"type":"session.update"}`))
	h.reading = obsWithCUDA()
	for at := 5 * time.Second; at <= 9*time.Second; at += 4 * time.Second {
		c.at(at)
		if op, _ := client.RoundTrip(9, []byte("keep")); op != 10 {
			t.Fatalf("no pong: %d", op)
		}
		h.w.Tick()
		if h.models.unloads != 0 {
			t.Fatalf("a priority process unloaded %s after the last data", at-time.Second)
		}
	}
	c.at(11 * time.Second)
	client.RoundTrip(9, []byte("keep"))
	h.w.Tick()
	if h.models.unloads != 1 {
		t.Fatal("pings kept a session in flight past its window")
	}
	if code, _ := client.Closed(); code != 1013 {
		t.Fatalf("closed with %d", code)
	}
}

func TestASessionMovingDataDefersEvenAPriorityUnload(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	h.reading = obsWithCUDA()
	for at := time.Second; at <= 41*time.Second; at += 8 * time.Second {
		c.at(at)
		client.RoundTrip(2, []byte("pcm"))
		c.at(at + 7*time.Second)
		h.w.Tick()
		if h.models.unloads != 0 {
			t.Fatalf("unloaded under the user speaking, at %s", at+7*time.Second)
		}
	}
	c.at(41*time.Second + 10*time.Second)
	h.w.Tick()
	if h.models.unloads != 1 {
		t.Fatal("not unloaded once the session went quiet")
	}
	client.Closed()
}

func TestABatchSessionIsClosedWhenTheGPUYields(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "batch-key")
	c.at(time.Second)
	client.RoundTrip(2, []byte("pcm"))
	h.reading = busy(60)
	h.w.Tick()
	code, reason := client.Closed()
	if code != 1013 || !strings.HasPrefix(reason, "infermux: batch session cancelled, the GPU was yielded: ") {
		t.Fatalf("closed with %d %q", code, reason)
	}
	if h.w.State().Traffic.CancelledBatch != 1 {
		t.Fatal("not counted as a cancelled batch request")
	}
}

func TestTheEndOfAnIdleSessionIsNotActivity(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	client.RoundTrip(2, []byte("pcm"))
	c.at(time.Hour)
	if op, _ := client.RoundTrip(8, []byte("\x03\xe8")); op != 8 {
		t.Fatal("the backend did not return the close")
	}
	client.Conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.w.traffic.state().InFlight) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	last := h.w.State().Traffic.LastInteractive
	if last == nil || !last.Equal(t0.Add(time.Second)) {
		t.Fatalf("last interactive %v, want the last data at %v", last, t0.Add(time.Second))
	}
}

func TestUnloadByHandWaitsOnlyForAMovingSession(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	client.RoundTrip(2, []byte("pcm"))
	c.at(2 * time.Second)
	if _, err := h.w.UnloadNow(); err == nil {
		t.Fatal("unloaded by hand under a session moving data")
	}
	c.at(12 * time.Second)
	if _, err := h.w.UnloadNow(); err != nil {
		t.Fatalf("an idle session refused the unload: %v", err)
	}
	if code, reason := client.Closed(); code != 1013 || reason != "infermux: the models were unloaded by hand" {
		t.Fatalf("closed with %d %q", code, reason)
	}
}

func TestAReloadClosesSessionsWithServiceRestart(t *testing.T) {
	h, _, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	if n := h.w.CloseSessions(1012, "infermux: the model configuration was reloaded"); n != 1 {
		t.Fatalf("closed %d", n)
	}
	if code, _ := client.Closed(); code != 1012 {
		t.Fatalf("closed with %d", code)
	}
}

func TestBinaryFramesPassThroughUnchanged(t *testing.T) {
	_, _, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, 300000} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i * 7)
		}
		op, got := client.RoundTrip(2, payload)
		if op != 2 || !bytes.Equal(got, payload) {
			t.Fatalf("%d bytes came back as op %d, %d bytes", n, op, len(got))
		}
	}
}
