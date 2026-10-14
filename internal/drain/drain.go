// Package drain holds a deploy's stop until the requests running finish
// (0026). From Start every new request gets 503 with Retry-After, the running
// ones get until a deadline, and Cut ends whatever is left.
package drain

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Drain is the outermost handler: every request passes it, so it alone sees
// all of them and can refuse a new one before anything starts work for it.
type Drain struct {
	// RetryAfter is the refusal's Retry-After, in seconds.
	RetryAfter int

	mu       sync.Mutex
	draining bool
	running  int
	next     uint64
	cancels  map[uint64]context.CancelFunc
}

// Wrap counts and refuses requests for next.
func (d *Drain) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		counts := Counts(r)
		id, ok := d.begin(counts, cancel)
		if !ok {
			d.refuse(rw)
			return
		}
		defer d.end(id, counts)
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

// Counts is whether the drain waits for r: anything but a read. A GET is a
// list, a WebSocket session or a stream that follows something, a job or
// the events, and those end only when what they follow does or the client
// leaves.
func Counts(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

func (d *Drain) begin(counts bool, cancel context.CancelFunc) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return 0, false
	}
	if d.cancels == nil {
		d.cancels = map[uint64]context.CancelFunc{}
	}
	d.next++
	d.cancels[d.next] = cancel
	if counts {
		d.running++
	}
	return d.next, true
}

func (d *Drain) end(id uint64, counts bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.cancels, id)
	if counts {
		d.running--
	}
}

func (d *Drain) refuse(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Retry-After", strconv.Itoa(d.RetryAfter))
	rw.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(rw).Encode(map[string]any{
		"error": map[string]string{"type": "draining", "message": "InferMux is restarting: the requests running finish first"},
	})
}

// Start refuses every request from now on.
func (d *Drain) Start() {
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
}

// Running is how many requests that count are running.
func (d *Drain) Running() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// Cut cancels every request still running, counted or not, and says how many
// it cancelled.
func (d *Drain) Cut() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cancel := range d.cancels {
		cancel()
	}
	return len(d.cancels)
}

// poll is how often Wait looks.
var poll = 100 * time.Millisecond

// Wait returns once every count is zero, true, or once timeout has passed,
// false.
func Wait(timeout time.Duration, counts ...func() int) bool {
	deadline := time.Now().Add(timeout)
	for {
		zero := true
		for _, count := range counts {
			if count() > 0 {
				zero = false
				break
			}
		}
		if zero {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(poll)
	}
}
