package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/failover"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// tries is how many attempts in a row may fail with no progress before the
// job fails, the next waiting twice backoffUnit, then four times.
const tries = 3

var backoffUnit = time.Second

// run is a job's runner: it waits until the job may go, sends an attempt,
// and reads what the attempt's end means, until the job ends or the store
// stops.
func (s *Store) run(j *Job) {
	defer s.runners.Done()
	defer s.leaveLine(j)
	for s.wait(j) && s.attempt(j) {
	}
}

// wait blocks until the job may send its next attempt. It is false when the
// job ended or the store stops.
func (s *Store) wait(j *Job) bool {
	for {
		j.mu.Lock()
		if j.state.final() {
			j.mu.Unlock()
			return false
		}
		var timer <-chan time.Time
		now := s.now()
		switch {
		case j.auth == nil && s.keysRequired():
			// Read back after a restart: the client's key comes with its
			// next request for the job (Rekey).
		case now.Before(j.notBefore):
			timer = time.After(j.notBefore.Sub(now))
		case j.waiting && !s.head(j):
			// Behind another job of the same model and client, which asks
			// for both.
		case j.waiting && now.Before(j.retryAt):
			timer = time.After(j.retryAt.Sub(now))
		default:
			j.waiting = false
			j.mu.Unlock()
			return true
		}
		j.mu.Unlock()
		select {
		case <-s.base.Done():
			return false
		case <-j.nudge:
		case <-timer:
		}
	}
}

// attempt is one try at the rest of the job's reply. It is false when the
// runner should stop.
func (s *Store) attempt(j *Job) bool {
	ctx, stop := context.WithCancelCause(s.base)
	defer stop(nil)
	j.mu.Lock()
	if j.state.final() {
		j.mu.Unlock()
		return false
	}
	body, err := j.body()
	if err != nil {
		j.finish(Failed, "the request could not be continued: "+err.Error(), 0, nil)
		j.mu.Unlock()
		return false
	}
	j.attempts++
	j.stop = stop
	auth := j.auth.Clone()
	j.mu.Unlock()

	if j.kind == whole {
		// Seconds of one forward pass: done whole rather than stopped.
		ctx = warden.WithPauser(ctx, func(warden.Pause) {})
	} else {
		// Only the first cause counts, so a late call from an attempt
		// that already ended changes nothing.
		ctx = warden.WithPauser(ctx, func(p warden.Pause) {
			stop(&pauseCause{reason: p.Reason, hold: p.Hold})
		})
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, j.path, bytes.NewReader(body))
	for k, v := range auth {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:0"
	out := &attemptWriter{job: j, header: http.Header{}, store: s}
	broke := serve(s.Serve, out, req)
	cause := context.Cause(ctx)

	j.mu.Lock()
	defer j.mu.Unlock()
	j.stop = nil
	answered := out.status == http.StatusOK && !out.streaming() && broke == nil && json.Valid(out.body.Bytes())
	var paused *pauseCause
	switch {
	case out.complete || answered:
		if answered {
			j.emit("response", responseData{Status: out.status, Body: compact(out.body.Bytes())})
		}
		j.finish(Done, "", 0, nil)
		s.wakeModel(j.model)
		return false
	case j.cancelled:
		j.finish(Cancelled, "cancelled by its client", 0, nil)
		s.wakeModel(j.model)
		return false
	case errors.Is(cause, errStopping):
		j.held = nil
		j.setState(Paused, "", errStopping.Error())
		return false
	case errors.As(cause, &paused):
		j.failures = 0
		j.held = nil
		j.notBefore = s.now().Add(paused.hold)
		j.setState(Paused, "", paused.reason)
		return true
	}
	status := out.status
	switch {
	case status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests:
		// Refused: yielded, or every place down or full. It asks again when
		// a device here resumes, and on its own at the Retry-After, at most
		// the probe interval apart.
		retry := s.probe()
		if secs, err := strconv.Atoi(out.header.Get("Retry-After")); err == nil && secs >= 1 {
			retry = min(retry, time.Duration(secs)*time.Second)
		}
		j.failures = 0
		j.waiting = true
		j.retryAt = s.now().Add(retry)
		s.enterLine(j)
		j.setState(Paused, "", refusal(out))
		return true
	case status >= 400 && status < 500:
		j.finish(Failed, fmt.Sprintf("refused with %d", status), status, out.body.Bytes())
		s.wakeModel(j.model)
		return false
	}
	// A 5xx, or an answer that broke off: tried again, from the reply so
	// far. A yield on another host ends here too, since its warden cannot
	// tell this job.
	why := fmt.Sprintf("the place answered %d", status)
	if status < 400 {
		why = "the answer broke off"
		if broke != nil {
			why += ": " + broke.Error()
		}
	}
	if out.progress {
		j.failures = 0
	}
	j.failures++
	j.held = nil
	if j.failures >= tries {
		j.finish(Failed, fmt.Sprintf("%s, %d attempts in a row", why, j.failures), status, out.body.Bytes())
		s.wakeModel(j.model)
		return false
	}
	backoff := time.Duration(1<<j.failures) * backoffUnit
	j.notBefore = s.now().Add(backoff)
	j.setState(Queued, "", fmt.Sprintf("%s; trying again in %s", why, backoff))
	return true
}

// serve calls the stack in this goroutine. A reverse proxy whose stream
// breaks may panic with http.ErrAbortHandler, as it would for a client; that
// and any other panic is the attempt breaking.
func serve(h http.Handler, rw http.ResponseWriter, r *http.Request) (broke error) {
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				broke = errors.New("aborted")
				return
			}
			broke = fmt.Errorf("panic: %v", p)
		}
	}()
	h.ServeHTTP(rw, r)
	return nil
}

// refusal is the reason a refused attempt gave, from an OpenAI error or
// llama-swap's plain text.
func refusal(out *attemptWriter) string {
	raw := out.body.Bytes()
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch v := e.Error.(type) {
		case string:
			return v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return m
			}
		}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return fmt.Sprintf("refused with %d", out.status)
	}
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

// body is the next attempt's request: the client's, streamed, continuing the
// reply so far. Under j.mu.
func (j *Job) body() ([]byte, error) {
	if j.kind == whole {
		return j.request, nil
	}
	dec := json.NewDecoder(bytes.NewReader(j.request))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	req["stream"] = true
	r := j.reply
	if r.Tokens > 0 {
		for _, key := range []string{"max_tokens", "max_completion_tokens", "n_predict"} {
			if n, ok := req[key].(json.Number); ok {
				if limit, err := n.Int64(); err == nil && limit > 0 {
					req[key] = max(1, limit-int64(r.Tokens))
				}
			}
		}
	}
	switch j.kind {
	case completion:
		if r.Text != "" {
			prompt, _ := req["prompt"].(string)
			req["prompt"] = prompt + r.Text
		}
	case chat:
		if r.Reasoning != "" || r.Content != "" {
			messages, _ := req["messages"].([]any)
			prefill := map[string]any{"role": "assistant"}
			// A client's own prefill is the start of the reply: what the
			// model wrote follows it.
			if n := len(messages); n > 0 {
				if last, ok := messages[n-1].(map[string]any); ok && last["role"] == "assistant" {
					prefill = last
					messages = messages[:n-1]
				}
			}
			req["messages"] = append(append([]any{}, messages...), joined(prefill, r))
			req["add_generation_prompt"] = false
			if r.Content != "" {
				req["continue_final_message"] = "content"
			} else {
				req["continue_final_message"] = "reasoning_content"
			}
		}
	}
	return json.Marshal(req)
}

// joined is a copy of the assistant message with the reply so far appended.
// It always has a content, if only "": llama-server refuses an assistant
// message with neither content nor tool calls, even to continue its
// reasoning.
func joined(msg map[string]any, r reply) map[string]any {
	out := make(map[string]any, len(msg)+2)
	out["content"] = ""
	for k, v := range msg {
		out[k] = v
	}
	if r.Reasoning != "" {
		before, _ := out["reasoning_content"].(string)
		out["reasoning_content"] = before + r.Reasoning
	}
	if r.Content != "" {
		before, _ := out["content"].(string)
		out["content"] = before + r.Content
	}
	return out
}

// attemptWriter is the ResponseWriter of an attempt. It reads a stream as it
// comes, adds each chunk to the job's events and its reply, and keeps the
// body of any other answer.
type attemptWriter struct {
	job    *Job
	store  *Store
	header http.Header
	status int
	// line is a stream's unfinished last line, dropped if the stream stops
	// there: the client is sent whole chunks only.
	line []byte
	// body is a whole answer, or the start of a refusal's.
	body     bytes.Buffer
	complete bool
	// progress is a token added to the reply in this attempt.
	progress bool
}

func (w *attemptWriter) Header() http.Header { return w.header }

func (w *attemptWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status != http.StatusOK {
		return
	}
	j := w.job
	j.mu.Lock()
	j.setState(Running, w.header.Get(failover.PlaceHeader), "")
	j.mu.Unlock()
	w.store.leaveLine(j)
}

func (w *attemptWriter) streaming() bool {
	return w.status == http.StatusOK && w.job.kind != whole && strings.HasPrefix(w.header.Get("Content-Type"), "text/event-stream")
}

func (w *attemptWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if !w.streaming() {
		if w.status == http.StatusOK || w.body.Len() < 64<<10 {
			w.body.Write(b)
		}
		return len(b), nil
	}
	w.line = append(w.line, b...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			break
		}
		w.event(bytes.TrimRight(w.line[:i], "\r"))
		w.line = w.line[i+1:]
	}
	return len(b), nil
}

func (w *attemptWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
}

// chunk is the part of an OpenAI stream chunk a job reads.
type chunk struct {
	Choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content   string          `json:"content"`
			Reasoning string          `json:"reasoning_content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// event reads one line of the stream.
func (w *attemptWriter) event(line []byte) {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if string(data) == "[DONE]" {
		w.complete = true
		return
	}
	var c chunk
	if json.Unmarshal(data, &c) != nil {
		return
	}
	finished, calling := false, false
	for _, choice := range c.Choices {
		finished = finished || choice.FinishReason != nil
		calling = calling || (len(choice.Delta.ToolCalls) > 0 && string(choice.Delta.ToolCalls) != "null")
	}
	j := w.job
	j.mu.Lock()
	defer j.mu.Unlock()
	raw := append([]byte(nil), data...)
	// A tool call is sent whole: a pause before its end drops it, and the
	// model writes it again from the text so far.
	if calling || len(j.held) > 0 {
		j.held = append(j.held, raw)
		if !finished {
			return
		}
		held := j.held
		j.held = nil
		for _, h := range held {
			w.add(h)
		}
	} else {
		w.add(raw)
	}
	if finished {
		w.complete = true
	}
}

// add sends a chunk to the client and adds its text to the reply. Under
// j.mu.
func (w *attemptWriter) add(raw []byte) {
	var c chunk
	json.Unmarshal(raw, &c)
	j := w.job
	for _, choice := range c.Choices {
		d := choice.Delta
		if choice.Text == "" && d.Content == "" && d.Reasoning == "" {
			continue
		}
		j.reply.Text += choice.Text
		j.reply.Content += d.Content
		j.reply.Reasoning += d.Reasoning
		j.reply.Tokens++
		w.progress = true
	}
	j.emit("chunk", compact(raw))
}

// A line is the order in which jobs refused for the same model and client
// ask again: only the first asks on its own, so a thousand refused jobs are
// one request each probe interval, and the others go when it is let
// through.
func lineKey(j *Job) string { return j.model + "\x00" + j.client }

func (s *Store) enterLine(j *Job) {
	s.lineMu.Lock()
	defer s.lineMu.Unlock()
	if s.lines == nil {
		s.lines = map[string][]*Job{}
	}
	key := lineKey(j)
	for _, other := range s.lines[key] {
		if other == j {
			return
		}
	}
	s.lines[key] = append(s.lines[key], j)
}

// head is whether j is first in its line, or in none.
func (s *Store) head(j *Job) bool {
	s.lineMu.Lock()
	defer s.lineMu.Unlock()
	line := s.lines[lineKey(j)]
	return len(line) == 0 || line[0] == j
}

// leaveLine takes j out of its line. A job let through means the others in
// its line likely would be too, so they all ask again.
func (s *Store) leaveLine(j *Job) {
	s.lineMu.Lock()
	key := lineKey(j)
	line := s.lines[key]
	i := -1
	for n, other := range line {
		if other == j {
			i = n
		}
	}
	if i < 0 {
		s.lineMu.Unlock()
		return
	}
	rest := append(append([]*Job{}, line[:i]...), line[i+1:]...)
	if len(rest) == 0 {
		delete(s.lines, key)
	} else {
		s.lines[key] = rest
	}
	s.lineMu.Unlock()
	for _, other := range rest {
		other.wake()
	}
}

// wakeModel lets the first job in each of a model's lines ask again: a job
// of the model ended, so a place may be free. It may be called under a
// job's mu, so the waking is done apart.
func (s *Store) wakeModel(model string) {
	s.lineMu.Lock()
	var heads []*Job
	for _, line := range s.lines {
		if len(line) > 0 && line[0].model == model {
			heads = append(heads, line[0])
		}
	}
	s.lineMu.Unlock()
	if len(heads) > 0 {
		go func() {
			for _, h := range heads {
				h.wake()
			}
		}()
	}
}

// wake ends a refused job's wait.
func (j *Job) wake() {
	j.mu.Lock()
	was := j.waiting
	j.waiting = false
	j.mu.Unlock()
	if was {
		j.poke()
	}
}
