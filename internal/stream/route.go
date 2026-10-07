package stream

import (
	"net/http"
	"net/url"
	"strings"
)

// Route sends a WebSocket upgrade that names a model in ?model= to that
// model's server, at the path and with the query the client sent (0018, 5):
// /v1/realtime?model=x reaches x's server as /v1/realtime?model=x, the way
// llama-swap's /upstream/x/v1/realtime would. It sits just in front of
// llama-swap, so the warden, the stats and the remote router have read the
// model from the query before it.
func Route(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		model := Routed(r)
		if model == "" {
			next.ServeHTTP(rw, r)
			return
		}
		u := *r.URL
		prefix := "/upstream/" + model
		u.Path = prefix + r.URL.Path
		if r.URL.RawPath != "" {
			u.RawPath = (&url.URL{Path: prefix}).EscapedPath() + r.URL.RawPath
		}
		out := r.WithContext(r.Context())
		out.URL = &u
		next.ServeHTTP(rw, out)
	})
}

// Routed is the model Route sends r to, or "". llama-swap's own paths keep
// their meaning: /upstream/ and /comfyui/ name the model in the path, and
// the warden's and llama-swap's API are not a model's.
func Routed(r *http.Request) string {
	if r.Method != http.MethodGet || !Upgrade(r) {
		return ""
	}
	for _, own := range []string{"/upstream/", "/comfyui", "/api/", "/warden/"} {
		if strings.HasPrefix(r.URL.Path, own) {
			return ""
		}
	}
	return r.URL.Query().Get("model")
}

// Upgrade is a request to switch the connection to a WebSocket, with both
// headers the switch needs.
func Upgrade(r *http.Request) bool {
	return hasToken(r.Header.Values("Connection"), "upgrade") && hasToken(r.Header.Values("Upgrade"), "websocket")
}

func hasToken(values []string, token string) bool {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
