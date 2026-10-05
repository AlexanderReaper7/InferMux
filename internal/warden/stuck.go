package warden

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// A stuck agent (0015). On 2026-10-05 Codex on the 35B ran the same
// `nix-store --query --deriver` five times in a row, got `unknown-deriver`
// each time, and said the same sentence before each, until the user stopped
// it after 492 s. Every reply copied the ones before it, because nothing new
// had entered the context since.
//
// The warden reads that from the request alone: the client sends the whole
// turn every time. A reply added nothing new when every call it made, with
// the output that came back, was already made in this turn. The trailing
// run of such replies is the streak, and the streak picks the step: a note
// on the last tool output and DRY sampling, then one reply from a stronger
// model, then a refusal that ends the turn.

// Stuck is the steps' thresholds, each a streak length; 0 turns a step off.
type Stuck struct {
	// AnnotateAfter adds a note to the last tool output saying the call
	// returned nothing new, and turns DRY on for that one request, which
	// penalises repeating any sequence already in the context.
	AnnotateAfter int `yaml:"annotate_after" json:"annotate_after"`
	// EscalateAfter sends the request to EscalateTo instead, noted and with
	// DRY. When the stuck model is EscalateTo, or the key may not use it, the
	// request is refused.
	EscalateAfter int    `yaml:"escalate_after" json:"escalate_after"`
	EscalateTo    string `yaml:"escalate_to" json:"escalate_to"`
	// RefuseAfter answers 400, which ends a Codex turn without a retry.
	RefuseAfter int `yaml:"refuse_after" json:"refuse_after"`
}

type stuckStep int

const (
	notStuck stuckStep = iota
	annotateStuck
	escalateStuck
	refuseStuck
)

func (s Stuck) step(streak int) stuckStep {
	switch {
	case streak <= 0:
		return notStuck
	case s.RefuseAfter > 0 && streak >= s.RefuseAfter:
		return refuseStuck
	case s.EscalateAfter > 0 && streak >= s.EscalateAfter && s.EscalateTo != "":
		return escalateStuck
	case s.AnnotateAfter > 0 && streak >= s.AnnotateAfter:
		return annotateStuck
	}
	return notStuck
}

func (s Stuck) on() bool {
	return s.AnnotateAfter > 0 || s.EscalateAfter > 0 && s.EscalateTo != "" || s.RefuseAfter > 0
}

func (s Stuck) check() error {
	last := 0
	for _, t := range []struct {
		name string
		v    int
	}{{"annotate_after", s.AnnotateAfter}, {"escalate_after", s.EscalateAfter}, {"refuse_after", s.RefuseAfter}} {
		if t.v < 0 {
			return fmt.Errorf("stuck.%s is %d, below 0", t.name, t.v)
		}
		if t.v > 0 && t.v < last {
			return fmt.Errorf("stuck.%s is %d, before the step above it at %d", t.name, t.v, last)
		}
		last = max(last, t.v)
	}
	return nil
}

// repetition is what the turn in a request shows.
type repetition struct {
	streak int
	// repeated is the last reply's first call, or its text, for the note.
	repeated string
	// output is the sjson path of the last item when it is a tool output,
	// where the note goes; empty when it is not. outputArray is true when
	// that output is a list of parts rather than a string.
	output      string
	outputArray bool
	chat        bool
}

// reply is one model reply in the turn.
type reply struct {
	calls []call
	text  string
}

type call struct {
	id, name, args string
}

// findRepetition reads a Responses (`input`) or Chat Completions
// (`messages`) body. The turn starts after the last user message. Anthropic's
// /v1/messages is not read: its tool results are user messages, and
// llama-server's converter would drop the DRY fields anyway.
func findRepetition(body []byte) repetition {
	if items := gjson.GetBytes(body, "input"); items.IsArray() {
		return responsesRepetition(items.Array())
	}
	if items := gjson.GetBytes(body, "messages"); items.IsArray() {
		return chatRepetition(items.Array())
	}
	return repetition{}
}

func responsesRepetition(items []gjson.Result) repetition {
	var replies []reply
	outputs := map[string]string{}
	open := false // the last item came from the model
	rep := repetition{}
	for i, item := range items {
		kind, role := item.Get("type").String(), item.Get("role").String()
		if role == "user" && (kind == "" || kind == "message") {
			replies, outputs, open = nil, map[string]string{}, false
			continue
		}
		if role == "developer" || role == "system" {
			continue
		}
		switch kind {
		case "function_call_output", "custom_tool_call_output":
			out := item.Get("output")
			outputs[item.Get("call_id").String()] = toolText(out, "text")
			open = false
			if i == len(items)-1 {
				rep.output, rep.outputArray = fmt.Sprintf("input.%d.output", i), out.IsArray()
			}
			continue
		}
		if !open {
			replies = append(replies, reply{})
			open = true
		}
		r := &replies[len(replies)-1]
		switch kind {
		case "function_call":
			r.calls = append(r.calls, call{item.Get("call_id").String(), item.Get("name").String(), item.Get("arguments").String()})
		case "custom_tool_call":
			r.calls = append(r.calls, call{item.Get("call_id").String(), item.Get("name").String(), item.Get("input").String()})
		case "message":
			r.text += toolText(item.Get("content"), "text")
		}
	}
	rep.streak, rep.repeated = streak(replies, outputs)
	return rep
}

func chatRepetition(messages []gjson.Result) repetition {
	var replies []reply
	outputs := map[string]string{}
	rep := repetition{chat: true}
	for i, m := range messages {
		switch m.Get("role").String() {
		case "user":
			replies, outputs = nil, map[string]string{}
		case "assistant":
			r := reply{text: toolText(m.Get("content"), "text")}
			for _, c := range m.Get("tool_calls").Array() {
				r.calls = append(r.calls, call{c.Get("id").String(), c.Get("function.name").String(), c.Get("function.arguments").String()})
			}
			replies = append(replies, r)
		case "tool":
			content := m.Get("content")
			outputs[m.Get("tool_call_id").String()] = toolText(content, "text")
			if i == len(messages)-1 {
				rep.output, rep.outputArray = fmt.Sprintf("messages.%d.content", i), content.IsArray()
			}
		}
	}
	rep.streak, rep.repeated = streak(replies, outputs)
	return rep
}

// streak counts the trailing replies that added nothing new. A reply with
// neither a call nor text is counted as new: the warden cannot tell what it
// did.
func streak(replies []reply, outputs map[string]string) (int, string) {
	seen := map[string]bool{}
	n := 0
	for _, r := range replies {
		fresh := len(r.calls) == 0 && strings.TrimSpace(r.text) == ""
		var keys []string
		for _, c := range r.calls {
			out := outputs[c.id]
			if waits(c, out) {
				fresh = true
				continue
			}
			keys = append(keys, "call\x00"+c.name+"\x00"+strings.TrimSpace(c.args)+"\x00"+normalizeOutput(out))
		}
		if len(r.calls) == 0 && !fresh {
			keys = append(keys, "text\x00"+strings.TrimSpace(r.text))
		}
		for _, k := range keys {
			if !seen[k] {
				fresh = true
			}
			seen[k] = true
		}
		if fresh {
			n = 0
		} else {
			n++
		}
	}
	if n == 0 {
		return 0, ""
	}
	last := replies[len(replies)-1]
	if len(last.calls) > 0 {
		c := last.calls[0]
		return n, clip(c.name+" "+strings.TrimSpace(c.args), 200)
	}
	return n, clip(strings.TrimSpace(last.text), 200)
}

// waits is a call that waits for something: Codex polling a command that has
// not ended, directly or from its exec tool, or sleeping, or waiting for a
// subagent. The same call with the same output is then progress, since time
// passed. Found by replaying every Codex thread on reaperboi (0015).
func waits(c call, output string) bool {
	switch c.name {
	case "write_stdin", "sleep", "wait_agent":
		return true
	}
	return strings.Contains(output, "Process running with session ID") ||
		strings.Contains(c.args, "write_stdin") || strings.Contains(c.args, "tools.sleep") || strings.Contains(c.args, "tools.wait_agent")
}

// toolText is a string, or the text of a list of parts.
func toolText(v gjson.Result, field string) string {
	if !v.IsArray() {
		return v.String()
	}
	var b strings.Builder
	for _, part := range v.Array() {
		b.WriteString(part.Get(field).String())
	}
	return b.String()
}

// normalizeOutput drops the lines of Codex's exec envelope that differ on
// every run of the same command: its chunk ID and its wall time.
func normalizeOutput(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "Chunk ID: ") || strings.HasPrefix(l, "Wall time: ") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// annotate appends the note to the last tool output, which is where the
// model reads next. A system or developer message would be moved to the top
// by llama-server's converter (nixcfg packages/llama-cpp, point 4), far from
// the end and invalidating the whole prompt cache.
func annotate(body []byte, rep repetition, note string) ([]byte, error) {
	if rep.output == "" {
		return body, nil
	}
	if !rep.outputArray {
		text := gjson.GetBytes(body, rep.output).String()
		return sjson.SetBytes(body, rep.output, text+"\n\n"+note)
	}
	part := map[string]string{"type": "input_text", "text": note}
	if rep.chat {
		part["type"] = "text"
	}
	return sjson.SetBytes(body, rep.output+".-1", part)
}

// dry is DRY at its usual strength over the last 65536 tokens, the presets'
// context. llama-server feeds the prompt into the sampler's history
// (server-context.cpp, init_sampler), so the earlier identical replies are
// what it penalises. Its request schema refuses -1, the "whole context" of
// the CLI flag, with a 400, and sizes the history buffer by this number, so
// it is a finite window. A field the client set itself is left alone.
func dry(body []byte) ([]byte, error) {
	for _, f := range []struct {
		key   string
		value float64
	}{{"dry_multiplier", 0.8}, {"dry_base", 1.75}, {"dry_allowed_length", 2}, {"dry_penalty_last_n", 65536}} {
		if gjson.GetBytes(body, f.key).Exists() {
			continue
		}
		var err error
		if body, err = sjson.SetBytes(body, f.key, f.value); err != nil {
			return body, err
		}
	}
	return body, nil
}

func stuckNote(rep repetition) string {
	return fmt.Sprintf("[InferMux: your last %d replies added nothing new. `%s` returned this same output before in this turn, and running it again returns it again. Do something different, or stop and tell the user what you found and what you need from them.]",
		rep.streak, rep.repeated)
}

// unstick takes the step the request's streak calls for. It returns the
// request to serve, its model and whether that is local, or done when it
// answered the request itself.
func (w *Warden) unstick(rw http.ResponseWriter, r *http.Request, key namedKey, model string) (*http.Request, string, bool, bool) {
	cfg := w.config().Stuck
	if r.Body == nil || !cfg.on() {
		return r, model, true, false
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return r, model, true, false
	}
	rep := findRepetition(body)
	step := cfg.step(rep.streak)
	if step == notStuck {
		return r, model, true, false
	}
	who := key.name
	if who == "" {
		who = "no key"
	}
	switch step {
	case refuseStuck:
		w.refuseStuck(rw, rep, model, who, "")
		return r, model, true, true
	case escalateStuck:
		why := ""
		target, ok := w.models.QualifyName(cfg.EscalateTo)
		switch {
		case !ok:
			why = cfg.EscalateTo + " is not a model this host knows"
		case target == model:
			why = "it is already the model stuck agents go to"
		case !key.allows(target):
			why = "key " + who + " may not use " + target
		}
		if why != "" {
			w.refuseStuck(rw, rep, model, who, why)
			return r, model, true, true
		}
		if body, err = sjson.SetBytes(body, "model", cfg.EscalateTo); err != nil {
			return r, model, true, false
		}
		w.log.Warnf("Stuck agent on %s (%s): %d replies added nothing new, repeating %s; this reply goes to %s", model, who, rep.streak, rep.repeated, target)
	case annotateStuck:
		w.log.Warnf("Stuck agent on %s (%s): %d replies added nothing new, repeating %s; noted, with DRY", model, who, rep.streak, rep.repeated)
	}
	if noted, err := annotate(body, rep, stuckNote(rep)); err == nil {
		body = noted
	}
	r = withBody(r, body)
	model, local, _ := w.models.Qualify(r)
	if local {
		if dried, err := dry(body); err == nil {
			r = withBody(r, dried)
		}
	}
	return r, model, local, false
}

// refuseStuck answers 400. why is set when the refusal comes at the
// escalation step because escalating is not possible.
func (w *Warden) refuseStuck(rw http.ResponseWriter, rep repetition, model, who, why string) {
	message := fmt.Sprintf("InferMux stopped this turn: the last %d replies from %s added nothing new, repeating %s", rep.streak, model, rep.repeated)
	if why != "" {
		message += "; not escalated: " + why
	}
	w.log.Warnf("Stuck agent (%s): %s", who, message)
	writeJSON(rw, http.StatusBadRequest, map[string]any{
		"error": map[string]string{"type": "stuck", "message": message + " (InferMux 0015)"},
	})
}

func withBody(r *http.Request, body []byte) *http.Request {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return r
}
