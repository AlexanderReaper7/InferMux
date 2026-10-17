package warden

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A safety stop takes this host's local models off and leaves the daemon up
// (0025): the zbox's quiet hours and thermal pause, which stopped the whole
// daemon until 2026-10-14 and with it the cloud jobs it was following. Off is
// the one place the warden kills the user's own request, the exception to
// 0004 a safety stop is: every request for a local model is refused whatever
// its class, the ones in flight are cancelled and a job's paused, and the
// models stop at once, without waiting for interactive quiet. Another host's
// model and a cloud's pass as always (0009).

// offDevice stands for a device in a refusal or a cancel while the local
// models are off: every one of them is.
const offDevice = "off"

// Off is why the local models are off, and since when.
type Off struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// SetOff takes the local models off with reason, or with "" puts them back.
// Called again while off, it stops any model that started since, as a GET to
// /upstream/<model>/ can: no gate sees a GET.
func (w *Warden) SetOff(reason string) {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()

	w.mu.Lock()
	was := w.off
	switch {
	case reason == "":
		w.off = nil
	case was == nil:
		w.off = &Off{Reason: reason, Since: w.now()}
	case was.Reason != reason:
		w.off = &Off{Reason: reason, Since: was.Since}
	}
	w.mu.Unlock()

	if reason == "" {
		if was != nil {
			w.traffic.setOff("")
			w.log.Infof("Local models on again, off since %s: %s", was.Since.Format(time.TimeOnly), was.Reason)
		}
		return
	}
	if was == nil || was.Reason != reason {
		w.log.Infof("Local models off: %s", reason)
		if n := w.traffic.setOff(reason); n > 0 {
			w.log.Infof("Cancelled %d request(s), the local models are off", n)
		}
	}
	if len(w.models.Running()) == 0 {
		return
	}
	unloaded := w.models.UnloadAll()
	w.mu.Lock()
	w.gpuUnload.pending, w.cpu.unload.pending = false, false
	w.mu.Unlock()
	w.log.Infof("Models stopped, the local models are off: %s", orNone(strings.Join(unloaded, ", ")))
}

// refuseOff answers 503 for a local model while the models are off, with the
// GPU's resume window as Retry-After, which a job waits at most.
func (w *Warden) refuseOff(rw http.ResponseWriter) {
	e := w.offRefusal()
	writeRefusal(rw, "off", e.Error(), e.RetryAfter)
}

func (w *Warden) offRefusal() OffError {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := OffError{RetryAfter: w.cfg.Policy.ResumeQuietSeconds}
	if w.off != nil {
		e.Reason = w.off.Reason
	}
	return e
}

// OffError is a model start refused while off, with the answer the gate
// writes. llama-swap writes it whole to whoever asked for the start: it has
// the methods of llama-swap's HTTPError.
type OffError struct {
	Reason     string
	RetryAfter int
}

// OffError is reason's refusal, with this warden's Retry-After.
func (w *Warden) OffError(reason string) OffError {
	return OffError{Reason: reason, RetryAfter: w.config().Policy.ResumeQuietSeconds}
}

func (e OffError) Error() string {
	if e.Reason == "" {
		return "the local models were off"
	}
	return "the local models are off: " + e.Reason
}

func (e OffError) StatusCode() int { return http.StatusServiceUnavailable }

// Header has no Retry-After before the warden exists, when only preload can
// be refused.
func (e OffError) Header() http.Header {
	h := http.Header{"Content-Type": {"application/json"}}
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	return h
}

func (e OffError) Body() []byte { return refusalBody("off", e.Error()) }
