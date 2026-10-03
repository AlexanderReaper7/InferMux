package warden

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// keyContext carries the request's key from Wrap to the handlers after it.
type keyContext struct{}

// Client is the name of the key a request came with, empty without a
// keys_file.
func Client(r *http.Request) string {
	key, _ := r.Context().Value(keyContext{}).(namedKey)
	return key.name
}

// FilterModels drops from /v1/models every model the request's key may not
// use, named as the gate names it, so a client's picker offers nothing that
// would answer 403 (0006, 4). It goes inside Wrap, which identifies the key,
// and before anything that rewrites the list into another format, such as
// Codex's catalog. A key without an allow list sees the list as it is.
func (w *Warden) FilterModels(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		key, _ := r.Context().Value(keyContext{}).(namedKey)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || key.Allow == nil {
			next.ServeHTTP(rw, r)
			return
		}
		rec := &listRecorder{header: http.Header{}, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		var list map[string]any
		if rec.code != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
			rec.replay(rw)
			return
		}
		data, _ := list["data"].([]any)
		kept := []any{}
		for _, entry := range data {
			item, _ := entry.(map[string]any)
			id, _ := item["id"].(string)
			if model, ok := w.models.QualifyName(id); ok && key.allows(model) {
				kept = append(kept, entry)
			}
		}
		list["data"] = kept
		for k, v := range rec.header {
			if k != "Content-Length" {
				rw.Header()[k] = v
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(list)
	})
}

// listRecorder holds the list until it has been filtered.
type listRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *listRecorder) Header() http.Header         { return r.header }
func (r *listRecorder) WriteHeader(code int)        { r.code = code }
func (r *listRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *listRecorder) replay(rw http.ResponseWriter) {
	for k, v := range r.header {
		rw.Header()[k] = v
	}
	rw.WriteHeader(r.code)
	rw.Write(r.body.Bytes())
}
