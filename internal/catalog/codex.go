package catalog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// Enrich adds meta.infermux, the model's Facts, to every local model in
// next's /v1/models. command is a model's llama-server command, false for a
// model that has none to read.
func Enrich(next http.Handler, d *Deriver, command func(id string) ([]string, bool)) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			next.ServeHTTP(rw, r)
			return
		}
		list, rec, ok := record(next, r)
		if !ok {
			rec.replay(rw)
			return
		}
		for _, item := range entries(list) {
			if typeOf(item) != "model" {
				continue
			}
			id, _ := item["id"].(string)
			args, ok := command(id)
			if !ok {
				continue
			}
			loaded := 0
			if meta, _ := item["meta"].(map[string]any); meta != nil {
				n, _ := meta["n_ctx"].(float64)
				loaded = int(n)
			}
			mergeMeta(item, "infermux", d.Derive(args, loaded))
		}
		write(rw, rec.header, list)
	})
}

// Codex answers Codex's own request for its model catalog, which it marks
// with client_version, with the list in Codex's format (ModelsResponse in
// codex-rs/protocol/src/openai_models.rs). Every other request, and every
// one while prompt is empty, gets the list as it is.
//
// Codex refuses a catalog entry without its base instructions, which are
// Codex's own prompt.md and change with its version, so the catalog carries
// the prompt.md of the Codex installed beside it: promptFile.
func Codex(next http.Handler, promptFile string) (http.Handler, error) {
	if promptFile == "" {
		return next, nil
	}
	raw, err := os.ReadFile(promptFile)
	if err != nil {
		return nil, err
	}
	prompt := string(raw)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || !r.URL.Query().Has("client_version") {
			next.ServeHTTP(rw, r)
			return
		}
		list, rec, ok := record(next, r)
		if !ok {
			rec.replay(rw)
			return
		}
		models := []any{}
		for i, item := range entries(list) {
			id, _ := item["id"].(string)
			if id == "" {
				continue
			}
			var f Facts
			if meta, _ := item["meta"].(map[string]any); meta != nil {
				raw, _ := json.Marshal(meta["infermux"])
				json.Unmarshal(raw, &f)
			}
			models = append(models, codexModel(id, i, f, prompt))
		}
		write(rw, rec.header, map[string]any{"models": models})
	}), nil
}

// codexModel is one catalog entry: Codex's fallback for an unknown model
// (model_info_from_slug in codex-rs/models-manager/src/model_info.rs) with the
// derived facts in place of its guesses.
func codexModel(id string, priority int, f Facts, prompt string) map[string]any {
	modalities := f.InputModalities
	if len(modalities) == 0 {
		modalities = []string{"text"}
	}
	levels := []map[string]string{}
	for _, e := range f.ReasoningEfforts {
		levels = append(levels, map[string]string{"effort": e, "description": effortDescription(e, len(f.ReasoningEfforts))})
	}
	var def any
	if f.DefaultEffort != "" {
		def = f.DefaultEffort
	}
	m := map[string]any{
		"slug":                                 id,
		"display_name":                         id,
		"description":                          nil,
		"default_reasoning_level":              def,
		"supported_reasoning_levels":           levels,
		"shell_type":                           "unified_exec",
		"visibility":                           "list",
		"supported_in_api":                     true,
		"priority":                             priority,
		"availability_nux":                     nil,
		"upgrade":                              nil,
		"model_messages":                       map[string]any{"instructions_template": prompt},
		"include_skills_usage_instructions":    false,
		"include_plugin_usage_instructions":    false,
		"include_apps_usage_instructions":      false,
		"supports_reasoning_summary_parameter": true,
		"default_reasoning_summary":            "auto",
		"support_verbosity":                    false,
		"default_verbosity":                    nil,
		"apply_patch_tool_type":                nil,
		"web_search_tool_type":                 "text",
		"truncation_policy":                    map[string]any{"mode": "bytes", "limit": 10000},
		"experimental_supported_tools":         []string{},
		"input_modalities":                     modalities,
	}
	if f.ContextWindow > 0 {
		m["context_window"] = f.ContextWindow
		m["max_context_window"] = f.ContextWindow
	}
	return m
}

func effortDescription(effort string, of int) string {
	switch {
	case effort == "none":
		return "Thinking off"
	case of == 2:
		return "Thinking on"
	}
	return "Thinking, " + effort + " effort"
}

func record(next http.Handler, r *http.Request) (map[string]any, *recorder, bool) {
	rec := &recorder{header: http.Header{}, code: http.StatusOK}
	next.ServeHTTP(rec, r)
	var list map[string]any
	if rec.code != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
		return nil, rec, false
	}
	return list, rec, true
}

func entries(list map[string]any) []map[string]any {
	data, _ := list["data"].([]any)
	out := make([]map[string]any, 0, len(data))
	for _, d := range data {
		if m, ok := d.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func typeOf(item map[string]any) string {
	meta, _ := item["meta"].(map[string]any)
	ls, _ := meta["llamaswap"].(map[string]any)
	t, _ := ls["type"].(string)
	return t
}

// mergeMeta adds value's fields to meta[key], keeping what is there.
func mergeMeta(item map[string]any, key string, value any) {
	meta, _ := item["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		item["meta"] = meta
	}
	merged, _ := meta[key].(map[string]any)
	if merged == nil {
		merged = map[string]any{}
	}
	raw, _ := json.Marshal(value)
	json.Unmarshal(raw, &merged)
	meta[key] = merged
}

func write(rw http.ResponseWriter, header http.Header, body any) {
	for k, v := range header {
		if !strings.EqualFold(k, "Content-Length") {
			rw.Header()[k] = v
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(body)
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

func (r *recorder) replay(rw http.ResponseWriter) {
	for k, v := range r.header {
		rw.Header()[k] = v
	}
	rw.WriteHeader(r.code)
	io.Copy(rw, &r.body)
}
