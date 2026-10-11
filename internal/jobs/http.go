package jobs

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mostlygeek/llama-swap/internal/warden"
)

// Handler serves /warden/jobs, behind the warden's key check, so a job
// belongs to the key that made it and another key's job is not found:
//
//	POST   /warden/jobs              {"path": "/v1/chat/completions", "body": {...}}
//	GET    /warden/jobs              the caller's jobs
//	GET    /warden/jobs/{id}
//	GET    /warden/jobs/{id}/events  server-sent events, from ?after=<seq> or Last-Event-ID
//	DELETE /warden/jobs/{id}
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /warden/jobs", s.create)
	mux.HandleFunc("GET /warden/jobs", s.list)
	mux.HandleFunc("GET /warden/jobs/{id}", s.get)
	mux.HandleFunc("DELETE /warden/jobs/{id}", s.cancel)
	mux.HandleFunc("GET /warden/jobs/{id}/events", s.events)
	return mux
}

func (s *Store) client(r *http.Request) string {
	if s.Client != nil {
		return s.Client(r)
	}
	return warden.Client(r)
}

// authOf is the key headers of a request, which the job's attempts carry.
func authOf(r *http.Request) http.Header {
	auth := http.Header{}
	for _, k := range []string{"Authorization", "X-Api-Key"} {
		if v := r.Header.Values(k); len(v) > 0 {
			auth[k] = append([]string(nil), v...)
		}
	}
	return auth
}

// claim gives the client's jobs read back after a restart the key of this
// request, which the restart did not keep.
func (s *Store) claim(r *http.Request, client string) {
	if !s.keysRequired() {
		return
	}
	auth := authOf(r)
	for _, j := range s.List(client) {
		s.Rekey(j, auth)
	}
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

func (s *Store) create(rw http.ResponseWriter, r *http.Request) {
	client := s.client(r)
	s.claim(r, client)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"detail": "could not read the request body"})
		return
	}
	var in struct {
		Path string          `json:"path"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"detail": "a job is {\"path\": ..., \"body\": {...}}: " + err.Error()})
		return
	}
	j, status, err := s.Create(client, in.Path, in.Body, authOf(r))
	if err != nil {
		writeJSON(rw, status, map[string]string{"detail": err.Error()})
		return
	}
	rw.Header().Set("Location", "/warden/jobs/"+j.id)
	writeJSON(rw, status, j.Snapshot(s.keysRequired()))
}

func (s *Store) list(rw http.ResponseWriter, r *http.Request) {
	client := s.client(r)
	s.claim(r, client)
	out := []Snapshot{}
	for _, j := range s.List(client) {
		out = append(out, j.Snapshot(s.keysRequired()))
	}
	writeJSON(rw, http.StatusOK, map[string]any{"jobs": out})
}

// lookup is the job the path names, or a 404 written.
func (s *Store) lookup(rw http.ResponseWriter, r *http.Request) *Job {
	client := s.client(r)
	s.claim(r, client)
	j := s.Get(r.PathValue("id"), client)
	if j == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"detail": "no job " + r.PathValue("id")})
	}
	return j
}

func (s *Store) get(rw http.ResponseWriter, r *http.Request) {
	if j := s.lookup(rw, r); j != nil {
		writeJSON(rw, http.StatusOK, j.Snapshot(s.keysRequired()))
	}
}

func (s *Store) cancel(rw http.ResponseWriter, r *http.Request) {
	if j := s.lookup(rw, r); j != nil {
		s.Cancel(j)
		writeJSON(rw, http.StatusOK, j.Snapshot(s.keysRequired()))
	}
}

// keepalive is how often an idle event stream sends a comment, so a proxy
// between does not close it.
var keepalive = 15 * time.Second

// events streams a job's events from the one after ?after or Last-Event-ID,
// and ends after the job's last.
func (s *Store) events(rw http.ResponseWriter, r *http.Request) {
	j := s.lookup(rw, r)
	if j == nil {
		return
	}
	from := r.URL.Query().Get("after")
	if from == "" {
		from = r.Header.Get("Last-Event-ID")
	}
	var after int64
	if from != "" {
		var err error
		if after, err = strconv.ParseInt(from, 10, 64); err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"detail": "after is an event's seq"})
			return
		}
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	flush := func() {
		if f, ok := rw.(http.Flusher); ok {
			f.Flush()
		}
	}
	flush()
	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	for {
		events, ended, changed := j.since(after)
		for _, e := range events {
			if _, err := fmt.Fprintf(rw, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, e.Data); err != nil {
				return
			}
			after = e.Seq
		}
		flush()
		if ended {
			return
		}
		select {
		case <-changed:
		case <-tick.C:
			if _, err := io.WriteString(rw, ": keepalive\n\n"); err != nil {
				return
			}
			flush()
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		}
	}
}
