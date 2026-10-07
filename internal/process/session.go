package process

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/stream"
)

// InferMux: a WebSocket session is in flight on its process only while it
// moves data (0018, 10). A file of its own, so a merge from upstream touches
// only the hook lines in process_command.go that call it.

// Sessions is what the router asks a process about its open sessions.
type Sessions interface {
	// IdleSessions is how many of the requests in flight are sessions that
	// moved no data within stream.ActiveWindow.
	IdleSessions() int
	// CloseSessions sends each open session a close frame with code and
	// reason, at a frame boundary, and closes it.
	CloseSessions(code int, reason string)
}

var _ Sessions = (*ProcessCommand)(nil)

// sessionSet is a process's open sessions. The zero value is empty.
type sessionSet struct {
	mu  sync.Mutex
	all map[*stream.Session]struct{}
}

func (set *sessionSet) add(s *stream.Session) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.all == nil {
		set.all = map[*stream.Session]struct{}{}
	}
	set.all[s] = struct{}{}
}

func (set *sessionSet) remove(s *stream.Session) {
	set.mu.Lock()
	defer set.mu.Unlock()
	delete(set.all, s)
}

func (set *sessionSet) list() []*stream.Session {
	set.mu.Lock()
	defer set.mu.Unlock()
	out := make([]*stream.Session, 0, len(set.all))
	for s := range set.all {
		out = append(out, s)
	}
	return out
}

// serveSession serves a request that carries a session and reports whether
// r had one. It counts in inflight as any request does, and is in the set
// for as long as it does, so an idle session can be subtracted from it.
func (p *ProcessCommand) serveSession(fn http.HandlerFunc, w http.ResponseWriter, r *http.Request) bool {
	s := stream.From(r.Context())
	if s == nil {
		return false
	}
	p.inflight.Add(1)
	p.sessions.add(s)
	defer func() {
		// The TTL runs from the last data, not from the close: an idle
		// session's end is not use. One that never upgraded was a request.
		end := time.Now()
		if last, ok := s.LastData(); ok {
			end = last
		}
		p.usedAt(end)
		p.sessions.remove(s)
		p.inflight.Add(-1)
	}()
	fn(w, r)
	return true
}

// usedAt moves lastUse forward to t, never back.
func (p *ProcessCommand) usedAt(t time.Time) {
	at := t.UnixNano()
	for {
		prev := p.lastUse.Load()
		if at <= prev || p.lastUse.CompareAndSwap(prev, at) {
			return
		}
	}
}

// IdleSessions implements Sessions.
func (p *ProcessCommand) IdleSessions() int {
	n := 0
	for _, s := range p.sessions.list() {
		if idle, _ := s.Idle(); idle {
			n++
		}
	}
	return n
}

// CloseSessions implements Sessions.
func (p *ProcessCommand) CloseSessions(code int, reason string) {
	for _, s := range p.sessions.list() {
		s.Close(code, reason)
	}
}

// closeSessionsForTTL closes the open sessions, all idle, before the TTL
// unloads the process under them (0018, 7).
func (p *ProcessCommand) closeSessionsForTTL() {
	p.CloseSessions(stream.TryAgainLater, fmt.Sprintf("infermux: %s unloaded after %d s unused", p.id, p.config.UnloadAfter))
}

// busy is whether a request, or a session moving data, is in flight: what
// holds off the TTL.
func (p *ProcessCommand) busy() bool {
	return p.inflight.Load() > int64(p.IdleSessions())
}

// idleSince is when the process was last used: the end of its last request,
// or the last data of an idle session still open, whichever is later.
func (p *ProcessCommand) idleSince() time.Time {
	since := time.Unix(0, p.lastUse.Load())
	for _, s := range p.sessions.list() {
		if _, last := s.Idle(); last.After(since) {
			since = last
		}
	}
	return since
}
