package router

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/stream"
)

// InferMux: a WebSocket session counts in the scheduler only while it moves
// data (0018, 10). The scheduler asks the processes how many of their
// sessions are idle whenever it compares counts; the router only has to
// wake it when a session stops moving data, and close a session before a
// swap stops the model under it. A file of its own, so a merge from upstream
// touches only the hook lines in base.go that call it.

// sessionPoll is how often an open session is looked at for having gone
// idle. A request queued behind it waits up to this long past its window.
const sessionPoll = time.Second

var _ scheduler.SessionEffects = (*baseRouter)(nil)

// IdleSessions implements scheduler.SessionEffects.
func (b *baseRouter) IdleSessions(modelID string) int {
	if s, ok := b.processes[modelID].(process.Sessions); ok {
		return s.IdleSessions()
	}
	return 0
}

// watchSession follows r's session, when it has one, for as long as its
// handler runs, and tells the run loop each time it stops moving data. The
// returned func ends the watch.
func (b *baseRouter) watchSession(modelID string, r *http.Request) (stop func()) {
	s := stream.From(r.Context())
	if s == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(sessionPoll)
		defer tick.Stop()
		wasIdle := false
		for {
			select {
			case <-tick.C:
			case <-done:
				return
			case <-b.shutdownCtx.Done():
				return
			}
			idle, _ := s.Idle()
			if idle && !wasIdle {
				select {
				case b.sessionIdleCh <- modelID:
				case <-done:
					return
				case <-b.shutdownCtx.Done():
					return
				}
			}
			wasIdle = idle
		}
	}()
	return func() { close(done) }
}

// onSessionIdle runs on the run loop.
func (b *baseRouter) onSessionIdle(modelID string) {
	if s, ok := b.schedule.(scheduler.SessionScheduler); ok {
		s.OnSessionIdle(modelID)
	}
}

// closeSessions closes id's sessions, idle when the swap was decided, before
// the swap to target stops it (0018, 7).
func closeSessions(p process.Process, id, target string) {
	if s, ok := p.(process.Sessions); ok {
		s.CloseSessions(stream.TryAgainLater, fmt.Sprintf("infermux: %s unloaded to load %s", id, target))
	}
}
