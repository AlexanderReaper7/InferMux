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

	mu     sync.Mutex
	places map[string][]string
}

// New fails over in front of next. host is this host's name, which in a
// place means a local model.
func New(next http.Handler, host string, log Logger) *Handler {
	return &Handler{next: next, host: host, log: log}
}

// Set replaces the table, as failover.yaml is reloaded.
func (h *Handler) Set(places map[string][]string) {
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
	// Every attempt reads the body from the start.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		swaputil.SendResponse(rw, r, http.StatusBadRequest, "could not read the request body")
		return
	}
	for i, place := range places {
		name := h.name(place, model)
		attempt := r.Clone(r.Context())
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		attempt.ContentLength = int64(len(body))
		if name != model {
			if attempt, err = swaputil.ReplaceRequestModel(attempt, model, name); err != nil {
				swaputil.SendResponse(rw, r, http.StatusBadRequest, err.Error())
				return
			}
		}
		if i == len(places)-1 {
			h.next.ServeHTTP(rw, attempt)
			return
		}
		held := &heldWriter{rw: rw, header: http.Header{}}
		h.next.ServeHTTP(held, attempt)
		if !held.failed {
			return
		}
		h.log.Warnf("Failover: %s answered %d, trying %s", name, held.status, h.name(places[i+1], model))
	}
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
	if failsOver(status) {
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
