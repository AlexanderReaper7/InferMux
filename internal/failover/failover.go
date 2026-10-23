// Package failover sends a request for a model to the first place in its
// list that answers (0016). It is the outermost handler: each attempt goes
// through the whole stack with the model renamed to that place, zbox/<model>
// or a local name, so the warden gates and checks the allow list of each
// attempt as of a request for that name, and a local fallback is gated by
// this host's card as any local request is (0009).
//
// An attempt that is not the last one fails over when it answers 502, 503 or
// 504: the place is offline or unreachable (remote answers 502 for both), its
// warden refused, or it timed out. It also fails over on a 500 that Mark
// marked as llama-swap's own, for a request no model had: the model would
// not start. Nothing of that answer reaches the client. Any other answer is
// the client's.
package failover

import (
	"bufio"
	"bytes"
	"io"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/remote"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Logger is the part of llama-swap's proxy log this writes to.
type Logger interface {
	Debugf(format string, args ...any)
	Warnf(format string, args ...any)
}

// Handler fails a request over between the places its model lists.
type Handler struct {
	next http.Handler
	host string
	log  Logger
	// Batch classifies the original client's key. Every attempt still passes
	// the destination's authentication, allow list and request gate.
	Batch func(*http.Request) bool

	mu     sync.Mutex
	places map[string][]Place
}

// Place is a destination and its optional overflow policy. A Failover
// place is tried only when no balance place served the request and none was
// merely at its limit (0022, 2).
type Place struct {
	Place       string
	MaxInflight int
	OnlyIfIdle  bool
	BatchOnly   bool
	Failover    bool
}

// New fails over in front of next. host is this host's name, which in a
// place means a local model.
func New(next http.Handler, host string, log Logger) *Handler {
	return &Handler{next: next, host: host, log: log}
}

// Set replaces the table, as failover.yaml is reloaded.
func (h *Handler) Set(places map[string][]Place) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.places = places
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// A forwarded request is served where it lands (0006, 2), and an
	// /upstream/ path names no model in a body to rename.
	if r.Header.Get(remote.HopHeader) != "" || strings.HasPrefix(r.URL.Path, "/upstream/") {
		h.next.ServeHTTP(rw, r)
		return
	}
	model, err := swaputil.ExtractModel(r)
	h.mu.Lock()
	places := h.places[model]
	h.mu.Unlock()
	if err != nil || len(places) == 0 {
		h.next.ServeHTTP(rw, r)
		return
	}
	if !inference(r) {
		h.next.ServeHTTP(rw, r)
		return
	}
	batch := h.Batch != nil && h.Batch(r)
	eligible := make([]Place, 0, len(places))
	for _, place := range places {
		if !place.BatchOnly || batch {
			eligible = append(eligible, place)
		}
	}
	if len(eligible) == 0 {
		h.next.ServeHTTP(rw, r)
		return
	}
	// Every attempt reads the body from the start.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		swaputil.SendResponse(rw, r, http.StatusBadRequest, "could not read the request body")
		return
	}
	attemptFor := func(place Place) (*http.Request, error) {
		name := h.name(place.Place, model)
		attempt := r.Clone(r.Context())
		attempt.Header.Del(MaxInflightHeader)
		attempt.Header.Del(OnlyIfIdleHeader)
		if place.MaxInflight > 0 {
			attempt.Header.Set(MaxInflightHeader, strconv.Itoa(place.MaxInflight))
		}
		if place.OnlyIfIdle {
			attempt.Header.Set(OnlyIfIdleHeader, "true")
		}
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		attempt.ContentLength = int64(len(body))
		if name != model {
			return swaputil.ReplaceRequestModel(attempt, model, name)
		}
		return attempt, nil
	}
	var balance, standby []Place
	for _, place := range eligible {
		if place.Failover {
			standby = append(standby, place)
		} else {
			balance = append(balance, place)
		}
	}
	// try attempts each place in order until one serves. The last place of
	// the request's last lane answers the client even with a failure, unless
	// a place before it was only at its limit: the request waits there
	// instead (0019). It returns the first such place.
	try := func(lane []Place, last bool, next []Place) (bool, *Place) {
		var queue *Place
		for i, place := range lane {
			attempt, err := attemptFor(place)
			if err != nil {
				swaputil.SendResponse(rw, r, http.StatusBadRequest, err.Error())
				return true, nil
			}
			held := &heldWriter{rw: rw, header: http.Header{}, place: h.where(place.Place, model), final: last && i == len(lane)-1 && queue == nil}
			h.next.ServeHTTP(held, attempt)
			if !held.failed {
				return true, nil
			}
			if queue == nil && held.header.Get(BusyHeader) == "capacity" && !place.OnlyIfIdle {
				copy := place
				queue = &copy
			}
			// A place at its limit is the balance working, not a failure,
			// and its host's /warden/requests counts each one (0019). So is
			// a place on a GPU the host does not have now, which its warden
			// logs once per absence (0032).
			logf := h.log.Warnf
			if held.header.Get(BusyHeader) == "capacity" || held.header.Get(AbsentHeader) != "" {
				logf = h.log.Debugf
			}
			switch {
			case i+1 < len(lane):
				logf("Failover: %s answered %d, trying %s", h.name(place.Place, model), held.status, h.name(lane[i+1].Place, model))
			case len(next) > 0 && queue == nil:
				logf("Failover: %s answered %d, the last balance place; trying the failover lane at %s", h.name(place.Place, model), held.status, h.name(next[0].Place, model))
			}
		}
		return false, queue
	}
	served, queue := try(balance, len(standby) == 0, standby)
	if served {
		return
	}
	// A balance place only at its limit is alive: the request waits there
	// rather than going to the failover lane, which is for places down or
	// refusing (0022, 2).
	if queue == nil && len(standby) > 0 {
		if served, queue = try(standby, true, nil); served {
			return
		}
	}
	// With no batch CPU destination, interactive work queues at the first
	// saturated place. A destination occupied by another model stays skipped.
	if queue != nil && r.Context().Err() == nil {
		queue.MaxInflight = 0
		attempt, err := attemptFor(*queue)
		if err != nil {
			swaputil.SendResponse(rw, r, http.StatusBadRequest, err.Error())
			return
		}
		h.next.ServeHTTP(&heldWriter{rw: rw, header: http.Header{}, place: h.where(queue.Place, model), queued: true}, attempt)
		return
	}
	rw.Header().Set("Retry-After", "1")
	swaputil.SendResponse(rw, r, http.StatusServiceUnavailable, "all destinations are busy")
}

// name is what a place calls the model: zbox/<model> on another host, the
// bare name here.
func (h *Handler) name(place, model string) string {
	host, other, qualified := strings.Cut(place, "/")
	if !qualified {
		other = model
	}
	if host == h.host {
		return other
	}
	return host + "/" + other
}

// where is a place's name for the model with its host, this host's too.
func (h *Handler) where(place, model string) string {
	name := h.name(place, model)
	if !strings.Contains(name, "/") {
		return h.host + "/" + name
	}
	return name
}

// failsOver is an answer that says the place cannot serve the request now:
// 502, 503, 504, or llama-swap's 500 for a request it never sent to a model.
func failsOver(status int, header http.Header) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusInternalServerError:
		return header.Get(NotStartedHeader) != ""
	}
	return false
}

// informational is a 1xx that another status follows. 101 is the last one.
func informational(status int) bool {
	return status >= 100 && status < 200 && status != http.StatusSwitchingProtocols
}

// heldWriter holds an attempt's headers until its status is known. A 1xx
// passes on to the client as it comes, and the status after it decides
// (RFC 9110, 15.2). A status that fails over is dropped with its body; any
// other is written to the client with the place that gave it, and from then
// on everything passes straight through, a stream chunk by chunk.
type heldWriter struct {
	rw        http.ResponseWriter
	header    http.Header
	place     string
	status    int
	failed    bool
	committed bool
	final     bool
	// queued is the request waiting at a place at its limit: its answer is
	// the client's, whatever it is.
	queued bool
}

func (w *heldWriter) Header() http.Header {
	if w.committed {
		return w.rw.Header()
	}
	return w.header
}

func (w *heldWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if informational(status) {
		w.inform(status)
		return
	}
	w.status = status
	if !w.queued && ((!w.final && failsOver(status, w.header)) || (status == http.StatusServiceUnavailable && w.header.Get(BusyHeader) != "")) {
		w.failed = true
		return
	}
	w.commit()
	w.rw.WriteHeader(status)
}

// inform sends a 1xx with its own headers and leaves the client's as they
// were for the answer. A ReverseProxy clears the attempt's headers after it,
// so the place is set at commit, not before.
func (w *heldWriter) inform(status int) {
	h := w.rw.Header()
	kept := h.Clone()
	maps.Copy(h, w.header)
	w.rw.WriteHeader(status)
	clear(h)
	maps.Copy(h, kept)
}

func (w *heldWriter) commit() {
	maps.Copy(w.rw.Header(), w.header)
	w.rw.Header().Set(PlaceHeader, w.place)
	w.committed = true
}

func (w *heldWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.failed {
		return len(b), nil
	}
	return w.rw.Write(b)
}

func (w *heldWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.rw.(http.Flusher); ok && !w.failed {
		f.Flush()
	}
}

// Hijack is a WebSocket taking the connection: the attempt is the answer.
func (w *heldWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.status = http.StatusSwitchingProtocols
	w.commit()
	return http.NewResponseController(w.rw).Hijack()
}

// WroteHeader is swaputil.StatusMarker, so a handler below knows the answer
// has started.
func (w *heldWriter) WroteHeader() bool { return w.status != 0 }

func (w *heldWriter) Unwrap() http.ResponseWriter { return w.rw }
