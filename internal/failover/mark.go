package failover

import (
	"bufio"
	"maps"
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
// entered llama-swap, and after the answer's headers have gone out, as they
// went, so no trailer of the model's is one.
func Mark(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &markWriter{ResponseWriter: rw, ours: ours(rw.Header())}
		trace := &httptrace.ClientTrace{WroteHeaders: func() { w.sent.Store(true) }}
		req := r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
		// The model is not asked for a 100 Continue. Go's server sends the
		// client its own when the body is first read, and llama-swap's
		// writers take a model's 1xx for the status and drop the one after
		// it (v264), so a model's 400 would reach the client as 200 (0016).
		if req.Header.Get("Expect") != "" {
			req.Header = req.Header.Clone()
			req.Header.Del("Expect")
		}
		next.ServeHTTP(w, req)
		restore(w.Header(), w.ours)
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

// restore puts h's InferMux headers back to kept. A trailer named as one is
// the model's, so it goes too, declared in Trailer or sent with net/http's
// TrailerPrefix.
func restore(h, kept http.Header) {
	for k := range h {
		if name, _ := strings.CutPrefix(k, http.TrailerPrefix); isOurs(name) {
			delete(h, k)
		}
	}
	if declared, ok := h["Trailer"]; ok {
		var names []string
		dropped := false
		for _, v := range declared {
			for name := range strings.SplitSeq(v, ",") {
				switch name = strings.TrimSpace(name); {
				case isOurs(name):
					dropped = true
				case name != "":
					names = append(names, name)
				}
			}
		}
		switch {
		case !dropped:
		case len(names) == 0:
			delete(h, "Trailer")
		default:
			h["Trailer"] = []string{strings.Join(names, ", ")}
		}
	}
	maps.Copy(h, kept)
}

type markWriter struct {
	http.ResponseWriter
	// ours is InferMux's headers as the request entered llama-swap, and from
	// the answer's status on, as they went out with it.
	ours  http.Header
	sent  atomic.Bool
	wrote bool
}

func (w *markWriter) WriteHeader(status int) {
	if w.wrote {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	h := w.Header()
	restore(h, w.ours)
	// A 1xx is followed by the answer's own status.
	if informational(status) {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.wrote = true
	if status == http.StatusInternalServerError && !w.sent.Load() {
		h.Set(NotStartedHeader, "true")
	}
	w.ResponseWriter.WriteHeader(status)
	// The writer below may have handed its headers on: read them where
	// they are now.
	w.ours = ours(w.Header())
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
