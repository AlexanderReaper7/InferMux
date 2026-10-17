package warden

import (
	"encoding/base64"
	"net/http"
	"slices"
	"sort"
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
// while one is in flight or was recent. The exception is a safety stop: while
// the local models are off, every request is refused and cancelled (0025).
//
// Each request is on the devices its model occupies (0022): a batch request
// is refused when any of them is yielded, and cancelled when one yields. The
// interactive quiet an unload waits for is the quiet on that device.
//
// A WebSocket is a request in flight until its upgrade, and from then on a
// session, in flight only while data moves (0018, 6): its last data frame is
// what counts toward interactive_recent_seconds, not its being open.

type Class string

const (
	Interactive Class = "interactive"
	Batch       Class = "batch"
)

// The warden's devices: one GPU, NVML's first, and the CPU (0022).
const (
	DeviceGPU = "gpu"
	DeviceCPU = "cpu"
)

// deviceName is a device as a log line or a refusal names it.
func deviceName(device string) string {
	switch device {
	case DeviceGPU:
		return "the GPU"
	case DeviceCPU:
		return "the CPU"
	}
	return "every device"
}

type flight struct {
	class   Class
	path    string
	started time.Time
	devices []string
	// cancel ends the request; device is the one that yielded, "" for a
	// cancel by hand, and why says so.
	cancel func(device, why string)
	// session is a WebSocket's, nil for any other request.
	session *stream.Session
	// comfy is a request to ComfyUI (0030). It is refused and cancelled as
	// any other, but is not interactive activity on its device: ComfyUI is
	// never unloaded under it, and its work is what the GPU's other models
	// make room for.
	comfy bool
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
	Class      Class    `json:"class"`
	Path       string   `json:"path"`
	AgeSeconds float64  `json:"age_seconds"`
	Devices    []string `json:"devices"`
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
	// Paused is the GPU's; PausedDevices every device that refuses batch.
	Paused          bool          `json:"paused"`
	PausedDevices   []string      `json:"paused_devices"`
	InFlight        []FlightState `json:"in_flight"`
	LastInteractive *time.Time    `json:"last_interactive"`
	RefusedBatch    int           `json:"refused_batch"`
	CancelledBatch  int           `json:"cancelled_batch"`
}

type traffic struct {
	keys *keyring // nil: no keys file, no key needed
	now  func() time.Time

	mu              sync.Mutex
	paused          map[string]bool
	next            uint64
	flights         map[uint64]*flight
	lastInteractive time.Time
	lastOn          map[string]time.Time
	refused         int
	cancelled       int
	resumed         []func()
	// off is why every request is refused, "" while the models are on (0025).
	off string
}

func newTraffic(keys *KeyFile, now func() time.Time) *traffic {
	t := &traffic{now: now, flights: map[uint64]*flight{}, paused: map[string]bool{}, lastOn: map[string]time.Time{}}
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

// begin registers a request on devices, or refuses it and names the yielded
// device. The pause check and the registration share one lock with pause(),
// so a batch request is either refused or registered before the yield, never
// neither. session is a WebSocket's, nil for any other request.
func (t *traffic) begin(class Class, path string, devices []string, cancel func(device, why string), session *stream.Session) (uint64, string) {
	return t.add(&flight{class: class, path: path, devices: devices, cancel: cancel, session: session})
}

// beginComfyUI is begin for a request to ComfyUI (flight.comfy).
func (t *traffic) beginComfyUI(class Class, path string, devices []string, cancel func(device, why string)) (uint64, string) {
	return t.add(&flight{class: class, path: path, devices: devices, cancel: cancel, comfy: true})
}

func (t *traffic) add(f *flight) (uint64, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.off != "" {
		if f.class == Batch {
			t.refused++
		}
		return 0, offDevice
	}
	if f.class == Batch {
		for _, d := range f.devices {
			if t.paused[d] {
				t.refused++
				return 0, d
			}
		}
	}
	t.next++
	now := t.now()
	f.started = now
	t.flights[t.next] = f
	if f.class == Interactive && !f.comfy {
		t.lastInteractive = now
		for _, d := range f.devices {
			t.lastOn[d] = now
		}
	}
	return t.next, ""
}

// on is whether f is on device; every flight is on "".
func (f *flight) on(device string) bool {
	return device == "" || slices.Contains(f.devices, device)
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
	if f.class != Interactive || f.comfy {
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
	for _, d := range f.devices {
		if end.After(t.lastOn[d]) {
			t.lastOn[d] = end
		}
	}
}

// pause refuses batch requests on device from now on and cancels the ones in
// flight, a session with a close frame that gives the reason. Returns how
// many were cancelled.
func (t *traffic) pause(device, reason string) int {
	t.mu.Lock()
	t.paused[device] = true
	t.mu.Unlock()
	return t.cancelBatch(device, deviceName(device)+" was yielded: "+reason)
}

// cancelBatch cancels the batch requests in flight on device, "" for every
// one by hand, and closes a batch session first. why is the yield's reason,
// or "cancelled by hand". Returns how many. The set is taken under the lock
// and cancelled after it, since a close frame to a slow client can take up
// to a second.
func (t *traffic) cancelBatch(device, why string) int {
	t.mu.Lock()
	var batch []*flight
	for _, f := range t.flights {
		if f.class == Batch && f.on(device) {
			batch = append(batch, f)
		}
	}
	t.cancelled += len(batch)
	t.mu.Unlock()
	reason := "infermux: batch session cancelled, " + why
	if device == "" {
		reason = "infermux: batch session " + why
	}
	for _, f := range batch {
		if f.session != nil {
			f.session.Close(stream.TryAgainLater, reason)
		}
		f.cancel(device, why)
	}
	return len(batch)
}

// cutOn cancels every request in flight on device, whatever its class, for
// an unload by hand or by infermux-hold (0024, 4); a job's request is paused.
// The caller closes the sessions first. Returns how many.
func (t *traffic) cutOn(device, why string) int {
	t.mu.Lock()
	var on []*flight
	for _, f := range t.flights {
		if f.on(device) {
			on = append(on, f)
		}
	}
	t.mu.Unlock()
	for _, f := range on {
		f.cancel(heldDevice, why)
	}
	return len(on)
}

// closeSessions closes every upgraded session on device, "" for all, with
// code and reason, before the models under them stop (0018, 7). Returns how
// many.
func (t *traffic) closeSessions(device string, code int, reason string) int {
	t.mu.Lock()
	var sessions []*stream.Session
	for _, f := range t.flights {
		if f.session != nil && f.on(device) {
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

// setOff refuses every request from now on and cancels every one in flight,
// interactive too, a session with a close frame; why is the reason, "" lets
// them through again and wakes whoever waits for a resume. Returns how many
// were cancelled.
func (t *traffic) setOff(why string) int {
	t.mu.Lock()
	was := t.off
	t.off = why
	var all []*flight
	if was == "" && why != "" {
		for _, f := range t.flights {
			all = append(all, f)
			if f.class == Batch {
				t.cancelled++
			}
		}
	}
	resumed := t.resumed
	t.mu.Unlock()
	if why == "" {
		if was != "" {
			for _, f := range resumed {
				f()
			}
		}
		return 0
	}
	for _, f := range all {
		if f.session != nil {
			f.session.Close(stream.TryAgainLater, "infermux: the local models are off: "+why)
		}
		f.cancel(offDevice, "the local models are off: "+why)
	}
	return len(all)
}

func (t *traffic) resume(device string) {
	t.mu.Lock()
	delete(t.paused, device)
	resumed := t.resumed
	t.mu.Unlock()
	for _, f := range resumed {
		f()
	}
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

// interactiveRecent is true while an interactive request is in flight on
// device, "" for any, and for window after the last one there ended or an
// idle session's last data. A request to ComfyUI is none (flight.comfy).
func (t *traffic) interactiveRecent(window time.Duration, device string) (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	latest := t.lastInteractive
	if device != "" {
		latest = t.lastOn[device]
	}
	for _, f := range t.flights {
		if f.class != Interactive || f.comfy || !f.on(device) {
			continue
		}
		yes, last := f.inFlight(now)
		if yes {
			return true, latest
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
		Paused:         t.paused[DeviceGPU],
		PausedDevices:  []string{},
		InFlight:       []FlightState{},
		RefusedBatch:   t.refused,
		CancelledBatch: t.cancelled,
	}
	last := t.lastInteractive
	for _, f := range t.flights {
		fs := FlightState{Class: f.class, Path: f.path, AgeSeconds: round1(now.Sub(f.started).Seconds()), Devices: f.devices}
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
	for d := range t.paused {
		s.PausedDevices = append(s.PausedDevices, d)
	}
	sort.Strings(s.PausedDevices)
	return s
}
