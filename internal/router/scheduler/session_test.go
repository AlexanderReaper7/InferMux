package scheduler

import (
	"io"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// sessionFakes is fakeEffects with sessions: idle is how many of each
// model's requests in flight are idle sessions, as the processes would say.
type sessionFakes struct {
	*fakeEffects
	idle map[string]int
}

func (f *sessionFakes) IdleSessions(modelID string) int { return f.idle[modelID] }

func newSessionFakes() *sessionFakes {
	return &sessionFakes{fakeEffects: newFakeEffects(), idle: map[string]int{}}
}

// An idle session holds no concurrency slot, and takes it back when data
// moves again, past the limit if the slot was taken meanwhile (0018, 10).
func TestFIFO_AnIdleSessionHoldsNoConcurrencySlot(t *testing.T) {
	eff := newSessionFakes()
	eff.states["a"] = process.StateReady
	s := NewFIFO("test", logmon.NewWriter(io.Discard), &stubPlanner{}, config.FifoConfig{},
		map[string]config.ModelConfig{"a": {ConcurrencyLimit: 1}}, eff)

	session := req("a")
	s.OnRequest(session)
	assertAdmitted(t, session)
	full := req("a")
	s.OnRequest(full)
	assertAdmission429(t, full)

	eff.idle["a"] = 1
	other := req("a")
	s.OnRequest(other)
	assertAdmitted(t, other)
	if got := eff.served("a"); got != 2 {
		t.Fatalf("served %d, want the session and the request beside its idle session", got)
	}

	// Data moves again: the session counts, and the model is over its limit,
	// then at it once one of the two ends, then under it.
	eff.idle["a"] = 0
	over := req("a")
	s.OnRequest(over)
	assertAdmission429(t, over)
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	at := req("a")
	s.OnRequest(at)
	assertAdmission429(t, at)
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	under := req("a")
	s.OnRequest(under)
	assertAdmitted(t, under)
}

// A swap that evicts a model with only idle sessions on it starts at once.
func TestFIFO_AnIdleSessionDoesNotBlockASwap(t *testing.T) {
	eff := newSessionFakes()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a"))
	eff.idle["a"] = 1
	s.OnRequest(req("b"))
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("b's swap started %d times, want 1: a has only an idle session", got)
	}
}

// A request queued behind a session goes ahead when the session goes idle,
// and not before.
func TestFIFO_AQueuedSwapGoesWhenTheSessionGoesIdle(t *testing.T) {
	eff := newSessionFakes()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a"))
	s.OnRequest(req("b"))
	s.OnSessionIdle("a") // a session elsewhere; a's still moves data
	if got := eff.startsFor("b"); got != 0 {
		t.Fatalf("b's swap started under a session moving data")
	}
	eff.idle["a"] = 1
	s.OnSessionIdle("a")
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("b's swap started %d times after a's session went idle, want 1", got)
	}
}

// A request ending with only idle sessions left on its model lets a queued
// swap go.
func TestFIFO_ARequestEndingBesideAnIdleSessionLetsASwapGo(t *testing.T) {
	eff := newSessionFakes()
	eff.states["a"] = process.StateReady
	eff.states["b"] = process.StateStopped
	s := newFIFO(&stubPlanner{evict: map[string][]string{"b": {"a"}}}, eff)

	s.OnRequest(req("a")) // the session
	s.OnRequest(req("a")) // a request
	eff.idle["a"] = 1
	s.OnRequest(req("b"))
	if got := eff.startsFor("b"); got != 0 {
		t.Fatal("b's swap started under a's request")
	}
	s.OnServeDone(ServeDoneEvent{ModelID: "a"})
	if got := eff.startsFor("b"); got != 1 {
		t.Fatalf("b's swap started %d times after a's request ended, want 1", got)
	}
}
