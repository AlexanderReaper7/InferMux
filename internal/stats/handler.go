package stats

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Host is one host's kept requests, newest first, and their summary.
type Host struct {
	Host     string    `json:"host"`
	Requests []Request `json:"requests"`
	Models   []Summary `json:"models"`
	Error    string    `json:"error,omitempty"`
}

// Of is this recorder's Host.
func (rec *Recorder) Of(host string) Host {
	requests := rec.Recent()
	return Host{Host: host, Requests: requests, Models: Summarize(requests)}
}

// Handler answers GET /warden/requests with this host's requests, and with
// ?hosts=all every other host's after them, each read from that host as it
// is now. A host that does not answer is listed with its error.
func Handler(rec *Recorder, host string, others func(ctx context.Context) []Host) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hosts := []Host{rec.Of(host)}
		if r.URL.Query().Get("hosts") == "all" && others != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			hosts = append(hosts, others(ctx)...)
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]any{"hosts": hosts})
	})
}
