package warden

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// codexTurn is a Responses body as Codex sends it: the user's message, then
// for each reply a function call and its output in Codex's exec envelope,
// whose chunk ID and wall time differ on every run.
func codexTurn(model string, calls ...string) string {
	items := []any{
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "rules"}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "something killed my network"}}},
	}
	for i, c := range calls {
		cmd, out, _ := strings.Cut(c, "=>")
		id := fmt.Sprintf("call_%d", i)
		items = append(items,
			map[string]any{"type": "reasoning", "summary": []any{}, "content": []any{map[string]any{"type": "reasoning_text", "text": "Let me check."}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "The kernel is the standard NixOS kernel."}}},
			map[string]any{"type": "function_call", "name": "exec_command", "call_id": id, "arguments": `{"cmd":"` + cmd + `"}`},
			map[string]any{"type": "function_call_output", "call_id": id, "output": fmt.Sprintf("Chunk ID: %06x\nWall time: 0.%d seconds\nProcess exited with code 0\nOutput:\n%s", i, i, out)},
		)
	}
	raw, _ := json.Marshal(map[string]any{"model": model, "stream": true, "input": items})
	return string(raw)
}

func TestTheStreakCountsRepliesThatAddedNothingNew(t *testing.T) {
	cases := []struct {
		name  string
		calls []string
		want  int
	}{
		{"all new", []string{"ip link=>up", "ping=>loss", "dmesg=>denied"}, 0},
		{"one repeat", []string{"ip link=>up", "deriver=>unknown", "deriver=>unknown"}, 1},
		{"two repeats, the thread of 2026-10-05", []string{"ip link=>up", "deriver=>unknown", "deriver=>unknown", "deriver=>unknown"}, 2},
		{"a new output breaks it", []string{"deriver=>unknown", "deriver=>unknown", "deriver=>found", "deriver=>found"}, 1},
		{"an older call repeated counts too", []string{"a=>1", "b=>2", "a=>1", "b=>2"}, 2},
		{"a new call after repeats ends it", []string{"a=>1", "a=>1", "a=>1", "c=>3"}, 0},
	}
	for _, c := range cases {
		if got := findRepetition([]byte(codexTurn("m", c.calls...))).streak; got != c.want {
			t.Errorf("%s: streak %d, want %d", c.name, got, c.want)
		}
	}
}

func TestAUserMessageStartsANewTurn(t *testing.T) {
	var body map[string]any
	json.Unmarshal([]byte(codexTurn("m", "a=>1", "a=>1", "a=>1")), &body)
	items := body["input"].([]any)
	// The user's next message, then the same call once more.
	body["input"] = append(items, map[string]any{"type": "message", "role": "user", "content": "try again"},
		map[string]any{"type": "function_call", "name": "exec_command", "call_id": "x", "arguments": `{"cmd":"a"}`},
		map[string]any{"type": "function_call_output", "call_id": "x", "output": "Output:\n1"})
	raw, _ := json.Marshal(body)
	if got := findRepetition(raw).streak; got != 0 {
		t.Fatalf("streak %d across the user's message", got)
	}
}

func TestPollingARunningCommandIsNotStuck(t *testing.T) {
	poll := func(id string) []any {
		return []any{
			map[string]any{"type": "function_call", "name": "write_stdin", "call_id": id, "arguments": `{"session_id":7,"chars":"","yield_time_ms":30000}`},
			map[string]any{"type": "function_call_output", "call_id": id, "output": "Wall time: 30.0 seconds\nProcess running with session ID 7\nOutput:\n"},
		}
	}
	items := []any{map[string]any{"role": "user", "content": "build it"}}
	for _, id := range []string{"a", "b", "c", "d"} {
		items = append(items, poll(id)...)
	}
	raw, _ := json.Marshal(map[string]any{"input": items})
	if got := findRepetition(raw).streak; got != 0 {
		t.Fatalf("streak %d for a build being waited on", got)
	}
}

func TestChatCompletionsAreReadToo(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "go"}}
	for i := range 3 {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"cmd":"ls"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": "a b"})
	}
	raw, _ := json.Marshal(map[string]any{"messages": msgs})
	rep := findRepetition(raw)
	if rep.streak != 2 || rep.output != "messages.6.content" || !rep.chat {
		t.Fatalf("got %+v", rep)
	}
}

func TestStuckThresholdsMustNotGoBackwards(t *testing.T) {
	if err := (Stuck{AnnotateAfter: 3, EscalateAfter: 2}).check(); err == nil {
		t.Error("escalating before annotating was taken")
	}
	if err := (Stuck{AnnotateAfter: -1}).check(); err == nil {
		t.Error("a negative threshold was taken")
	}
	if err := (Stuck{AnnotateAfter: 2, RefuseAfter: 3}).check(); err != nil {
		t.Errorf("escalation off was refused: %v", err)
	}
}

// stuckHarness is the warden in front of a server that records what reached
// it.
func stuckHarness(t *testing.T, mutate func(*Stuck)) (http.Handler, *[]string) {
	h := newHarness(t, func(c *Config) {
		c.Stuck.EscalateTo = "big"
		if mutate != nil {
			mutate(&c.Stuck)
		}
	})
	var got []string
	return h.wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(raw)) {
			t.Errorf("Content-Length %d for a body of %d", r.ContentLength, len(raw))
		}
		got = append(got, string(raw))
	})), &got
}

func sendBody(handler http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAStuckAgentIsNotedThenEscalatedThenRefused(t *testing.T) {
	handler, got := stuckHarness(t, nil)
	repeats := func(n int) []string {
		calls := []string{"ip link=>up"}
		for range n + 1 {
			calls = append(calls, "deriver=>unknown")
		}
		return calls
	}

	fresh := codexTurn("qwen", repeats(0)...)
	if rec := sendBody(handler, fresh); rec.Code != http.StatusOK || (*got)[0] != fresh {
		t.Fatalf("a turn making progress was changed: %d", rec.Code)
	}

	if rec := sendBody(handler, codexTurn("qwen", repeats(2)...)); rec.Code != http.StatusOK {
		t.Fatalf("streak 2 got %d", rec.Code)
	}
	noted := (*got)[1]
	last := gjson.Get(noted, "input.@reverse.0.output").String()
	if !strings.Contains(last, "[InferMux: your last 2 replies added nothing new.") || !strings.HasPrefix(last, "Chunk ID") {
		t.Fatalf("the note is not after the last output: %q", last)
	}
	if gjson.Get(noted, "dry_multiplier").Float() != 0.8 || gjson.Get(noted, "dry_penalty_last_n").Int() != 65536 || gjson.Get(noted, "model").String() != "qwen" {
		t.Fatalf("streak 2 should keep the model and turn DRY on: %s", gjson.Get(noted, "@this").Get("model"))
	}

	if rec := sendBody(handler, codexTurn("qwen", repeats(3)...)); rec.Code != http.StatusOK {
		t.Fatalf("streak 3 got %d", rec.Code)
	}
	if model := gjson.Get((*got)[2], "model").String(); model != "big" {
		t.Fatalf("streak 3 went to %q, not the escalation model", model)
	}

	rec := sendBody(handler, codexTurn("qwen", repeats(4)...))
	if rec.Code != http.StatusBadRequest || len(*got) != 3 {
		t.Fatalf("streak 4 got %d and reached the model %d times", rec.Code, len(*got))
	}
	if msg := gjson.Get(rec.Body.String(), "error.message").String(); !strings.Contains(msg, "deriver") || gjson.Get(rec.Body.String(), "error.type").String() != "stuck" {
		t.Fatalf("the refusal does not say what repeated: %s", rec.Body)
	}
}

func TestTheEscalationModelItselfIsRefusedWhenStuck(t *testing.T) {
	handler, got := stuckHarness(t, nil)
	rec := sendBody(handler, codexTurn("big", "a=>1", "a=>1", "a=>1", "a=>1"))
	if rec.Code != http.StatusBadRequest || len(*got) != 0 {
		t.Fatalf("got %d, reached the model %d times", rec.Code, len(*got))
	}
	if msg := gjson.Get(rec.Body.String(), "error.message").String(); !strings.Contains(msg, "already the model") {
		t.Fatalf("the refusal does not say why it was not escalated: %s", msg)
	}
}

func TestWithoutAnEscalationModelTheNoteContinuesUntilTheRefusal(t *testing.T) {
	handler, got := stuckHarness(t, func(s *Stuck) { s.EscalateTo = "" })
	if rec := sendBody(handler, codexTurn("qwen", "a=>1", "a=>1", "a=>1", "a=>1")); rec.Code != http.StatusOK {
		t.Fatalf("streak 3 got %d", rec.Code)
	}
	if model := gjson.Get((*got)[0], "model").String(); model != "qwen" || !strings.Contains((*got)[0], "added nothing new") {
		t.Fatalf("streak 3 without escalation: model %q", model)
	}
}

func TestAnotherHostsModelIsNotLookedAt(t *testing.T) {
	handler, got := stuckHarness(t, nil)
	body := codexTurn("zbox/qwen", "a=>1", "a=>1", "a=>1", "a=>1", "a=>1")
	if rec := sendBody(handler, body); rec.Code != http.StatusOK || (*got)[0] != body {
		t.Fatalf("another host's request was changed or refused: %d", rec.Code)
	}
}
