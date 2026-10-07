package router

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/stream"
)

// sessionProcess is a fakeProcess that follows its sessions as
// ProcessCommand does, and records the closes a swap sends before its stop.
type sessionProcess struct {
	*fakeProcess

	mu       sync.Mutex
	sessions []*stream.Session
	closes   []string
	// closedFirst is whether every close came before the first Stop.
	closedFirst atomic.Bool
}

var _ process.Sessions = (*sessionProcess)(nil)

func (p *sessionProcess) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s := stream.From(r.Context()); s != nil {
		p.mu.Lock()
		p.sessions = append(p.sessions, s)
		p.mu.Unlock()
	}
	p.fakeProcess.ServeHTTP(w, r)
}

func (p *sessionProcess) IdleSessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.sessions {
		if idle, _ := s.Idle(); idle {
			n++
		}
	}
	return n
}

func (p *sessionProcess) CloseSessions(code int, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes = append(p.closes, reason)
	p.closedFirst.Store(code == stream.TryAgainLater && p.stopCalls.Load() == 0)
}

// A request whose swap would stop a model under a session moving data waits;
// once the session is idle it goes ahead without anything else happening,
// and the session is closed before its model stops (0018, 7 and 10).
func TestBaseRouter_ASwapWaitsForASessionToGoIdle(t *testing.T) {
	a := &sessionProcess{fakeProcess: newFakeProcess("a")}
	a.markReady()
	a.serveBlock = make(chan struct{})
	pb := newFakeProcess("b")
	pb.autoReady = true
	b := newTestBase(t, map[string]process.Process{"a": a, "b": pb}, &stubPlanner{
		evict: map[string][]string{"b": {"a"}},
	})

	var shift atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	session := stream.New(clock(), clock)
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go io.Copy(io.Discard, client)
	session.Attach(server)

	sessionDone := make(chan struct{})
	go func() {
		defer close(sessionDone)
		r := newRequest("a").WithContext(stream.With(context.Background(), session))
		b.ServeHTTP(httptest.NewRecorder(), r)
	}()
	waitSignal(t, a.serveStarted, "the session's start")

	swapped := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		b.ServeHTTP(w, newRequest("b"))
		swapped <- w.Code
	}()
	select {
	case <-swapped:
		t.Fatal("b swapped in under a session moving data")
	case <-time.After(sessionPoll + 500*time.Millisecond):
	}

	shift.Add(int64(stream.ActiveWindow + time.Second))
	select {
	case code := <-swapped:
		if code != http.StatusOK {
			t.Fatalf("b answered %d", code)
		}
	case <-time.After(sessionPoll + 2*time.Second):
		t.Fatal("b still waits after a's only session went idle")
	}
	a.mu.Lock()
	closes := append([]string(nil), a.closes...)
	a.mu.Unlock()
	if len(closes) != 1 || !strings.Contains(closes[0], "a unloaded to load b") || !a.closedFirst.Load() {
		t.Fatalf("closes %q, closed with 1013 before the stop: %v", closes, a.closedFirst.Load())
	}

	close(a.serveBlock)
	waitSignal(t, sessionDone, "the session's end")
}
