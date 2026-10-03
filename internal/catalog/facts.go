// Package catalog derives what a client has to know about a local model from
// the command that starts it and the GGUF it loads: the context window, the
// input it takes, and the reasoning efforts its chat template accepts. Nothing
// is declared by hand, so nothing can drift from the model file (the user's
// choice, 2026-10-03).
//
// llama-swap's /v1/models gets these as meta.infermux on each local model, so
// the other hosts read them with the list. A client that asks in Codex's own
// format gets the whole list as Codex's model catalog (codex.go).
package catalog

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Facts are one model's derived settings.
type Facts struct {
	// ContextWindow is the tokens one request may use: the context llama.cpp
	// actually allocated once the model has run, before that --ctx-size (or
	// the GGUF's trained length), divided between --parallel slots unless
	// the cache is unified. Zero when nothing says.
	ContextWindow int `json:"context_window,omitempty"`
	// InputModalities is text, and image with an --mmproj.
	InputModalities []string `json:"input_modalities"`
	// ReasoningEfforts are the efforts a request may send, in OpenAI's
	// names. "none" turns thinking off; the rest are only what the chat
	// template accepts, because it raises an exception on any other.
	ReasoningEfforts []string `json:"reasoning_efforts"`
	DefaultEffort    string   `json:"default_effort,omitempty"`
}

// efforts is OpenAI's order, which Codex shows them in.
var efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Deriver reads each GGUF once, again only when its size or time changes.
type Deriver struct {
	mu    sync.Mutex
	files map[string]cachedGGUF
}

type cachedGGUF struct {
	size int64
	mod  time.Time
	meta ggufMeta
	err  error
}

func (d *Deriver) gguf(path string) (ggufMeta, error) {
	st, err := os.Stat(path)
	if err != nil {
		return ggufMeta{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.files[path]; ok && c.size == st.Size() && c.mod.Equal(st.ModTime()) {
		return c.meta, c.err
	}
	meta, err := readGGUF(path)
	if d.files == nil {
		d.files = map[string]cachedGGUF{}
	}
	d.files[path] = cachedGGUF{size: st.Size(), mod: st.ModTime(), meta: meta, err: err}
	return meta, err
}

// Derive reads a llama-server command. loadedContext is the context llama.cpp
// reported for a model that has run, or zero. A GGUF that cannot be read
// leaves what comes from it unknown rather than failing the list.
func (d *Deriver) Derive(args []string, loadedContext int) Facts {
	flags := parseFlags(args)
	f := Facts{InputModalities: []string{"text"}, ReasoningEfforts: []string{}}
	if flags.has("-mm", "--mmproj") {
		f.InputModalities = append(f.InputModalities, "image")
	}

	var meta ggufMeta
	if model := flags.value("-m", "--model"); model != "" {
		meta, _ = d.gguf(model)
	}
	template := meta.chatTemplate
	if file := flags.value("--chat-template-file"); file != "" {
		raw, _ := os.ReadFile(file)
		template = string(raw)
	} else if inline := flags.value("--chat-template"); inline != "" {
		// A built-in template's name says nothing about its kwargs.
		template = ""
		if strings.Contains(inline, "{%") || strings.Contains(inline, "{{") {
			template = inline
		}
	}

	f.ContextWindow = loadedContext
	if f.ContextWindow == 0 {
		ctx, _ := strconv.Atoi(flags.value("-c", "--ctx-size"))
		if ctx == 0 {
			ctx = meta.contextLength
		}
		slots, _ := strconv.Atoi(flags.value("-np", "--parallel"))
		if slots > 1 && !flags.has("-kvu", "--kv-unified") {
			ctx /= slots
		}
		f.ContextWindow = ctx
	}

	thinkingOff := flags.value("-rea", "--reasoning") == "off" || flags.value("--reasoning-budget") == "0"
	if !thinkingOff {
		f.ReasoningEfforts, f.DefaultEffort = templateEfforts(template)
		if set := flags.value("--reasoning-effort"); slices.Contains(f.ReasoningEfforts, set) {
			f.DefaultEffort = set
		}
	}
	return f
}

var (
	effortCompare = regexp.MustCompile(`reasoning_effort\w*\s*[!=]=\s*['"](\w+)['"]`)
	effortTuple   = regexp.MustCompile(`reasoning_effort\w*\s+(?:not\s+)?in\s*[(\[]([^)\]]*)[)\]]`)
	effortDefault = regexp.MustCompile(`reasoning_effort\s*\|\s*default\(\s*['"](\w+)['"]`)
	quoted        = regexp.MustCompile(`['"](\w+)['"]`)
)

// templateEfforts is what a chat template accepts. llama-server sends "none"
// as enable_thinking false, so a template that reads enable_thinking can turn
// thinking off; any other effort reaches the template as reasoning_effort,
// and only the values it compares against are safe to send.
func templateEfforts(template string) (levels []string, def string) {
	accepted := map[string]bool{}
	for _, m := range effortCompare.FindAllStringSubmatch(template, -1) {
		accepted[m[1]] = true
	}
	for _, m := range effortTuple.FindAllStringSubmatch(template, -1) {
		for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
			accepted[q[1]] = true
		}
	}
	if m := effortDefault.FindStringSubmatch(template); m != nil {
		def = m[1]
	}
	toggles := strings.Contains(template, "enable_thinking")
	for _, e := range efforts {
		if accepted[e] && e != "none" {
			levels = append(levels, e)
		}
	}
	switch {
	case len(levels) == 0 && toggles:
		// On or off: "medium" reaches no template that ignores it.
		levels, def = []string{"medium"}, "medium"
	case len(levels) == 0:
		return []string{}, ""
	}
	if !slices.Contains(levels, def) {
		def = levels[len(levels)-1]
	}
	if toggles {
		levels = append([]string{"none"}, levels...)
	}
	return levels, def
}

// flags is a command's arguments by name, last one winning as in llama.cpp.
type flags map[string]string

func parseFlags(args []string) flags {
	f := flags{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		if name, value, ok := strings.Cut(a, "="); ok {
			f[name] = value
			continue
		}
		f[a] = ""
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			f[a] = args[i+1]
			i++
		}
	}
	return f
}

func (f flags) has(names ...string) bool {
	for _, n := range names {
		if _, ok := f[n]; ok {
			return true
		}
	}
	return false
}

func (f flags) value(names ...string) string {
	for _, n := range names {
		if v, ok := f[n]; ok {
			return v
		}
	}
	return ""
}
