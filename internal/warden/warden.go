package warden

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/stream"
)

// Models is the warden's handle on llama-swap's local models. main supplies it
// over whichever server is active, since a config reload replaces the server
// but not the warden.
type Models interface {
	// Running is every model process that is not stopped, by ID, with its
	// state. "starting" counts as loaded: a model halfway into VRAM occupies
	// it as much as a resident one (0001).
	Running() map[string]string
	// UnloadAll stops every model and returns the ones that were running.
	UnloadAll() []string
	// Qualify is the model a request names, as <host>/<model> or
	// <peer>/<model>, for a key's allow list (0006, 4), and whether it is one
	// of this host's own, which is all the gate looks at (0009). ok is false
	// when the request names no model this host knows.
	Qualify(r *http.Request) (model string, local, ok bool)
	// QualifyName is Qualify for a name as /v1/models lists it, so the list a
	// key sees and the models it may use follow one allow list.
	QualifyName(name string) (model string, ok bool)
}

// Logger is the part of llama-swap's proxy log the warden writes to, so its
// lines land in the same stream, the UI's proxy log and journald.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Warden owns the verdict. Everything else reads it.
//
// Two verdicts are kept. own is what the measurements say. verdict is the one
// acted on and announced: own, unless the user set one by hand, which holds
// until own changes (0005).
type Warden struct {
	models Models
	log    Logger
	now    func() time.Time

	traffic   *traffic
	announcer *announcer

	// extra are the routes added by Handle.
	extra []route

	// tickMu serialises everything that moves the verdict: a tick, a manual
	// verdict, a reload. The probe is only called under it.
	tickMu sync.Mutex
	probe  func() (Resources, error)

	mu            sync.Mutex
	cfg           Config
	freshProbe    func() (Resources, error)
	comfyQueue    func() *int
	comfyFree     func() error
	own           Verdict
	verdict       Verdict
	manual        *Manual
	comfy         ComfyIdle
	lastResources *Resources
	probeError    *string
	lastUnload    []string
	pendingUnload bool
	deferLogged   bool
	whenQuiet     map[string]func()
	remotes       func() any
	quietLogged   map[string]bool

	stop chan struct{}
	done chan struct{}
}

// New builds a warden over the given models. It does nothing until Start.
func New(cfg Config, models Models, log Logger) *Warden {
	w := &Warden{
		cfg:         cfg,
		models:      models,
		log:         log,
		probe:       newProbe(cfg),
		freshProbe:  newProbe(cfg),
		now:         time.Now,
		announcer:   newAnnouncer(cfg.Consumers, log),
		own:         initialVerdict(),
		verdict:     initialVerdict(),
		whenQuiet:   map[string]func(){},
		quietLogged: map[string]bool{},
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	w.traffic = newTraffic(cfg.Keys, func() time.Time { return w.now() })
	w.setComfyUI(cfg.ComfyUIURL)
	w.comfy = ComfyIdle{BusyAt: w.now()}
	return w
}

func (w *Warden) setComfyUI(url string) {
	w.comfyQueue, w.comfyFree = nil, nil
	if url != "" {
		w.comfyQueue = func() *int { return comfyUIQueueDepth(url) }
		w.comfyFree = func() error { return comfyUIFree(url) }
	}
}

func (w *Warden) config() Config {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg
}

// Start runs the loop. The loop runs even with the policy disabled, since a
// reload may enable it and a deferred reload still has to happen; a disabled
// tick measures nothing and announces nothing.
func (w *Warden) Start() {
	w.logPolicy("Warden")
	go w.run()
}

func (w *Warden) logPolicy(prefix string) {
	cfg := w.config()
	p := cfg.Policy
	if !p.Enabled {
		w.log.Infof("%s policy disabled: measuring on request only, announcing nothing", prefix)
		return
	}
	names := make([]string, 0, len(cfg.Consumers))
	for _, c := range cfg.Consumers {
		names = append(names, c.Name)
	}
	w.log.Infof("%s watching every %.0fs: busy >= %.0f%%, resume after %ds quiet, no unload within %ds of an interactive request, consumers: %s, ComfyUI: %s",
		prefix, p.PollSeconds, p.GPUBusyPercent, p.ResumeQuietSeconds, p.InteractiveRecentSeconds,
		orNone(strings.Join(names, ", ")), orNone(cfg.ComfyUIURL))
}

func (w *Warden) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

func (w *Warden) run() {
	defer close(w.done)
	for {
		started := time.Now()
		w.safeTick()
		interval := time.Duration(w.config().Policy.PollSeconds * float64(time.Second))
		wait := max(time.Second, interval-time.Since(started))
		select {
		case <-w.stop:
			return
		case <-time.After(wait):
		}
	}
}

// safeTick keeps a panicking tick from being the last one.
func (w *Warden) safeTick() {
	defer func() {
		if r := recover(); r != nil {
			w.log.Warnf("Warden tick failed: %v", r)
		}
	}()
	w.Tick()
}

// Tick measures once, decides, acts on the models and ComfyUI, and tells
// whoever needs telling.
//
// A failed measurement leaves the verdict alone rather than reading as quiet:
// a probe that cannot run is not evidence that the GPU is free.
func (w *Warden) Tick() Verdict {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	defer w.runWhenQuiet()

	cfg := w.config()
	if !cfg.Policy.Enabled {
		return w.disabledTick()
	}

	res, err := w.probe()
	if err != nil {
		detail := err.Error()
		w.mu.Lock()
		w.probeError = &detail
		v := w.verdict
		w.mu.Unlock()
		w.log.Warnf("Warden probe failed, verdict unchanged: %s", detail)
		return v
	}

	now := w.now()
	w.mu.Lock()
	queue := w.comfyQueue
	w.mu.Unlock()
	if queue != nil {
		res.ComfyUIJobs = queue()
		w.comfyTick(res.ComfyUIJobs, now)
	}
	loaded := len(w.models.Running()) > 0

	w.mu.Lock()
	w.probeError = nil
	w.lastResources = &res
	w.own = Decide(w.own, res, now, loaded, cfg.Policy)
	before := w.verdict
	after := w.effectiveLocked()
	w.verdict = after
	w.mu.Unlock()

	w.act(before, after)
	return after
}

// effectiveLocked is the verdict to act on: the user's, until the warden's own
// decision moves away from what it was when the user set it.
func (w *Warden) effectiveLocked() Verdict {
	if w.manual == nil {
		return w.own
	}
	if w.own.Action() != w.manual.OwnAction {
		w.log.Infof("Manual %s ended: the warden decided %s - %s", w.manual.Action, w.own.Action(), w.own.Reason)
		w.manual = nil
		return w.own
	}
	return w.manual.verdict()
}

// act carries out a change of verdict and keeps the consumers told.
func (w *Warden) act(before, after Verdict) {
	if before.Yielded != after.Yielded {
		w.log.Infof("Verdict: %s - %s", strings.ToUpper(after.Action()), after.Reason)
	}
	switch {
	case after.Yielded && !before.Yielded:
		if n := w.traffic.pause(after.Reason); n > 0 {
			w.log.Infof("Cancelled %d batch request(s)", n)
		}
		// The unload is owed once per yield, on the transition (0002). It may
		// wait for an interactive session to go quiet (0004), but a model a
		// client loads again during the pause stays loaded.
		w.mu.Lock()
		w.pendingUnload, w.deferLogged = true, false
		w.mu.Unlock()
	case !after.Yielded && before.Yielded:
		w.traffic.resume()
		w.mu.Lock()
		w.pendingUnload = false
		w.mu.Unlock()
	case after.Priority && !before.Priority:
		// A priority process arriving during a pause owes an unload of its
		// own: a model loaded again since the first one holds the VRAM it
		// needs (0017).
		w.log.Infof("Priority: %s", after.Reason)
		w.mu.Lock()
		w.pendingUnload, w.deferLogged = true, false
		w.mu.Unlock()
	}
	w.settleUnload()

	w.announcer.sync(after)
}

// disabledTick measures nothing. Consumers a pause was announced to are told
// to resume once, since nothing will ever lift it otherwise.
func (w *Warden) disabledTick() Verdict {
	w.mu.Lock()
	before := w.verdict
	w.own, w.manual = initialVerdict(), nil
	after := Verdict{Reason: "the warden is disabled"}
	w.verdict = after
	w.mu.Unlock()
	if before.Yielded {
		w.act(before, after)
	}
	return after
}

// settleUnload pays an owed unload unless the user is in the middle of an
// interactive session. For a priority process only a request in flight counts
// as that (0017).
func (w *Warden) settleUnload() {
	w.mu.Lock()
	pending, priority := w.pendingUnload, w.verdict.Priority
	w.mu.Unlock()
	if !pending {
		return
	}
	window := time.Duration(w.config().Policy.InteractiveRecentSeconds) * time.Second
	if priority {
		window = 0
	}
	if recent, last := w.traffic.interactiveRecent(window); recent {
		w.mu.Lock()
		logged := w.deferLogged
		w.deferLogged = true
		w.mu.Unlock()
		if !logged {
			w.log.Infof("Unload deferred: interactive request %s", describeLast(last, w.now()))
		}
		return
	}
	w.mu.Lock()
	reason := w.verdict.Reason
	w.mu.Unlock()
	w.closeSessions("infermux: the GPU was yielded: " + reason)
	unloaded := w.models.UnloadAll()
	w.mu.Lock()
	w.pendingUnload = false
	w.lastUnload = unloaded
	w.mu.Unlock()
	w.log.Infof("Models unloaded: %s", orNone(strings.Join(unloaded, ", ")))
}

// closeSessions ends the open sessions with 1013 before their models stop,
// so a client learns why rather than seeing its connection drop (0018, 7).
// Every one is idle by now: one that moves data defers the unload.
func (w *Warden) closeSessions(reason string) {
	if n := w.traffic.closeSessions(stream.TryAgainLater, reason); n > 0 {
		w.log.Infof("Closed %d idle session(s): %s", n, reason)
	}
}

// CloseSessions ends every open session with code and reason, for a config
// reload, which stops every model (0018, 7).
func (w *Warden) CloseSessions(code int, reason string) int {
	return w.traffic.closeSessions(code, reason)
}

func describeLast(last, now time.Time) string {
	if last.IsZero() {
		return "in flight"
	}
	return fmt.Sprintf("in flight or %.0fs ago", now.Sub(last).Seconds())
}

func (w *Warden) comfyTick(jobs *int, now time.Time) {
	w.mu.Lock()
	previous, free, policy := w.comfy, w.comfyFree, w.cfg.Policy
	w.mu.Unlock()
	after, due := ComfyUIFreeDue(previous, jobs, now, policy)
	if due {
		if err := free(); err != nil {
			// Not marked freed, so the next tick tries again.
			w.log.Warnf("ComfyUI did not take /free: %v", err)
			return
		}
		w.log.Infof("ComfyUI idle for %ds: models freed", policy.ComfyUIIdleSeconds)
	}
	w.mu.Lock()
	w.comfy = after
	w.mu.Unlock()
}

// Wrap puts the warden in front of llama-swap: it answers /warden/*, refuses
// batch inference during a pause, and tracks every inference request until its
// response has been written.
func (w *Warden) Wrap(next http.Handler) http.Handler {
	api := w.api()
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if refused := w.guard(r); refused != "" {
			writeJSON(rw, http.StatusForbidden, map[string]string{"detail": refused})
			return
		}
		key, required, known := w.traffic.identify(r)
		if required && !known && !keyless(r) {
			rw.Header().Set("WWW-Authenticate", `Basic realm="InferMux"`)
			writeJSON(rw, http.StatusUnauthorized, map[string]string{"detail": "a key from keys.yaml is needed"})
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), keyContext{}, key))
		if strings.HasPrefix(r.URL.Path, "/warden/") {
			api.ServeHTTP(rw, r)
			return
		}
		if !isInference(r) {
			next.ServeHTTP(rw, r)
			return
		}
		model, local, known := w.models.Qualify(r)
		if key.Allow != nil && (!known || !key.allows(model)) {
			if !known {
				model = "an unknown model"
			}
			writeJSON(rw, http.StatusForbidden, map[string]string{"detail": "key " + key.name + " may not use " + model})
			return
		}
		// A stuck agent is only looked for on this host's models (0009, 0015).
		if known && local {
			var done bool
			if r, model, local, done = w.unstick(rw, r, key, model); done {
				return
			}
		}
		// The gate is for this host's card (0009). Another host's model is
		// gated by that host's warden, and a cloud peer's uses no card here, so
		// neither is paused, cancelled or counted as interactive traffic. A
		// model nobody knows is gated: it fails in llama-swap either way.
		if known && !local {
			next.ServeHTTP(rw, r)
			return
		}
		class := key.Class
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		// A WebSocket becomes a session at its upgrade, followed frame by
		// frame from then on (0018, 6).
		var session *stream.Session
		if isWebSocket(r) {
			session = stream.New(w.now(), w.now)
			ctx = stream.With(ctx, session)
		}
		id, ok := w.traffic.begin(class, r.URL.Path, func() { cancel(errYielded) }, session)
		if !ok {
			w.refuse(rw, "batch requests wait while the GPU is yielded: ")
			return
		}
		defer w.runWhenQuiet()
		defer w.traffic.end(id)
		tracked := &trackedWriter{ResponseWriter: rw, session: session}
		next.ServeHTTP(tracked, r.WithContext(ctx))
		if context.Cause(ctx) != errYielded || tracked.hijacked {
			// A cancelled session has had its close frame (0018, 7).
			return
		}
		// llama-swap returns without a word when the request's context ends,
		// and Go would send that as an empty 200. A cancelled batch request
		// has to read as a failure to the client that sent it.
		if !tracked.started {
			w.refuse(rw, "batch request cancelled, the GPU was yielded: ")
			return
		}
		// Mid-stream: the status is gone, so break the connection rather than
		// end the stream cleanly.
		panic(http.ErrAbortHandler)
	})
}

var errYielded = errors.New("the GPU was yielded")

func (w *Warden) refuse(rw http.ResponseWriter, why string) {
	w.mu.Lock()
	reason := w.verdict.Reason
	retry := w.cfg.Policy.ResumeQuietSeconds
	w.mu.Unlock()
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Retry-After", strconv.Itoa(retry))
	rw.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(rw).Encode(map[string]any{
		"error": map[string]string{
			"type":    "gpu_yielded",
			"message": why + reason,
		},
	})
}

// trackedWriter remembers whether the response has started. It passes Flush
// and Hijack through, since llama-swap's streaming asserts both, and hands a
// WebSocket's connection to its session.
type trackedWriter struct {
	http.ResponseWriter
	started  bool
	hijacked bool
	session  *stream.Session
}

func (t *trackedWriter) WriteHeader(code int) {
	t.started = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackedWriter) Write(b []byte) (int, error) {
	t.started = true
	return t.ResponseWriter.Write(b)
}

func (t *trackedWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		t.started = true
		f.Flush()
	}
}

func (t *trackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("%T cannot be hijacked", t.ResponseWriter)
	}
	t.started = true
	conn, brw, err := hj.Hijack()
	if err != nil {
		return conn, brw, err
	}
	t.hijacked = true
	if t.session != nil {
		conn = t.session.Attach(conn)
	}
	return conn, brw, nil
}

func (t *trackedWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// VerdictState is /warden/verdict: what the warden believes, who has heard
// it, and what it saw. Cheap: it reports the last tick rather than measuring.
type VerdictState struct {
	Enabled bool    `json:"enabled"`
	Verdict Verdict `json:"verdict"`
	Action  string  `json:"action"`
	// Own is what the measurements say. It differs from Verdict while a
	// manual verdict holds.
	Own           Verdict                  `json:"own"`
	Manual        *Manual                  `json:"manual"`
	WaitingQuiet  []string                 `json:"waiting_for_quiet"`
	Policy        Policy                   `json:"policy"`
	Consumers     map[string]ConsumerState `json:"consumers"`
	Traffic       TrafficState             `json:"traffic"`
	Models        map[string]string        `json:"models"`
	PendingUnload bool                     `json:"pending_unload"`
	LastUnload    []string                 `json:"last_unload"`
	ComfyUI       *comfyState              `json:"comfyui"`
	ProbeError    *string                  `json:"probe_error"`
	Resources     *Resources               `json:"resources"`
	// Remotes is the other hosts and what they list (0006, 2).
	Remotes any `json:"remotes"`
}

type comfyState struct {
	URL string `json:"url"`
	ComfyIdle
}

func (w *Warden) State() VerdictState {
	models := w.models.Running()
	w.mu.Lock()
	defer w.mu.Unlock()
	s := VerdictState{
		Enabled:       w.cfg.Policy.Enabled,
		Verdict:       w.verdict,
		Action:        w.verdict.Action(),
		Own:           w.own,
		Manual:        w.manual,
		WaitingQuiet:  w.waitingLocked(),
		Policy:        w.cfg.Policy,
		Consumers:     w.announcer.state(),
		Traffic:       w.traffic.state(),
		Models:        models,
		PendingUnload: w.pendingUnload,
		LastUnload:    w.lastUnload,
		ProbeError:    w.probeError,
		Resources:     w.lastResources,
	}
	if w.cfg.ComfyUIURL != "" {
		s.ComfyUI = &comfyState{URL: w.cfg.ComfyUIURL, ComfyIdle: w.comfy}
	}
	if w.remotes != nil {
		s.Remotes = w.remotes()
	}
	return s
}

// ReportRemotes adds the other hosts to /warden/verdict. The warden only
// shows them; routing to them is the remote package's.
func (w *Warden) ReportRemotes(state func() any) {
	w.mu.Lock()
	w.remotes = state
	w.mu.Unlock()
}

// Handle adds a route under /warden/, behind the same key check. Call it
// before Wrap.
func (w *Warden) Handle(pattern string, h http.Handler) {
	w.extra = append(w.extra, route{pattern, h})
}

type route struct {
	pattern string
	handler http.Handler
}

func (w *Warden) api() http.Handler {
	mux := http.NewServeMux()
	for _, r := range w.extra {
		mux.Handle(r.pattern, r.handler)
	}
	mux.HandleFunc("GET /warden/verdict", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, http.StatusOK, w.State())
	})
	// A fresh measurement, on purpose, from a probe of its own so it does not
	// move the loop's utilization window.
	mux.HandleFunc("GET /warden/resources", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		probe := w.freshProbe
		w.mu.Unlock()
		res, err := probe()
		if err != nil {
			writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"detail": "probe failed: " + err.Error()})
			return
		}
		writeJSON(rw, http.StatusOK, res)
	})
	w.controls(mux)
	return mux
}

func writeJSON(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	enc := json.NewEncoder(rw)
	enc.SetIndent("", "  ")
	enc.Encode(body)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
