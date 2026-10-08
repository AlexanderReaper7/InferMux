// Package failover sends a request for a model to the first place in its
// list that answers (0016). It is the outermost handler: each attempt goes
// through the whole stack with the model renamed to that place, zbox/<model>
// or a local name, so the warden gates and checks the allow list of each
// attempt as of a request for that name, and a local fallback is gated by
// this host's card as any local request is (0009).
//
// An attempt that is not the last one fails over when it answers 502, 503 or
// 504: the place is offline or unreachable (remote answers 502 for both), its
// model would not start, its warden refused, or it timed out. Nothing of that
// answer reaches the client. Any other answer is the client's.
package failover

import (
	"bufio"
	"bytes"
	"io"
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

// Place is a destination and its optional overflow policy.
type Place struct {
	Place       string
	MaxInflight int
	OnlyIfIdle  bool
	BatchOnly   bool
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
	var queue *Place
	for i, place := range eligible {
		attempt, err := attemptFor(place)
		if err != nil {
			swaputil.SendResponse(rw, r, http.StatusBadRequest, err.Error())
			return
		}
		held := &heldWriter{rw: rw, header: http.Header{}, final: i == len(eligible)-1}
		h.next.ServeHTTP(held, attempt)
		if !held.failed {
			return
		}
		if queue == nil && held.header.Get(BusyHeader) == "capacity" && !place.OnlyIfIdle {
			copy := place
			queue = &copy
		}
		if i+1 < len(eligible) {
			h.log.Warnf("Failover: %s answered %d, trying %s", h.name(place.Place, model), held.status, h.name(eligible[i+1].Place, model))
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
		h.next.ServeHTTP(rw, attempt)
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

// failsOver is an answer that says the place cannot serve the request now.
func failsOver(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// heldWriter holds an attempt's headers until its status is known. A status
// that fails over is dropped with its body; any other is written to the
// client, and from then on everything passes straight through, a stream
// chunk by chunk.
type heldWriter struct {
	rw        http.ResponseWriter
	header    http.Header
	status    int
	failed    bool
	committed bool
	final     bool
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
	w.status = status
	if (!w.final && failsOver(status)) || (status == http.StatusServiceUnavailable && w.header.Get(BusyHeader) != "") {
		w.failed = true
		return
	}
	w.commit()
	w.rw.WriteHeader(status)
}

func (w *heldWriter) commit() {
	for k, v := range w.header {
		w.rw.Header()[k] = v
	}
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
