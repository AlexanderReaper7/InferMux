package warden

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// A hold keeps every model off the GPU while a game runs (0024, 5). It lasts
// as long as one request to POST /warden/hold on the control socket stays
// open: infermux-hold keeps it open while its command runs, and the warden
// counts the open ones. While one is, Fits refuses every start on the GPU,
// the user's own request's and ComfyUI's included, whatever asked for it: a
// request, a GET to /upstream/, preload. An infermux-hold that dies, by
// SIGKILL too, closes its connection and with it the hold, and a daemon that
// restarts has none, so no hold outlives what made it. Holds stack: the card
// stays held until the last one ends.

// Hold counts a hold until release, which ends it and must be called once.
// n is the number of holds with this one.
func (w *Warden) Hold() (n int, release func()) {
	w.mu.Lock()
	w.holds++
	n = w.holds
	w.mu.Unlock()
	w.log.Infof("GPU held, %d hold(s): no model starts on it", n)
	return n, func() {
		w.mu.Lock()
		w.holds--
		left := w.holds
		w.mu.Unlock()
		if left == 0 {
			w.log.Infof("GPU hold ended: models may start on it again")
		} else {
			w.log.Infof("GPU hold ended, %d hold(s) left", left)
		}
	}
}

// holdRoute is the hold's endpoint, on the control socket only: the answer's
// headers say the hold is counted, and the body stays open until the client
// closes the connection. Go's server cancels the request's context when the
// connection ends, and the control socket's stop closes every connection.
func (w *Warden) holdRoute(mux *http.ServeMux) {
	mux.HandleFunc("POST /warden/hold", func(rw http.ResponseWriter, r *http.Request) {
		n, release := w.Hold()
		defer release()
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		json.NewEncoder(rw).Encode(map[string]int{"holds": n})
		http.NewResponseController(rw).Flush()
		<-r.Context().Done()
	})
}

// heldLocked is the refusal of a start on the GPU while a hold is open.
func (w *Warden) heldLocked(model string) error {
	if w.holds == 0 {
		return nil
	}
	w.log.Infof("<%s> not started: the GPU is held, %d hold(s)", model, w.holds)
	return HeldError{Model: model, RetryAfter: w.cfg.Policy.ResumeQuietSeconds}
}

// HeldError is a start on the GPU refused while the GPU is held, with the
// answer the gate writes: the 503 gpu_held of a request the hold cut, with
// the GPU's resume window as Retry-After, so a client that honours it does
// not start the model while the game reads its budget, and failover tries
// the model's next place at once (0016). It has the methods of llama-swap's
// HTTPError.
type HeldError struct {
	Model      string
	RetryAfter int
}

func (e HeldError) Error() string {
	return fmt.Sprintf("%s not started: the GPU is held for a game", e.Model)
}

func (e HeldError) StatusCode() int { return http.StatusServiceUnavailable }

func (e HeldError) Header() http.Header { return refusalHeader(e.RetryAfter) }

func (e HeldError) Body() []byte { return refusalBody("gpu_held", e.Error()) }
