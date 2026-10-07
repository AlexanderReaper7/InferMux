package warden

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// What the user can do to the warden from outside: set the verdict by hand,
// act on the models and ComfyUI now, and have the settings reloaded. All of it
// is POST under /warden/, behind guard (0005).

// Manual is a verdict the user set by hand. It holds until the warden's own
// decision differs from OwnAction, the decision it had when the user set it:
// a manual resume during a game lasts until the game ends, and a manual pause
// on a quiet card lasts until the next contention has come and gone (0005).
type Manual struct {
	Action    string    `json:"action"`
	OwnAction string    `json:"own_action"`
	At        time.Time `json:"at"`
}

func (m *Manual) verdict() Verdict {
	at := m.At
	return Verdict{
		Yielded: m.Action == "pause",
		Reason:  m.Action + "d by hand",
		Since:   &at,
	}
}

// SetManual sets the verdict by hand, or with "auto" hands it back. A manual
// verdict equal to the warden's own decision is the same as auto.
func (w *Warden) SetManual(action string) (Verdict, error) {
	if action != "pause" && action != "resume" && action != "auto" {
		return Verdict{}, fmt.Errorf("action must be pause, resume or auto, not %q", action)
	}
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	defer w.runWhenQuiet()

	w.mu.Lock()
	if !w.cfg.Policy.Enabled {
		w.mu.Unlock()
		return Verdict{}, fmt.Errorf("the warden is disabled")
	}
	before := w.verdict
	if action == "auto" || action == w.own.Action() {
		w.manual = nil
	} else {
		w.manual = &Manual{Action: action, OwnAction: w.own.Action(), At: w.now()}
	}
	after := w.effectiveLocked()
	w.verdict = after
	w.mu.Unlock()

	w.log.Infof("Verdict set by hand: %s", action)
	w.act(before, after)
	return after, nil
}

// Forgive drops an owed unload. Returns whether one was owed.
func (w *Warden) Forgive() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	owed := w.pendingUnload
	w.pendingUnload = false
	if owed {
		w.log.Infof("Owed unload forgiven by hand")
	}
	return owed
}

// UnloadNow unloads every model, unless an interactive request is in flight:
// the user's own request is never killed (0004), not even by the user's click.
func (w *Warden) UnloadNow() ([]string, error) {
	if n := w.traffic.interactiveInFlight(); n > 0 {
		return nil, fmt.Errorf("%d interactive request(s) in flight", n)
	}
	w.closeSessions("infermux: the models were unloaded by hand")
	unloaded := w.models.UnloadAll()
	w.mu.Lock()
	w.pendingUnload = false
	w.lastUnload = unloaded
	w.mu.Unlock()
	w.log.Infof("Models unloaded by hand: %s", orNone(strings.Join(unloaded, ", ")))
	return unloaded, nil
}

// CancelBatch cancels every batch request in flight without refusing new ones.
func (w *Warden) CancelBatch() int {
	n := w.traffic.cancelBatch("infermux: batch session cancelled by hand")
	w.log.Infof("Cancelled %d batch request(s) by hand", n)
	return n
}

// FreeComfyUI tells ComfyUI to drop its models now.
func (w *Warden) FreeComfyUI() error {
	w.mu.Lock()
	free := w.comfyFree
	w.mu.Unlock()
	if free == nil {
		return fmt.Errorf("no ComfyUI is configured")
	}
	if err := free(); err != nil {
		return err
	}
	w.mu.Lock()
	w.comfy.Freed = true
	w.mu.Unlock()
	w.log.Infof("ComfyUI freed by hand")
	return nil
}

// Reload swaps in new settings without touching the verdict, the requests in
// flight or what the consumers have heard. The next tick decides under the
// new policy.
func (w *Warden) Reload(cfg Config) {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()

	w.mu.Lock()
	old := w.cfg
	w.cfg = cfg
	if !slices.Equal(old.OurUnits, cfg.OurUnits) || !slices.Equal(old.DesktopProcesses, cfg.DesktopProcesses) ||
		!slices.Equal(old.PriorityProcesses, cfg.PriorityProcesses) {
		w.probe, w.freshProbe = newProbe(cfg), newProbe(cfg)
	}
	if old.ComfyUIURL != cfg.ComfyUIURL {
		w.setComfyUI(cfg.ComfyUIURL)
	}
	w.mu.Unlock()

	w.traffic.setKeys(cfg.Keys)
	w.announcer.setConsumers(cfg.Consumers)
	w.logPolicy("Warden settings reloaded:")
}

// WhenNoInteractive runs fn once no interactive request is in flight: now, if
// none is, or when the last one ends. Used for llama-swap's config reload,
// which stops every model, so that a change saved in the UI cannot cut off a
// reply the user is waiting for (0005). A second call under the same name
// before the first ran replaces it.
func (w *Warden) WhenNoInteractive(name string, fn func()) {
	w.mu.Lock()
	w.whenQuiet[name] = fn
	w.mu.Unlock()
	w.runWhenQuiet()
}

func (w *Warden) runWhenQuiet() {
	n := w.traffic.interactiveInFlight()
	w.mu.Lock()
	if len(w.whenQuiet) == 0 {
		w.mu.Unlock()
		return
	}
	if n > 0 {
		for name := range w.whenQuiet {
			if !w.quietLogged[name] {
				w.quietLogged[name] = true
				w.log.Infof("%s waits: %d interactive request(s) in flight", name, n)
			}
		}
		w.mu.Unlock()
		return
	}
	due := w.whenQuiet
	w.whenQuiet, w.quietLogged = map[string]func(){}, map[string]bool{}
	w.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

func (w *Warden) waitingLocked() []string {
	names := make([]string, 0, len(w.whenQuiet))
	for name := range w.whenQuiet {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// guard refuses a write a web page in some other origin made the browser
// send. CORS keeps such a page from reading an answer, not from sending a
// simple POST, so a page open in Firefox could otherwise unload the models or
// pause Episteme. A browser write has to come from InferMux's own origin on a
// loopback name or a trusted host: a page that rebinds its own DNS name to
// 127.0.0.1 is same origin, but neither. Clients that are not browsers send no
// Origin and pass. /warden/ writes also need X-InferMux, which no page can add
// without a preflight that llama-swap's CORS settings refuse (0005).
func (w *Warden) guard(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// A WebSocket is a GET that CORS does not cover, and a browser may
		// send a key it has cached for this host, so it is a write.
		if !isWebSocket(r) {
			return ""
		}
	}
	if refused := SameOrigin(r, w.config().TrustedHosts); refused != "" {
		return refused
	}
	if strings.HasPrefix(r.URL.Path, "/warden/") && r.Header.Get("X-InferMux") == "" {
		return "writes under /warden/ need the X-InferMux header"
	}
	return ""
}

// SameOrigin is guard's origin rule, shared with infermux-ui: no Origin, or
// the request's own origin on a known host.
func SameOrigin(r *http.Request, trusted []string) string {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return ""
	}
	if origin != "http://"+r.Host && origin != "https://"+r.Host {
		return "cross-origin writes are refused: " + origin
	}
	if !KnownHost(r.Host, trusted) {
		return "browser writes are only taken on a loopback name or a trusted host, not " + r.Host
	}
	return ""
}

// KnownHost is true for localhost, 127.0.0.1, [::1] and the trusted names,
// with or without a port.
func KnownHost(hostport string, trusted []string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	for _, t := range trusted {
		if strings.EqualFold(host, strings.TrimSuffix(t, ".")) {
			return true
		}
	}
	return false
}

func (w *Warden) controls(mux *http.ServeMux) {
	mux.HandleFunc("POST /warden/manual", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"detail": err.Error()})
			return
		}
		v, err := w.SetManual(body.Action)
		if err != nil {
			writeJSON(rw, http.StatusConflict, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(rw, http.StatusOK, v)
	})
	mux.HandleFunc("POST /warden/forgive", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, http.StatusOK, map[string]bool{"was_owed": w.Forgive()})
	})
	mux.HandleFunc("POST /warden/unload", func(rw http.ResponseWriter, r *http.Request) {
		unloaded, err := w.UnloadNow()
		if err != nil {
			writeJSON(rw, http.StatusConflict, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(rw, http.StatusOK, map[string][]string{"unloaded": unloaded})
	})
	mux.HandleFunc("POST /warden/cancel-batch", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, http.StatusOK, map[string]int{"cancelled": w.CancelBatch()})
	})
	mux.HandleFunc("POST /warden/comfyui/free", func(rw http.ResponseWriter, r *http.Request) {
		if err := w.FreeComfyUI(); err != nil {
			writeJSON(rw, http.StatusBadGateway, map[string]string{"detail": err.Error()})
			return
		}
		writeJSON(rw, http.StatusOK, map[string]bool{"freed": true})
	})
}
