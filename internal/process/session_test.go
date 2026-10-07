package process

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/stream"
	"github.com/mostlygeek/llama-swap/internal/stream/wstest"
)

// pipeSession is a session upgraded over a pipe, on a clock the test moves
// forward, with the frames the client end read.
type pipeSession struct {
	s      *stream.Session
	conn   net.Conn // the session's end, as the proxy writes to it
	frames chan []byte
	shift  atomic.Int64
}

func newPipeSession(t *testing.T) *pipeSession {
	t.Helper()
	ps := &pipeSession{frames: make(chan []byte, 16)}
	clock := func() time.Time { return time.Now().Add(time.Duration(ps.shift.Load())) }
	ps.s = stream.New(clock(), clock)
	client, server := net.Pipe()
	ps.conn = ps.s.Attach(server)
	t.Cleanup(func() { client.Close(); server.Close() })
	go func() {
		r := bufio.NewReader(client)
		for {
			op, p, err := wstest.ReadFrame(r)
			if err != nil {
				close(ps.frames)
				return
			}
			if op == wstest.Close {
				ps.frames <- p
			}
		}
	}()
	return ps
}

// data sends the client a text frame now, which the session stamps.
func (ps *pipeSession) data(t *testing.T) {
	t.Helper()
	if _, err := ps.conn.Write(wstest.Frame(wstest.Text, []byte(`{"type":"x"}`), false)); err != nil {
		t.Fatal(err)
	}
}

// idle moves the session's clock past its window.
func (ps *pipeSession) idle() { ps.shift.Add(int64(stream.ActiveWindow + time.Second)) }

// closeCode is the code of the close frame the client got, or 0.
func (ps *pipeSession) closeCode(t *testing.T) int {
	t.Helper()
	select {
	case p, ok := <-ps.frames:
		if !ok || len(p) < 2 {
			return 0
		}
		return int(binary.BigEndian.Uint16(p))
	case <-time.After(3 * time.Second):
		return 0
	}
}

func sessionRequest(s *stream.Session) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v1/realtime", nil).WithContext(stream.With(context.Background(), s))
}

// A session counts as in flight while it moves data or has not upgraded, and
// the process's idle time runs from its last data, also once it ends
// (0018, 10).
func TestProcessCommand_ASessionIsInFlightOnlyWhileItMovesData(t *testing.T) {
	p := newProcessCommand(t, config.ModelConfig{})
	ps := newPipeSession(t)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.serveSession(func(http.ResponseWriter, *http.Request) { <-release }, httptest.NewRecorder(), sessionRequest(ps.s))
	}()
	deadline := time.Now().Add(testReturnTimeout)
	for p.inflight.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the session never counted in flight")
		}
		time.Sleep(time.Millisecond)
	}
	if !p.busy() {
		t.Fatal("not busy under a session moving data")
	}
	ps.data(t)
	_, last := ps.s.Idle()
	ps.idle()
	if p.busy() || p.IdleSessions() != 1 {
		t.Fatalf("busy %v with %d idle sessions, want idle with 1", p.busy(), p.IdleSessions())
	}
	if got := p.idleSince(); !got.Equal(last) {
		t.Fatalf("idle since %v, want the last data at %v", got, last)
	}
	p.inflight.Add(1) // a request beside it
	if !p.busy() {
		t.Fatal("not busy with a request beside an idle session")
	}
	p.inflight.Add(-1)

	// One that has not upgraded is a request in flight, however long.
	waiting := stream.New(time.Now().Add(-time.Hour), time.Now)
	releaseWaiting := make(chan struct{})
	defer close(releaseWaiting)
	go p.serveSession(func(http.ResponseWriter, *http.Request) { <-releaseWaiting }, httptest.NewRecorder(), sessionRequest(waiting))
	for p.inflight.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the second session never counted in flight")
		}
		time.Sleep(time.Millisecond)
	}
	if !p.busy() {
		t.Fatal("not busy under a session waiting for its upgrade")
	}

	close(release)
	<-done
	if got := p.lastUse.Load(); got != last.UnixNano() {
		t.Fatalf("last use %v after the session ended, want its last data %v", time.Unix(0, got), last)
	}
}

// The TTL holds off while a session moves data, then unloads the model under
// it once it is idle, timed from its last data, and closes it with 1013
// first (0018, 7 and 10).
func TestProcessCommand_TTL_UnloadsUnderAnIdleSession(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(mock.Close)
	t.Cleanup(func() { close(release) })

	p := newProcessCommand(t, config.ModelConfig{
		Cmd:                "sleep 3600",
		Proxy:              mock.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UnloadAfter:        2,
		UnloadTimeout:      1,
	})
	runErr := runAsync(t, p)
	ready := time.Now()
	defer func() {
		if p.State() == StateReady {
			p.Stop(testStopTimeout)
		}
		<-runErr
	}()

	ps := newPipeSession(t)
	go p.ServeHTTP(httptest.NewRecorder(), sessionRequest(ps.s))
	select {
	case <-started:
	case <-time.After(testReturnTimeout):
		t.Fatal("the session never reached the backend")
	}

	// Past the TTL from the start, under a session moving data.
	time.Sleep(time.Until(ready.Add(2400 * time.Millisecond)))
	if got := p.State(); got != StateReady {
		t.Fatalf("state %s under a session moving data, want ready", got)
	}
	ps.data(t)
	ps.idle()
	// Past the TTL from the start again, but not from the last data.
	time.Sleep(time.Until(ready.Add(4 * time.Second)))
	if got := p.State(); got != StateReady {
		t.Fatalf("state %s 1.6 s after the session's last data, want ready: the TTL runs from it", got)
	}
	waitForState(t, p, StateStopped)
	if code := ps.closeCode(t); code != stream.TryAgainLater {
		t.Fatalf("the client's close code was %d, want 1013", code)
	}
}
