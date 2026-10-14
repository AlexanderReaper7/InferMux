package failover

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
)

// NotStartedHeader is on llama-swap's own 500 for a request it never sent to
// a model: the model did not start, or the router was shutting down. Mark
// sets it on the host that runs llama-swap, and the failover handler of the
// host the client asked fails over on it (0016, 3).
const NotStartedHeader = "X-InferMux-Not-Started"

// Mark wraps llama-swap's handler. A 500 written before llama-swap sent the
// request's headers to any upstream, which the request's httptrace hooks
// see, gets NotStartedHeader. A model's answer cannot carry an InferMux
// header: every X-InferMux- header is put back as it was when the request
// entered llama-swap.
func Mark(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &markWriter{ResponseWriter: rw, ours: ours(rw.Header())}
		trace := &httptrace.ClientTrace{WroteHeaders: func() { w.sent.Store(true) }}
		next.ServeHTTP(w, r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
	})
}

// isOurs is a header InferMux sets, whatever its case in the map.
func isOurs(key string) bool {
	const prefix = "X-InferMux-"
	return len(key) >= len(prefix) && strings.EqualFold(key[:len(prefix)], prefix)
}

func ours(h http.Header) http.Header {
	kept := http.Header{}
	for k, v := range h {
		if isOurs(k) {
			kept[k] = v
		}
	}
	return kept
}

type markWriter struct {
	http.ResponseWriter
	ours  http.Header
	sent  atomic.Bool
	wrote bool
}

func (w *markWriter) WriteHeader(status int) {
	if !w.wrote {
		h := w.Header()
		for k := range h {
			if isOurs(k) {
				delete(h, k)
			}
		}
		for k, v := range w.ours {
			h[k] = v
		}
		// A 1xx is followed by the answer's own status.
		if status >= 200 || status == http.StatusSwitchingProtocols {
			w.wrote = true
			if status == http.StatusInternalServerError && !w.sent.Load() {
				h.Set(NotStartedHeader, "true")
			}
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *markWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *markWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *markWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *markWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
