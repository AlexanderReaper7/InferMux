package warden

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Who is asking (0004). A request presenting one of Config.BatchAPIKeys is
// batch: work that can wait, or not happen at all, like Episteme's scheduled
// jobs. Everything else is interactive, because a prompt the user just sent is
// never the thing to kill, and a client nobody configured should not be killed
// either. Forgetting the key makes a batch client too polite, never too rude.
//
// Batch requests are refused with 503 while the verdict is pause, and cancelled
// on a yield. Interactive requests always pass, and the models are not unloaded
// while one is in flight or was recent.

type Class string

const (
	Interactive Class = "interactive"
	Batch       Class = "batch"
)

type flight struct {
	class   Class
	path    string
	started time.Time
	cancel  context.CancelFunc
}

// FlightState is one in-flight request as /warden/verdict reports it.
type FlightState struct {
	Class      Class   `json:"class"`
	Path       string  `json:"path"`
	AgeSeconds float64 `json:"age_seconds"`
}

// TrafficState is what /warden/verdict reports about requests.
type TrafficState struct {
	Paused          bool          `json:"paused"`
	InFlight        []FlightState `json:"in_flight"`
	LastInteractive *time.Time    `json:"last_interactive"`
	RefusedBatch    int           `json:"refused_batch"`
	CancelledBatch  int           `json:"cancelled_batch"`
}

type traffic struct {
	batchKeys map[string]bool
	now       func() time.Time

	mu              sync.Mutex
	paused          bool
	next            uint64
	flights         map[uint64]*flight
	lastInteractive time.Time
	refused         int
	cancelled       int
}

func newTraffic(batchKeys []string, now func() time.Time) *traffic {
	t := &traffic{now: now, flights: map[uint64]*flight{}}
	t.setBatchKeys(batchKeys)
	return t
}

func (t *traffic) setBatchKeys(batchKeys []string) {
	keys := map[string]bool{}
	for _, k := range batchKeys {
		if k != "" {
			keys[k] = true
		}
	}
	t.mu.Lock()
	t.batchKeys = keys
	t.mu.Unlock()
}

// classify reads the key the way llama-swap accepts one: a bearer token,
// x-api-key, or the password of HTTP Basic.
func (t *traffic) classify(r *http.Request) Class {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, key := range presentedKeys(r) {
		if t.batchKeys[key] {
			return Batch
		}
	}
	return Interactive
}

func presentedKeys(r *http.Request) []string {
	var keys []string
	if k := r.Header.Get("x-api-key"); k != "" {
		keys = append(keys, k)
	}
	auth := r.Header.Get("Authorization")
	if scheme, value, ok := strings.Cut(auth, " "); ok {
		switch strings.ToLower(scheme) {
		case "bearer":
			keys = append(keys, strings.TrimSpace(value))
		case "basic":
			if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value)); err == nil {
				if _, password, ok := strings.Cut(string(raw), ":"); ok {
					keys = append(keys, password)
				}
			}
		}
	}
	return keys
}

// isInference is a request that can load or run a model: any POST outside the
// management API. A GET of /v1/models is not activity, and a POST to
// /api/models/unload from a batch client must not be refused.
func isInference(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/warden/")
}

// begin registers a request, or refuses it. The pause check and the
// registration share one lock with pause(), so a batch request is either
// refused or registered before the yield, never neither.
func (t *traffic) begin(class Class, path string, cancel context.CancelFunc) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if class == Batch && t.paused {
		t.refused++
		return 0, false
	}
	t.next++
	now := t.now()
	t.flights[t.next] = &flight{class: class, path: path, started: now, cancel: cancel}
	if class == Interactive {
		t.lastInteractive = now
	}
	return t.next, true
}

func (t *traffic) end(id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.flights[id]
	if !ok {
		return
	}
	delete(t.flights, id)
	if f.class == Interactive {
		t.lastInteractive = t.now()
	}
}

// pause refuses batch requests from now on and cancels the ones in flight.
// Returns how many were cancelled.
func (t *traffic) pause() int {
	t.mu.Lock()
	t.paused = true
	t.mu.Unlock()
	return t.cancelBatch()
}

// cancelBatch cancels the batch requests in flight. Returns how many.
func (t *traffic) cancelBatch() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, f := range t.flights {
		if f.class == Batch {
			f.cancel()
			n++
		}
	}
	t.cancelled += n
	return n
}

func (t *traffic) resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = false
}

func (t *traffic) interactiveInFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, f := range t.flights {
		if f.class == Interactive {
			n++
		}
	}
	return n
}

// interactiveRecent is true while an interactive request is in flight, and for
// window after the last one ended.
func (t *traffic) interactiveRecent(window time.Duration) (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, f := range t.flights {
		if f.class == Interactive {
			return true, t.lastInteractive
		}
	}
	if t.lastInteractive.IsZero() {
		return false, t.lastInteractive
	}
	return t.now().Sub(t.lastInteractive) < window, t.lastInteractive
}

func (t *traffic) state() TrafficState {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := TrafficState{
		Paused:         t.paused,
		InFlight:       []FlightState{},
		RefusedBatch:   t.refused,
		CancelledBatch: t.cancelled,
	}
	for _, f := range t.flights {
		s.InFlight = append(s.InFlight, FlightState{
			Class: f.class, Path: f.path, AgeSeconds: round1(now.Sub(f.started).Seconds()),
		})
	}
	if !t.lastInteractive.IsZero() {
		last := t.lastInteractive
		s.LastInteractive = &last
	}
	return s
}
