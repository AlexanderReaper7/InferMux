// Package adapter puts an InferMux key on the requests of a client that cannot
// send one (0013). Immich's ML URL takes no headers, so an adapter listens
// where Immich can reach it and forwards to the daemon with Immich's key.
// One instance per such client.
package adapter

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// New forwards every request to target with key as its bearer token, in place
// of any credentials the client sent. A request path is appended to target's
// path: with target http://127.0.0.1:5001/upstream/immich-ml, /predict goes to
// /upstream/immich-ml/predict. A path in routes goes to the path it maps to
// on target's host instead, which is how Immich's /ping reaches the daemon's
// /health rather than starting the model.
func New(target *url.URL, key string, routes map[string]string) http.Handler {
	base := strings.TrimSuffix(target.Path, "/")
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = target.Scheme
			out.URL.Host = target.Host
			out.Host = target.Host
			if to, ok := routes[pr.In.URL.Path]; ok {
				out.URL.Path, out.URL.RawPath = to, ""
			} else {
				out.URL.Path, out.URL.RawPath = base+pr.In.URL.Path, ""
			}
			out.Header.Del("X-Api-Key")
			out.Header.Set("Authorization", "Bearer "+key)
		},
		// Streamed replies pass through as they come.
		FlushInterval: -1,
	}
}
