package warden

import (
	"net/http"
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
	w.mu.Lock()
	message := "the local models were off"
	if w.off != nil {
		message = "the local models are off: " + w.off.Reason
	}
	retry := w.cfg.Policy.ResumeQuietSeconds
	w.mu.Unlock()
	writeRefusal(rw, "off", message, retry)
}
