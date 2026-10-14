package jobs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/drain"
	"github.com/mostlygeek/llama-swap/internal/failover"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// model is a fake model behind the whole stack. It thinks in reasoning words
// r0 r1 ..., answers in content words w0 w1 ..., and continues a prefilled
// reply from the word after its last, as llama-server's
// continue_final_message does. onToken runs after each word is sent.
type model struct {
	t         *testing.T
	reasoning int
	words     int
	place     string
	delay     time.Duration
	// answer, when set, answers attempt n instead of the model.
	answer  func(n int, rw http.ResponseWriter, r *http.Request) bool
	onToken func(n int, word string, r *http.Request)

	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
}

func (m *model) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.bodies = append(m.bodies, body)
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	n := len(m.bodies)
	m.mu.Unlock()
	if m.answer != nil && m.answer(n, rw, r) {
		return
	}
	if r.URL.Path == "/v1/embeddings" {
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"data":[{"embedding":[0.5,0.25]}]}`))
		return
	}
	reasoning, content := 0, 0
	text := r.URL.Path == "/v1/completions"
	if text {
		content = len(strings.Fields(strings.TrimPrefix(body["prompt"].(string), "say:")))
	}
	if msgs, _ := body["messages"].([]any); len(msgs) > 0 {
		last := msgs[len(msgs)-1].(map[string]any)
		// llama-server v0.6.0 refuses this, even to continue the reasoning.
		if _, ok := last["content"]; last["role"] == "assistant" && !ok && last["tool_calls"] == nil {
			rw.WriteHeader(http.StatusBadRequest)
			rw.Write([]byte(`{"error":{"code":400,"message":"Assistant message must contain either 'content' or 'tool_calls'!"}}`))
			return
		}
		if last["role"] == "assistant" && body["continue_final_message"] != nil {
			r, _ := last["reasoning_content"].(string)
			c, _ := last["content"].(string)
			reasoning, content = len(strings.Fields(r)), len(strings.Fields(c))
			if body["continue_final_message"] == "content" {
				reasoning = m.reasoning
			}
		}
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	if m.place != "" {
		rw.Header().Set(failover.PlaceHeader, m.place)
	}
	rw.WriteHeader(http.StatusOK)
	send := func(delta string) bool {
		if r.Context().Err() != nil {
			return false
		}
		fmt.Fprintf(rw, "data: {\"choices\":[%s]}\n\n", delta)
		rw.(http.Flusher).Flush()
		return true
	}
	word := func(kind, w string) bool {
		var delta string
		switch {
		case text:
			delta = fmt.Sprintf(`{"text":%q}`, w)
		default:
			delta = fmt.Sprintf(`{"delta":{%q:%q}}`, kind, w)
		}
		if !send(delta) {
			return false
		}
		if m.onToken != nil {
			m.onToken(n, strings.TrimSpace(w), r)
		}
		time.Sleep(m.delay)
		return r.Context().Err() == nil
	}
	if !text {
		send(`{"delta":{"role":"assistant","content":null}}`)
	}
	for i := reasoning; i < m.reasoning; i++ {
		if !word("reasoning_content", fmt.Sprintf("r%d ", i)) {
			return
		}
	}
	for i := content; i < m.words; i++ {
		if !word("content", fmt.Sprintf("w%d ", i)) {
			return
		}
	}
	send(`{"delta":{},"finish_reason":"stop"}`)
	fmt.Fprint(rw, "data: [DONE]\n\n")
}

func (m *model) body(n int) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > len(m.bodies) {
		m.t.Fatalf("attempt %d of %d", n, len(m.bodies))
	}
	return m.bodies[n-1]
}

func (m *model) attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

func pause(r *http.Request, p warden.Pause) {
	warden.PauserOf(r.Context())(p)
}

func newStore(t *testing.T, serve http.Handler) *Store {
	t.Helper()
	s := &Store{Serve: serve, Probe: time.Hour, Logf: t.Logf}
	s.Start()
	t.Cleanup(func() { s.Stop(time.Second) })
	return s
}

func create(t *testing.T, s *Store, path, body string) *Job {
	t.Helper()
	j, status, err := s.Create("episteme", path, json.RawMessage(body), http.Header{"Authorization": {"Bearer batch-key"}})
	if err != nil {
		t.Fatalf("%d %v", status, err)
	}
	return j
}

const chatBody = `{"model":"qwen","messages":[{"role":"user","content":"count"}],"max_tokens":100}`

// until waits for a job's state.
func until(t *testing.T, j *Job, want State) Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := j.Snapshot(false)
		if s.State == want {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %s (%s), want %s", s.State, s.Reason, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// heard is what a client following the job would have: the text of every
// chunk, in order, and the states.
func heard(j *Job) (reasoning, content string, states []string) {
	events, _, _ := j.since(0)
	for _, e := range events {
		switch e.Type {
		case "chunk":
			var c chunk
			json.Unmarshal(e.Data, &c)
			for _, choice := range c.Choices {
				reasoning += choice.Delta.thought()
				content += choice.Delta.Content + choice.Text
			}
		case "state":
			var d stateData
			json.Unmarshal(e.Data, &d)
			states = append(states, string(d.State))
		}
	}
	return
}

func words(prefix string, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s%d ", prefix, i)
	}
	return b.String()
}

func TestAJobRunsToTheEnd(t *testing.T) {
	m := &model{t: t, words: 5, place: "strix"}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	_, content, states := heard(j)
	if content != words("w", 5) {
		t.Fatalf("heard %q", content)
	}
	if strings.Join(states, ",") != "queued,running,done" {
		t.Fatalf("states %v", states)
	}
	if m.body(1)["stream"] != true || m.auth[0] != "Bearer batch-key" {
		t.Fatalf("sent %v with %q", m.body(1), m.auth[0])
	}
}

// A 1xx before the reply, a 100 Continue or a model's 103, is not the
// attempt's answer: the reply after it is.
func TestAJobRunsPastA1xx(t *testing.T) {
	m := &model{t: t, words: 5, place: "strix"}
	s := newStore(t, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusEarlyHints)
		m.ServeHTTP(rw, r)
	}))
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	if _, content, _ := heard(j); content != words("w", 5) {
		t.Fatalf("heard %q", content)
	}
}

func TestAPausedJobContinuesWhereItStopped(t *testing.T) {
	m := &model{t: t, words: 10}
	m.onToken = func(n int, word string, r *http.Request) {
		if n == 1 && word == "w3" {
			pause(r, warden.Pause{Device: warden.DeviceGPU, Reason: "the GPU was yielded: blender"})
		}
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	_, content, states := heard(j)
	if content != words("w", 10) {
		t.Fatalf("heard %q", content)
	}
	if strings.Join(states, ",") != "queued,running,paused,running,done" {
		t.Fatalf("states %v", states)
	}
	next := m.body(2)
	msgs := next["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if len(msgs) != 2 || last["role"] != "assistant" || last["content"] != words("w", 4) {
		t.Fatalf("continued from %v", msgs)
	}
	if next["continue_final_message"] != "content" || next["add_generation_prompt"] != false || next["max_tokens"] != float64(96) {
		t.Fatalf("continued with %v", next)
	}
}

func TestAJobPausedWhileThinkingContinuesTheThought(t *testing.T) {
	m := &model{t: t, reasoning: 6, words: 3}
	m.onToken = func(n int, word string, r *http.Request) {
		if n == 1 && word == "r2" {
			pause(r, warden.Pause{Device: warden.DeviceGPU, Reason: "yielded"})
		}
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	reasoning, content, _ := heard(j)
	if reasoning != words("r", 6) || content != words("w", 3) {
		t.Fatalf("heard %q %q", reasoning, content)
	}
	if m.body(2)["continue_final_message"] != "reasoning_content" {
		t.Fatalf("continued with %v", m.body(2))
	}
}

func TestAClientsOwnPrefillIsTheStartOfTheReply(t *testing.T) {
	m := &model{t: t, words: 6}
	m.onToken = func(n int, word string, r *http.Request) {
		if n == 1 && word == "w3" {
			pause(r, warden.Pause{Reason: "yielded"})
		}
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", `{"model":"qwen","messages":[{"role":"user","content":"count"},{"role":"assistant","content":"w0 w1 "}],"continue_final_message":"content","add_generation_prompt":false}`)
	until(t, j, Done)
	_, content, _ := heard(j)
	if content != "w2 w3 w4 w5 " {
		t.Fatalf("heard %q", content)
	}
	msgs := m.body(2)["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "w0 w1 w2 w3 " {
		t.Fatalf("continued from %v", msgs)
	}
}

func TestACompletionContinuesItsPrompt(t *testing.T) {
	m := &model{t: t, words: 5}
	m.onToken = func(n int, word string, r *http.Request) {
		if n == 1 && word == "w1" {
			pause(r, warden.Pause{Reason: "yielded"})
		}
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/completions", `{"model":"qwen","prompt":"say:"}`)
	until(t, j, Done)
	if _, content, _ := heard(j); content != words("w", 5) {
		t.Fatalf("heard %q", content)
	}
	if m.body(2)["prompt"] != "say:w0 w1 " {
		t.Fatalf("continued from %v", m.body(2)["prompt"])
	}
}

func TestACancelByHandHoldsTheJob(t *testing.T) {
	m := &model{t: t, words: 4}
	var paused, resumed time.Time
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		if n == 2 {
			resumed = time.Now()
		}
		return false
	}
	m.onToken = func(n int, word string, r *http.Request) {
		if n == 1 && word == "w1" {
			paused = time.Now()
			pause(r, warden.Pause{Reason: "cancelled by hand", Hold: 150 * time.Millisecond})
		}
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Paused)
	// A device resuming does not cut the hold short.
	s.Wake()
	until(t, j, Done)
	if held := resumed.Sub(paused); held < 150*time.Millisecond {
		t.Fatalf("sent again after %s", held)
	}
	if _, content, _ := heard(j); content != words("w", 4) {
		t.Fatalf("heard %q", content)
	}
}

func TestARefusedJobWaitsForAWake(t *testing.T) {
	refuse := true
	var mu sync.Mutex
	m := &model{t: t, words: 3}
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if !refuse {
			return false
		}
		rw.Header().Set("Retry-After", "300")
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusServiceUnavailable)
		rw.Write([]byte(`{"error":{"type":"gpu_yielded","message":"batch request refused: blender is running"}}`))
		return true
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	snap := until(t, j, Paused)
	if !strings.Contains(snap.Reason, "blender") {
		t.Fatalf("paused for %q", snap.Reason)
	}
	time.Sleep(50 * time.Millisecond)
	if m.attempts() != 1 {
		t.Fatalf("asked again %d times before a wake", m.attempts()-1)
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	s.Wake()
	until(t, j, Done)
}

func TestTheFirstRefusedJobProbesForItsLine(t *testing.T) {
	var mu sync.Mutex
	refuse := true
	m := &model{t: t, words: 2}
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if !refuse {
			return false
		}
		rw.Header().Set("Retry-After", "1")
		rw.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	s := newStore(t, m)
	s.Probe = 40 * time.Millisecond
	var jobs []*Job
	for range 3 {
		jobs = append(jobs, create(t, s, "/v1/chat/completions", chatBody))
	}
	for _, j := range jobs {
		until(t, j, Paused)
	}
	time.Sleep(130 * time.Millisecond)
	// Three first refusals, then the head alone, about three times.
	if n := m.attempts(); n < 4 || n > 7 {
		t.Fatalf("%d attempts", n)
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	for _, j := range jobs {
		until(t, j, Done)
	}
}

func TestAClientErrorFailsTheJob(t *testing.T) {
	m := &model{t: t}
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusForbidden)
		rw.Write([]byte(`{"detail":"key episteme may not use qwen"}`))
		return true
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Failed)
	events, _, _ := j.since(0)
	last := events[len(events)-1]
	if !strings.Contains(string(last.Data), `"status":403`) || !strings.Contains(string(last.Data), "may not use qwen") {
		t.Fatalf("ended with %s", last.Data)
	}
}

func TestABrokenStreamIsContinuedAndFailsAfterThreeWithoutProgress(t *testing.T) {
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = time.Second })
	m := &model{t: t, words: 6}
	// Attempt 1 breaks after two words; 2 continues and finishes.
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		if n != 1 {
			return false
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"w0 \"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"w1 \"}}]}\n\ndata: {\"choi"))
		return true
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	if _, content, _ := heard(j); content != words("w", 6) {
		t.Fatalf("heard %q", content)
	}

	dead := &model{t: t}
	dead.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		rw.WriteHeader(http.StatusBadGateway)
		return true
	}
	s2 := newStore(t, dead)
	j2 := create(t, s2, "/v1/chat/completions", chatBody)
	snap := until(t, j2, Failed)
	if dead.attempts() != 3 || !strings.Contains(snap.Reason, "502") {
		t.Fatalf("%d attempts, %q", dead.attempts(), snap.Reason)
	}
}

func TestAStreamThatKeepsBreakingWhileItMovesIsNotGivenUp(t *testing.T) {
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = time.Second })
	m := &model{t: t, words: 6}
	// Attempts 1 to 4 each send one more word and break.
	m.onToken = func(n int, word string, r *http.Request) {
		if n <= 4 {
			panic(http.ErrAbortHandler)
		}
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	if _, content, _ := heard(j); content != words("w", 6) || m.attempts() != 5 {
		t.Fatalf("heard %q in %d attempts", content, m.attempts())
	}
}

// TestAnOpenRouterThoughtIsKeptWhenTheStreamBreaks replays a stream
// OpenRouter sent on 2026-10-14, which calls the reasoning reasoning where
// llama-server calls it reasoning_content. Attempts 1 to 3 each break after
// one more piece of the thought; three without progress would fail the job.
func TestAnOpenRouterThoughtIsKeptWhenTheStreamBreaks(t *testing.T) {
	backoffUnit = time.Millisecond
	t.Cleanup(func() { backoffUnit = time.Second })
	raw, err := os.ReadFile("testdata/openrouter-nemotron.sse")
	if err != nil {
		t.Fatal(err)
	}
	// ends[k] is the event after data line k+1, and thoughts[k] the
	// reasoning up to there, read without the package's chunk.
	events := strings.SplitAfter(string(raw), "\n\n")
	var ends []int
	var thoughts []string
	thought, answer := "", ""
	for i, e := range events {
		payload, ok := strings.CutPrefix(strings.TrimSpace(e), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatal(err)
		}
		for _, choice := range c.Choices {
			thought += choice.Delta.Reasoning
			answer += choice.Delta.Content
		}
		ends = append(ends, i+1)
		thoughts = append(thoughts, thought)
	}
	if answer == "" || thoughts[2] == thought {
		t.Fatalf("the capture has no thought past %q before its answer %q", thoughts[2], answer)
	}
	m := &model{t: t}
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		rw.Header().Set("Content-Type", "text/event-stream")
		from, to := 0, len(events)
		if n > 1 {
			from = ends[n-2]
		}
		if n <= 3 {
			to = ends[n-1]
		}
		for _, e := range events[from:to] {
			rw.Write([]byte(e))
		}
		if n <= 3 {
			panic(http.ErrAbortHandler)
		}
		return true
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	if reasoning, content, _ := heard(j); reasoning != thought || content != answer {
		t.Fatalf("heard %q %q", reasoning, content)
	}
	for n := 2; n <= 4; n++ {
		next := m.body(n)
		msgs := next["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		if last["reasoning_content"] != thoughts[n-2] || next["continue_final_message"] != "reasoning_content" || next["max_tokens"] != float64(101-n) {
			t.Fatalf("attempt %d continued with %v", n, next)
		}
	}
}

func TestAToolCallIsSentWholeOrNotAtAll(t *testing.T) {
	call := func(rw http.ResponseWriter, r *http.Request, n int) {
		rw.Header().Set("Content-Type", "text/event-stream")
		send := func(s string) { fmt.Fprintf(rw, "data: %s\n\n", s) }
		if n == 1 {
			send(`{"choices":[{"delta":{"content":"let me look "}}]}`)
		}
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"search","arguments":""}}]}}]}`)
		if n == 1 {
			pause(r, warden.Pause{Reason: "yielded"})
			return
		}
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":1}"}}]}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		send(`[DONE]`)
	}
	m := &model{t: t}
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		call(rw, r, n)
		return true
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	until(t, j, Done)
	events, _, _ := j.since(0)
	calls := 0
	for _, e := range events {
		if strings.Contains(string(e.Data), `"id":"c1"`) {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("the tool call's start was sent %d times", calls)
	}
	last := m.body(2)["messages"].([]any)[1].(map[string]any)
	if last["content"] != "let me look " {
		t.Fatalf("continued from %v", last)
	}
}

func TestCancellingEndsTheJobInFlightOrWaiting(t *testing.T) {
	m := &model{t: t, words: 100, delay: time.Millisecond}
	started := make(chan struct{}, 1)
	m.onToken = func(n int, word string, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/chat/completions", chatBody)
	<-started
	s.Cancel(j)
	until(t, j, Cancelled)

	backoffUnit = time.Hour
	t.Cleanup(func() { backoffUnit = time.Second })
	dead := &model{t: t}
	dead.answer = func(int, http.ResponseWriter, *http.Request) bool { return true }
	s2 := newStore(t, dead)
	j2 := create(t, s2, "/v1/chat/completions", chatBody)
	until(t, j2, Queued)
	for dead.attempts() < 1 {
		time.Sleep(time.Millisecond)
	}
	s2.Cancel(j2)
	until(t, j2, Cancelled)
}

func TestAnEmbeddingIsNeverPausedInside(t *testing.T) {
	m := &model{t: t}
	m.answer = func(n int, rw http.ResponseWriter, r *http.Request) bool {
		pause(r, warden.Pause{Reason: "yielded"})
		if r.Context().Err() != nil {
			t.Error("the pause stopped an embedding")
		}
		return false
	}
	s := newStore(t, m)
	j := create(t, s, "/v1/embeddings", `{"model":"embed","input":"x"}`)
	until(t, j, Done)
	events, _, _ := j.since(0)
	var got responseData
	for _, e := range events {
		if e.Type == "response" {
			json.Unmarshal(e.Data, &got)
		}
	}
	if got.Status != 200 || string(got.Body) != `{"data":[{"embedding":[0.5,0.25]}]}` {
		t.Fatalf("response %+v", got)
	}
}

func TestWhatAJobCannotContinueIsRefused(t *testing.T) {
	s := newStore(t, &model{t: t})
	for path, body := range map[string]string{
		"/v1/chat/completions": `{"model":"qwen","messages":[{"role":"user","content":"x"}],"n":2}`,
		"/v1/completions":      `{"model":"qwen","prompt":["a","b"]}`,
		"/v1/audio/speech":     `{"model":"tts","input":"x"}`,
		"/v1/embeddings":       `{"input":"x"}`,
	} {
		if _, status, err := s.Create("c", path, json.RawMessage(body), nil); status != http.StatusBadRequest {
			t.Errorf("%s %s: %d %v", path, body, status, err)
		}
	}
	s.Limit = 1
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	s.Serve = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
	})
	create(t, s, "/v1/chat/completions", chatBody)
	if _, status, _ := s.Create("c", "/v1/chat/completions", json.RawMessage(chatBody), nil); status != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", status)
	}
}

func TestAShutdownWritesTheJobsAndTheyGoOnAfterTheRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "jobs.json")
	m := &model{t: t, words: 8}
	stopped := make(chan struct{})
	m.onToken = func(n int, word string, r *http.Request) {
		if n == 1 && word == "w2" {
			close(stopped)
			<-r.Context().Done()
		}
	}
	keys := func() bool { return true }
	s := &Store{Serve: m, File: file, KeysRequired: keys, Logf: t.Logf}
	s.Start()
	j := create(t, s, "/v1/chat/completions", chatBody)
	<-stopped
	if err := s.Stop(time.Second); err != nil {
		t.Fatal(err)
	}
	if snap := j.Snapshot(true); snap.State != Paused {
		t.Fatalf("stopped as %+v", snap)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "batch-key") {
		t.Fatal("the key was written to disk")
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}

	after := &Store{Serve: m, File: file, KeysRequired: keys, Logf: t.Logf}
	after.Start()
	t.Cleanup(func() { after.Stop(time.Second) })
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("the file is still there after it was read")
	}
	back := after.Get(j.id, "episteme")
	if back == nil {
		t.Fatal("not read back")
	}
	time.Sleep(30 * time.Millisecond)
	if snap := back.Snapshot(true); !snap.WaitingForKey || m.attempts() != 1 {
		t.Fatalf("went without its key: %+v, %d attempts", snap, m.attempts())
	}
	after.Rekey(back, http.Header{"Authorization": {"Bearer batch-key"}})
	until(t, back, Done)
	if _, content, _ := heard(back); content != words("w", 8) {
		t.Fatalf("heard %q", content)
	}
	if m.auth[1] != "Bearer batch-key" {
		t.Fatalf("continued with %q", m.auth[1])
	}
}

// A deploy's drain (0026): the attempt running finishes, and no new one
// starts until the stop.
func TestADrainLetsTheRunningAttemptFinishAndStartsNoOther(t *testing.T) {
	inside := make(chan struct{})
	release := make(chan struct{})
	m := &model{t: t, words: 5}
	m.onToken = func(n int, word string, r *http.Request) {
		if n == 1 && word == "w2" {
			close(inside)
			<-release
		}
	}
	s := newStore(t, m)
	running := create(t, s, "/v1/chat/completions", chatBody)
	<-inside
	s.Drain()
	waiting := create(t, s, "/v1/chat/completions", chatBody)
	if s.Attempting() != 1 {
		t.Fatalf("attempting %d", s.Attempting())
	}
	close(release)
	until(t, running, Done)
	if _, content, _ := heard(running); content != words("w", 5) {
		t.Fatalf("heard %q", content)
	}
	time.Sleep(30 * time.Millisecond)
	if s.Attempting() != 0 || m.attempts() != 1 || waiting.Snapshot(false).State != Queued {
		t.Fatalf("attempting %d, %d attempts, the new job %s", s.Attempting(), m.attempts(), waiting.Snapshot(false).State)
	}
}

// sol, review 11: a runner that wait let go, held off the CPU until Drain
// returned and the stop saw no attempt running, starts none after it.
func TestADrainBetweenWaitAndAttemptStartsNothing(t *testing.T) {
	calls := 0
	s := newStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[]}`))
	}))
	// No runner of its own: the test is the runner, one step at a time.
	j := &Job{id: "r11", client: "episteme", path: "/v1/chat/completions", model: "qwen", kind: chat, request: []byte(chatBody), auth: http.Header{}, state: Queued, changed: make(chan struct{}), nudge: make(chan struct{}, 1)}
	if !s.wait(j) {
		t.Fatal("the job may not go")
	}
	s.Drain()
	if !drain.Wait(time.Second, s.Attempting) {
		t.Fatalf("attempting %d after Drain", s.Attempting())
	}
	goes := s.attempt(j)
	if calls != 0 {
		t.Fatalf("%d attempt(s) sent after the drain saw none running", calls)
	}
	if !goes {
		t.Fatal("the runner stopped instead of waiting for Stop")
	}
}

// sol, review 12: Drain does not return while a runner is between begin's
// check and its count, so the stop that waits on Attempting sees the attempt.
func TestADrainWaitsForTheRunnerBetweenCheckAndCount(t *testing.T) {
	inside, release := make(chan struct{}), make(chan struct{})
	checked = func() { close(inside); <-release }
	t.Cleanup(func() { checked = nil })
	s := &Store{}
	begun := make(chan bool, 1)
	go func() { begun <- s.begin() }()
	<-inside
	drained := make(chan struct{})
	go func() { s.Drain(); close(drained) }()
	select {
	case <-drained:
		n := s.Attempting()
		close(release)
		<-begun
		t.Fatalf("Drain returned with the runner between check and count, attempting %d", n)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if !<-begun {
		t.Fatal("the runner checked before Drain was refused")
	}
	<-drained
	if s.Attempting() != 1 {
		t.Fatalf("attempting %d after Drain", s.Attempting())
	}
}

// The API, through the warden's key check, which names the client.
func api(t *testing.T, s *Store) *httptest.Server {
	s.Client = func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTheAPICreatesListsFollowsAndCancels(t *testing.T) {
	m := &model{t: t, words: 3}
	s := newStore(t, m)
	srv := api(t, s)
	resp := call(t, srv, "POST", "/warden/jobs", "episteme", `{"path":"/v1/chat/completions","body":`+chatBody+`}`)
	var snap Snapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/warden/jobs/"+snap.ID {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	if resp := call(t, srv, "GET", "/warden/jobs/"+snap.ID, "someone-else", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another key saw the job: %d", resp.StatusCode)
	}
	var list struct{ Jobs []Snapshot }
	json.NewDecoder(call(t, srv, "GET", "/warden/jobs", "episteme", "").Body).Decode(&list)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != snap.ID {
		t.Fatalf("listed %+v", list)
	}

	stream := call(t, srv, "GET", "/warden/jobs/"+snap.ID+"/events", "episteme", "")
	if stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events as %s", stream.Header.Get("Content-Type"))
	}
	var ids, types []string
	sc := bufio.NewScanner(stream.Body)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
			ids = append(ids, v)
		}
		if v, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			types = append(types, v)
		}
	}
	// queued, running, the role chunk, three words, the finish, done.
	if strings.Join(types, ",") != "state,state,chunk,chunk,chunk,chunk,chunk,state" || ids[0] != "1" {
		t.Fatalf("events %v %v", ids, types)
	}
	// Reconnecting after the fifth gets the rest.
	again := call(t, srv, "GET", "/warden/jobs/"+snap.ID+"/events?after=5", "episteme", "")
	sc = bufio.NewScanner(again.Body)
	var first string
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "id: "); ok && first == "" {
			first = v
		}
	}
	if first != "6" {
		t.Fatalf("after 5 began at %s", first)
	}

	slow := &model{t: t, words: 1000, delay: time.Millisecond}
	s.Serve = slow
	resp = call(t, srv, "POST", "/warden/jobs", "episteme", `{"path":"/v1/chat/completions","body":`+chatBody+`}`)
	json.NewDecoder(resp.Body).Decode(&snap)
	if resp := call(t, srv, "DELETE", "/warden/jobs/"+snap.ID, "episteme", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	until(t, s.Get(snap.ID, "episteme"), Cancelled)
}
