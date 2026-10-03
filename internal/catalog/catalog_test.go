package catalog

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The parts of real templates that decide the efforts, cut from the GGUFs in
// /srv/models on 2026-10-03.
const (
	// Bonsai-2-27B: no "high", which raises an exception.
	bonsaiTemplate = `{%- if enable_thinking is undefined or enable_thinking is true %}
    {%- set resolved_reasoning_effort = reasoning_effort|default('xhigh') %}
    {%- if resolved_reasoning_effort not in ('xhigh', 'medium', 'low') %}
        {{- raise_exception('Unexpected reasoning effort ' ~ reasoning_effort ~ '. Supported types are xhigh (default), medium, and low.') }}
    {%- if resolved_reasoning_effort == 'xhigh' %}
    {%- elif resolved_reasoning_effort == 'low' %}
{%- if enable_thinking is defined and enable_thinking is false %}`
	// Qwen3.8-27B: "high" is taken and means xhigh.
	qwen38Template = `{%- if enable_thinking is undefined or enable_thinking is true %}
    {%- set resolved_reasoning_effort = reasoning_effort|default('xhigh') %}
    {%- if resolved_reasoning_effort == 'high' %}
        {%- set resolved_reasoning_effort = 'xhigh' %}
    {%- if resolved_reasoning_effort not in ('xhigh', 'medium', 'low') %}
        {{- raise_exception('Unexpected reasoning effort') }}`
	// Qwopus3.6-35B: thinking on or off, no effort.
	qwopusTemplate = `{%- if enable_thinking is defined and enable_thinking is false %}`
)

func TestTheEffortsAreWhatTheTemplateAccepts(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		levels         []string
		def            string
	}{
		{"bonsai", bonsaiTemplate, []string{"none", "low", "medium", "xhigh"}, "xhigh"},
		{"qwen3.8", qwen38Template, []string{"none", "low", "medium", "high", "xhigh"}, "xhigh"},
		{"on or off", qwopusTemplate, []string{"none", "medium"}, "medium"},
		{"no thinking", `{{ messages }}`, []string{}, ""},
		// Efforts it reads but does not list: only on and off are safe.
		{"unreadable efforts", `{{ reasoning_effort }}{% if enable_thinking %}{% endif %}`, []string{"none", "medium"}, "medium"},
		// Efforts but no switch: none is never offered.
		{"efforts only", `{% if reasoning_effort in ["low", "high"] %}{% endif %}`, []string{"low", "high"}, "high"},
	} {
		levels, def := templateEfforts(tc.template)
		if !slices.Equal(levels, tc.levels) || def != tc.def {
			t.Errorf("%s: %v default %q, want %v default %q", tc.name, levels, def, tc.levels, tc.def)
		}
	}
}

// writeGGUF is a GGUF with metadata only, as readGGUF sees one.
func writeGGUF(t *testing.T, path, arch string, ctx uint32, template string) {
	var b bytes.Buffer
	le := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { le(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	le(uint32(3))
	le(uint64(0))
	le(uint64(5))
	str("general.architecture")
	le(uint32(ggufString))
	str(arch)
	// The tokenizer's vocabulary sits before the template in real files.
	str("tokenizer.ggml.tokens")
	le(uint32(ggufArray))
	le(uint32(ggufString))
	le(uint64(3))
	str("a")
	str("bb")
	str("<|im_end|>")
	str("general.file_type")
	le(uint32(ggufUint32))
	le(uint32(15))
	str(arch + ".context_length")
	le(uint32(ggufUint32))
	le(ctx)
	str("tokenizer.chat_template")
	le(uint32(ggufString))
	str(template)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsAreReadFromTheCommandAndTheGGUF(t *testing.T) {
	dir := t.TempDir()
	gguf := filepath.Join(dir, "m.gguf")
	writeGGUF(t, gguf, "qwen35", 262144, bonsaiTemplate)
	other := filepath.Join(dir, "template.jinja")
	os.WriteFile(other, []byte(qwopusTemplate), 0o644)
	d := &Deriver{}
	cmd := func(extra ...string) []string {
		return append([]string{"/bin/llama-server", "--port", "5800", "--model", gguf}, extra...)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		loaded  int
		ctx     int
		image   bool
		efforts []string
		def     string
	}{
		{"the GGUF's own length", cmd(), 0, 262144, false, []string{"none", "low", "medium", "xhigh"}, "xhigh"},
		{"--ctx-size", cmd("--ctx-size", "65536", "--mmproj", "/m/proj.gguf"), 0, 65536, true, []string{"none", "low", "medium", "xhigh"}, "xhigh"},
		{"what it allocated wins", cmd("-c", "65536", "--fit", "on"), 40960, 40960, false, []string{"none", "low", "medium", "xhigh"}, "xhigh"},
		{"split between slots", cmd("-c", "65536", "-np", "2"), 0, 32768, false, []string{"none", "low", "medium", "xhigh"}, "xhigh"},
		{"unless unified", cmd("-c", "65536", "-np", "2", "-kvu"), 0, 65536, false, []string{"none", "low", "medium", "xhigh"}, "xhigh"},
		{"--reasoning-effort sets the default", cmd("--reasoning-effort", "low"), 0, 262144, false, []string{"none", "low", "medium", "xhigh"}, "low"},
		{"thinking off", cmd("--reasoning", "off"), 0, 262144, false, []string{}, ""},
		{"no budget", cmd("--reasoning-budget", "0"), 0, 262144, false, []string{}, ""},
		{"an unlimited budget is not a flag", cmd("--reasoning-budget", "-1", "-c", "8192"), 0, 8192, false, []string{"none", "low", "medium", "xhigh"}, "xhigh"},
		{"a template file replaces the GGUF's", cmd("--chat-template-file", other), 0, 262144, false, []string{"none", "medium"}, "medium"},
		{"a built-in template is not guessed", cmd("--chat-template", "chatml"), 0, 262144, false, []string{}, ""},
		{"no GGUF to read", []string{"/bin/llama-server", "--model", filepath.Join(dir, "gone.gguf"), "-c=4096"}, 0, 4096, false, []string{}, ""},
	} {
		f := d.Derive(tc.args, tc.loaded)
		if f.ContextWindow != tc.ctx || slices.Contains(f.InputModalities, "image") != tc.image ||
			!slices.Equal(f.ReasoningEfforts, tc.efforts) || f.DefaultEffort != tc.def {
			t.Errorf("%s: %+v", tc.name, f)
		}
	}
}

func TestAChangedGGUFIsReadAgain(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "m.gguf")
	writeGGUF(t, gguf, "llama", 4096, qwopusTemplate)
	d := &Deriver{}
	args := []string{"llama-server", "-m", gguf}
	if f := d.Derive(args, 0); f.ContextWindow != 4096 {
		t.Fatalf("first: %+v", f)
	}
	// The same size, a later time: a model file replaced in place.
	first, _ := os.Stat(gguf)
	writeGGUF(t, gguf, "llama", 131072, qwopusTemplate)
	os.Chtimes(gguf, first.ModTime().Add(time.Second), first.ModTime().Add(time.Second))
	if f := d.Derive(args, 0); f.ContextWindow != 131072 {
		t.Fatalf("after the file changed: %+v", f)
	}
	// Another size, the same time.
	writeGGUF(t, gguf, "llama", 2048, qwopusTemplate+" ")
	os.Chtimes(gguf, first.ModTime().Add(time.Second), first.ModTime().Add(time.Second))
	if f := d.Derive(args, 0); f.ContextWindow != 2048 {
		t.Fatalf("after the size changed: %+v", f)
	}
}

func TestAFileThatIsNotAGGUFIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.gguf")
	os.WriteFile(path, []byte("GGUF\x03\x00\x00\x00"), 0o644)
	if _, err := readGGUF(path); err == nil {
		t.Fatal("a truncated header was read")
	}
	writeGGUF(t, path, "llama", 4096, qwopusTemplate)
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, append([]byte("GGML"), raw[4:]...), 0o644)
	if _, err := readGGUF(path); err == nil {
		t.Fatal("a file without the magic was read")
	}
}

// fakeList is llama-swap's /v1/models: a local model, a peer's, and another
// host's as the remote router adds it, with that host's facts.
func fakeList(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Content-Length", "999")
	json.NewEncoder(rw).Encode(map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "local", "object": "model", "meta": map[string]any{"llamaswap": map[string]any{"type": "model"}, "n_ctx": 32768.0}},
		map[string]any{"id": "openrouter/glm", "object": "model", "meta": map[string]any{"llamaswap": map[string]any{"type": "peer"}}},
		map[string]any{"id": "zbox/small", "object": "model", "meta": map[string]any{
			"llamaswap": map[string]any{"type": "remote"},
			"infermux":  map[string]any{"host": "zbox", "online": true, "context_window": 8192, "input_modalities": []string{"text"}, "reasoning_efforts": []string{"none", "medium"}, "default_effort": "medium"},
		}},
	}})
}

func get(t *testing.T, h http.Handler, url string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	var out map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("%s: %d %s", url, rec.Code, rec.Body)
	}
	return out
}

func TestTheListCarriesTheLocalModelsSettings(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "m.gguf")
	writeGGUF(t, gguf, "qwen35", 262144, bonsaiTemplate)
	asked := []string{}
	h := Enrich(http.HandlerFunc(fakeList), &Deriver{}, func(id string) ([]string, bool) {
		asked = append(asked, id)
		return []string{"llama-server", "-m", gguf, "--mmproj", "p.gguf"}, true
	})
	data := get(t, h, "/v1/models")["data"].([]any)
	local := data[0].(map[string]any)["meta"].(map[string]any)["infermux"].(map[string]any)
	if local["context_window"] != 32768.0 || len(local["reasoning_efforts"].([]any)) != 4 || len(local["input_modalities"].([]any)) != 2 {
		t.Fatalf("the local model: %v", local)
	}
	if !slices.Equal(asked, []string{"local"}) {
		t.Fatalf("asked for the commands of %v: only a local model has one", asked)
	}
	if remote := data[2].(map[string]any)["meta"].(map[string]any)["infermux"].(map[string]any); remote["host"] != "zbox" {
		t.Fatalf("the other host's entry was touched: %v", remote)
	}
}

func TestCodexGetsItsCatalogAndEveryoneElseTheList(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	os.WriteFile(prompt, []byte("You are Codex."), 0o644)
	h, err := Codex(http.HandlerFunc(fakeList), prompt)
	if err != nil {
		t.Fatal(err)
	}
	if list := get(t, h, "/v1/models"); list["object"] != "list" {
		t.Fatalf("a plain client got %v", list)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models?client_version=0.159.2", nil))
	if rec.Header().Get("Content-Length") == "999" {
		t.Fatal("the list's Content-Length was kept for a different body")
	}
	// The fields Codex 0.159.2's ModelInfo requires: neither an Option nor
	// serde(default), so one missing fails its decode of the whole catalog.
	required := []string{"slug", "display_name", "supported_reasoning_levels", "shell_type", "visibility",
		"supported_in_api", "priority", "support_verbosity", "truncation_policy", "experimental_supported_tools"}
	var raw struct{ Models []map[string]json.RawMessage }
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Models) != 3 {
		t.Fatalf("the catalog: %v %s", err, rec.Body)
	}
	for _, m := range raw.Models {
		for _, field := range required {
			if _, ok := m[field]; !ok {
				t.Fatalf("an entry misses %s: %s", field, rec.Body)
			}
		}
	}
	var catalog struct {
		Models []struct {
			Slug     string                                 `json:"slug"`
			Levels   []struct{ Effort, Description string } `json:"supported_reasoning_levels"`
			Messages struct {
				Template string `json:"instructions_template"`
			} `json:"model_messages"`
			Default *string `json:"default_reasoning_level"`
			Context *int    `json:"context_window"`
		} `json:"models"`
	}
	json.Unmarshal(rec.Body.Bytes(), &catalog)
	for _, m := range catalog.Models {
		// Codex has no prompt for a model it does not know but this one.
		if m.Messages.Template != "You are Codex." {
			t.Fatalf("%s has no base instructions", m.Slug)
		}
	}
	zbox := catalog.Models[2]
	if zbox.Slug != "zbox/small" || *zbox.Context != 8192 || *zbox.Default != "medium" || len(zbox.Levels) != 2 ||
		zbox.Levels[0].Effort != "none" || zbox.Levels[1].Description != "Thinking on" {
		t.Fatalf("the other host's model: %s", rec.Body)
	}
	if peer := catalog.Models[1]; peer.Context != nil || len(peer.Levels) != 0 || peer.Default != nil {
		t.Fatalf("a model with no facts got some: %s", rec.Body)
	}
}

func TestWithoutAPromptCodexGetsTheListAsItIs(t *testing.T) {
	h, err := Codex(http.HandlerFunc(fakeList), "")
	if err != nil {
		t.Fatal(err)
	}
	if list := get(t, h, "/v1/models?client_version=1"); list["object"] != "list" {
		t.Fatalf("got %v", list)
	}
	if _, err := Codex(http.HandlerFunc(fakeList), "/nonexistent/prompt.md"); err == nil {
		t.Fatal("a prompt that cannot be read was taken")
	}
}
