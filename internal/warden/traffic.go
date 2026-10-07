package warden

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/stream"
)

// Who is asking (0004). The class comes with the client's key in keys.yaml
// (0006): batch is work that can wait, or not happen at all, like Episteme's
// scheduled jobs; interactive is a prompt the user just sent, never the thing
// to kill. Without a keys file every request is interactive.
//
// Batch requests are refused with 503 while the verdict is pause, and cancelled
// on a yield. Interactive requests always pass, and the models are not unloaded
// while one is in flight or was recent.
//
// A WebSocket is a request in flight until its upgrade, and from then on a
// session, in flight only while data moves (0018, 6): its last data frame is
// what counts toward interactive_recent_seconds, not its being open.

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
	// session is a WebSocket's, nil for any other request.
	session *stream.Session
}

// inFlight is whether f counts as a request in flight at now: a request, a
// WebSocket before its upgrade, or a session that moved data within
// stream.ActiveWindow. last is an idle session's last data.
func (f *flight) inFlight(now time.Time) (yes bool, last time.Time) {
	if f.session == nil {
		return true, time.Time{}
	}
	moving, upgraded := f.session.Moving(now)
	if !upgraded || moving {
		return true, time.Time{}
	}
	last, _ = f.session.LastData()
	return false, last
}

// FlightState is one in-flight request as /warden/verdict reports it.
type FlightState struct {
	Class      Class   `json:"class"`
	Path       string  `json:"path"`
	AgeSeconds float64 `json:"age_seconds"`
	// Session is set for an upgraded WebSocket.
	Session *SessionState `json:"session,omitempty"`
}

// SessionState is an open WebSocket: whether it counts as in flight, and how
// long since its last data frame.
type SessionState struct {
	Moving      bool    `json:"moving"`
	IdleSeconds float64 `json:"idle_seconds"`
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
	keys *keyring // nil: no keys file, no key needed
	now  func() time.Time

	mu              sync.Mutex
	paused          bool
	next            uint64
	flights         map[uint64]*flight
	lastInteractive time.Time
	refused         int
	cancelled       int
}

func newTraffic(keys *KeyFile, now func() time.Time) *traffic {
	t := &traffic{now: now, flights: map[uint64]*flight{}}
	t.setKeys(keys)
	return t
}

func (t *traffic) setKeys(keys *KeyFile) {
	var ring *keyring
	if keys != nil {
		ring = newKeyring(*keys)
	}
	t.mu.Lock()
	t.keys = ring
	t.mu.Unlock()
}

// identify finds the client. required is false without a keys file, and the
// request is then an unnamed interactive one.
func (t *traffic) identify(r *http.Request) (key namedKey, required, ok bool) {
	t.mu.Lock()
	ring := t.keys
	t.mu.Unlock()
	if ring == nil {
		return namedKey{Key: Key{Class: Interactive}}, false, true
	}
	key, ok = ring.identify(r)
	return key, true, ok
}

// presentedKeys reads the key the way llama-swap accepts one: a bearer token,
// x-api-key, or the password of HTTP Basic; and from a browser's WebSocket,
// the subprotocol OpenAI's realtime clients use.
func presentedKeys(r *http.Request) []string {
	keys := websocketKeys(r)
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
// management API, and a WebSocket opened to a model, a GET that lasts as long
// as the session and counts only while data moves (0018). A GET of /v1/models
// is not activity, and a POST to /api/models/unload from a batch client must
// not be refused. Every WebSocket stream.Route sends to a model is one.
func isInference(r *http.Request) bool {
	if r.Method != http.MethodPost && !(r.Method == http.MethodGet && isWebSocket(r)) {
		return false
	}
	return !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/warden/")
}

// begin registers a request, or refuses it. The pause check and the
// registration share one lock with pause(), so a batch request is either
// refused or registered before the yield, never neither. session is a
// WebSocket's, nil for any other request.
func (t *traffic) begin(class Class, path string, cancel context.CancelFunc, session *stream.Session) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if class == Batch && t.paused {
		t.refused++
		return 0, false
	}
	t.next++
	now := t.now()
	t.flights[t.next] = &flight{class: class, path: path, started: now, cancel: cancel, session: session}
	if class == Interactive {
		t.lastInteractive = now
	}
	return t.next, true
}

// end unregisters a request. A request's end is interactive activity; a
// session's end is not, only its last data frame was (0018, 6).
func (t *traffic) end(id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.flights[id]
	if !ok {
		return
	}
	delete(t.flights, id)
	if f.class != Interactive {
		return
	}
	end := t.now()
	if f.session != nil {
		if last, upgraded := f.session.LastData(); upgraded {
			end = last
		}
	}
	if end.After(t.lastInteractive) {
		t.lastInteractive = end
	}
}

// pause refuses batch requests from now on and cancels the ones in flight,
// a session with a close frame that gives the reason. Returns how many were
// cancelled.
func (t *traffic) pause(reason string) int {
	t.mu.Lock()
	t.paused = true
	t.mu.Unlock()
	return t.cancelBatch("infermux: batch session cancelled, the GPU was yielded: " + reason)
}

// cancelBatch cancels the batch requests in flight, and closes a batch
// session with reason first. Returns how many. The set is taken under the
// lock and cancelled after it, since a close frame to a slow client can take
// up to a second.
func (t *traffic) cancelBatch(reason string) int {
	t.mu.Lock()
	var batch []*flight
	for _, f := range t.flights {
		if f.class == Batch {
			batch = append(batch, f)
		}
	}
	t.cancelled += len(batch)
	t.mu.Unlock()
	for _, f := range batch {
		if f.session != nil {
			f.session.Close(stream.TryAgainLater, reason)
		}
		f.cancel()
	}
	return len(batch)
}

// closeSessions closes every upgraded session with code and reason, before
// the models under them stop (0018, 7). Returns how many.
func (t *traffic) closeSessions(code int, reason string) int {
	t.mu.Lock()
	var sessions []*stream.Session
	for _, f := range t.flights {
		if f.session != nil {
			sessions = append(sessions, f.session)
		}
	}
	t.mu.Unlock()
	n := 0
	for _, s := range sessions {
		if s.Close(code, reason) {
			n++
		}
	}
	return n
}

func (t *traffic) resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = false
}

// interactiveInFlight counts the interactive requests in flight, an idle
// session not among them.
func (t *traffic) interactiveInFlight() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	n := 0
	for _, f := range t.flights {
		if yes, _ := f.inFlight(now); yes && f.class == Interactive {
			n++
		}
	}
	return n
}

// interactiveRecent is true while an interactive request is in flight, and for
// window after the last one ended or an idle session's last data.
func (t *traffic) interactiveRecent(window time.Duration) (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	latest := t.lastInteractive
	for _, f := range t.flights {
		if f.class != Interactive {
			continue
		}
		yes, last := f.inFlight(now)
		if yes {
			return true, t.lastInteractive
		}
		if last.After(latest) {
			latest = last
		}
	}
	if latest.IsZero() {
		return false, latest
	}
	return now.Sub(latest) < window, latest
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
	last := t.lastInteractive
	for _, f := range t.flights {
		fs := FlightState{Class: f.class, Path: f.path, AgeSeconds: round1(now.Sub(f.started).Seconds())}
		if f.session != nil {
			if data, upgraded := f.session.LastData(); upgraded {
				moving, _ := f.session.Moving(now)
				fs.Session = &SessionState{Moving: moving, IdleSeconds: round1(now.Sub(data).Seconds())}
				if f.class == Interactive && data.After(last) {
					last = data
				}
			}
		}
		s.InFlight = append(s.InFlight, fs)
	}
	if !last.IsZero() {
		s.LastInteractive = &last
	}
	return s
}
