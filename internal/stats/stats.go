// Package stats times each request this host serves a model for, as the
// client sees it, beside what llama-server says about it: time to the first
// generated token, prefill and decode rates, the prompt cache and the
// drafts accepted. The last requests are kept in memory and summarised per
// model.
//
// llama-swap keeps an activity log of its own, but it records no time to the
// first token and drops llama-server's prompt_ms, and its entries are built
// inside upstream's code after the response ends. This is InferMux's own
// handler, so no upstream file changes.
package stats

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Request is one request, as recorded. A nil number was not reported: a
// reply that was not streamed has no time to its first token, and a cloud
// peer sends no timings.
type Request struct {
	ID         uint64    `json:"id"`
	Time       time.Time `json:"time"`
	Model      string    `json:"model"`
	Client     string    `json:"client"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	Stream     bool      `json:"stream"`
	DurationMs float64   `json:"duration_ms"`
	// TTFTMs is from the request's arrival to the first event that carries
	// generated text, reasoning or a tool call: model load, a swap and the
	// queue included.
	TTFTMs *float64 `json:"ttft_ms"`
	// PrefillMs is llama-server's prompt_ms, the prompt alone. TTFT minus
	// prefill is the time spent before it: loading, swapping, waiting.
	PrefillMs        *float64 `json:"prefill_ms"`
	PromptTokens     *int     `json:"prompt_tokens"`
	CachedTokens     *int     `json:"cached_tokens"`
	OutputTokens     *int     `json:"output_tokens"`
	PrefillPerSecond *float64 `json:"prefill_per_second"`
	DecodePerSecond  *float64 `json:"decode_per_second"`
	// RatesFrom is "llama-server" when the rates are its own timings, and
	// "client" when the decode rate is the output tokens over the time
	// after the first token, as for a cloud peer.
	RatesFrom     string `json:"rates_from,omitempty"`
	DraftTokens   *int   `json:"draft_tokens"`
	DraftAccepted *int   `json:"draft_accepted"`
}

// Recorder keeps the last Capacity requests.
type Recorder struct {
	Capacity int
	// Model names the model a request is for, as a key's allow list sees it,
	// and whether this host serves it, itself or through a cloud peer. A
	// request for another host's model is recorded there.
	Model func(r *http.Request) (model string, here bool)
	// Client names the key the request came with.
	Client func(r *http.Request) string

	mu       sync.Mutex
	next     uint64
	requests []Request
	now      func() time.Time
}

func (rec *Recorder) clock() time.Time {
	if rec.now != nil {
		return rec.now()
	}
	return time.Now()
}

// Recent is every kept request, newest first.
func (rec *Recorder) Recent() []Request {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := slices.Clone(rec.requests)
	slices.Reverse(out)
	return out
}

func (rec *Recorder) add(r Request) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.next++
	r.ID = rec.next
	rec.requests = append(rec.requests, r)
	if limit := max(rec.Capacity, 1); len(rec.requests) > limit {
		rec.requests = slices.Delete(rec.requests, 0, len(rec.requests)-limit)
	}
}

// Wrap records every POST for a model this host serves.
func (rec *Recorder) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/warden/") {
			next.ServeHTTP(rw, r)
			return
		}
		model, here := rec.Model(r)
		if !here {
			next.ServeHTTP(rw, r)
			return
		}
		w := &watcher{ResponseWriter: rw, start: rec.clock(), now: rec.clock}
		next.ServeHTTP(w, r)
		client := ""
		if rec.Client != nil {
			client = rec.Client(r)
		}
		rec.add(w.request(model, client, r.URL.Path))
	})
}

// bodyLimit is how much of a reply that is not a stream is kept to read its
// usage and timings from. An embedding of a long batch can be larger; its
// rates are then not reported.
const bodyLimit = 8 << 20

// watcher passes the reply on unchanged and reads it as it goes.
type watcher struct {
	http.ResponseWriter
	start, firstToken time.Time
	now               func() time.Time
	status            int
	stream            bool
	started           bool
	line              []byte // a stream's unfinished line
	body              bytes.Buffer
	overflow          bool
	timings           *timings
	usage             *usage
}

func (w *watcher) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *watcher) Write(b []byte) (int, error) {
	if !w.started {
		w.started = true
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.stream = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	}
	if w.stream {
		w.read(b)
	} else if !w.overflow {
		if w.body.Len()+len(b) > bodyLimit {
			w.overflow = true
			w.body = bytes.Buffer{}
		} else {
			w.body.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *watcher) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection beneath.
func (w *watcher) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// read takes a stream's bytes one line at a time.
func (w *watcher) read(b []byte) {
	w.line = append(w.line, b...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			return
		}
		line := bytes.TrimRight(w.line[:i], "\r")
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			w.event(bytes.TrimSpace(data))
		}
		w.line = w.line[i+1:]
	}
}

// event is one server-sent event's data.
func (w *watcher) event(data []byte) {
	if len(data) == 0 || data[0] != '{' {
		return // [DONE]
	}
	if w.firstToken.IsZero() && generated(data) {
		w.firstToken = w.now()
	}
	if bytes.Contains(data, []byte(`"timings"`)) || bytes.Contains(data, []byte(`"usage"`)) {
		w.report(data)
	}
}

// generated is whether an event carries output: a chat chunk with content,
// reasoning or a tool call, a Responses API delta, or llama-server's own
// /completion chunk. The events before the first token, such as the role
// chunk and response.created, carry none.
func generated(data []byte) bool {
	var e struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		Choices []struct {
			Text  string `json:"text"`
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &e) != nil {
		return false
	}
	if strings.HasSuffix(e.Type, ".delta") || e.Content != "" {
		return true
	}
	for _, c := range e.Choices {
		d := c.Delta
		if c.Text != "" || d.Content != "" || d.ReasoningContent != "" || d.Reasoning != "" || len(d.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// timings is llama-server's own account of a request.
type timings struct {
	PromptN            *int     `json:"prompt_n"`
	PromptMs           *float64 `json:"prompt_ms"`
	PromptPerSecond    *float64 `json:"prompt_per_second"`
	PredictedN         *int     `json:"predicted_n"`
	PredictedPerSecond *float64 `json:"predicted_per_second"`
	CacheN             *int     `json:"cache_n"`
	DraftN             *int     `json:"draft_n"`
	DraftNAccepted     *int     `json:"draft_n_accepted"`
}

// usage is the OpenAI usage block, in the Chat Completions and the
// Responses API's names.
type usage struct {
	PromptTokens        *int `json:"prompt_tokens"`
	CompletionTokens    *int `json:"completion_tokens"`
	InputTokens         *int `json:"input_tokens"`
	OutputTokens        *int `json:"output_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// report keeps the last timings and usage a reply carries. The Responses
// API puts usage inside response.
func (w *watcher) report(data []byte) {
	var r struct {
		Timings  *timings `json:"timings"`
		Usage    *usage   `json:"usage"`
		Response *struct {
			Usage *usage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &r) != nil {
		return
	}
	if r.Timings != nil {
		w.timings = r.Timings
	}
	if r.Usage != nil {
		w.usage = r.Usage
	} else if r.Response != nil && r.Response.Usage != nil {
		w.usage = r.Response.Usage
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// request is what the reply showed, once it has ended.
func (w *watcher) request(model, client, path string) Request {
	end := w.now()
	if !w.stream && !w.overflow && w.body.Len() > 0 {
		w.report(w.body.Bytes())
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	out := Request{Time: w.start, Model: model, Client: client, Path: path, Status: status, Stream: w.stream, DurationMs: ms(end.Sub(w.start))}
	if !w.firstToken.IsZero() {
		ttft := ms(w.firstToken.Sub(w.start))
		out.TTFTMs = &ttft
	}
	if u := w.usage; u != nil {
		out.PromptTokens = first(u.PromptTokens, u.InputTokens)
		out.OutputTokens = first(u.CompletionTokens, u.OutputTokens)
		if d := u.PromptTokensDetails; d != nil {
			out.CachedTokens = d.CachedTokens
		} else if d := u.InputTokensDetails; d != nil {
			out.CachedTokens = d.CachedTokens
		}
	}
	if t := w.timings; t != nil {
		out.RatesFrom = "llama-server"
		out.PrefillMs = t.PromptMs
		out.PrefillPerSecond = t.PromptPerSecond
		out.DecodePerSecond = t.PredictedPerSecond
		if t.PredictedN != nil {
			out.OutputTokens = t.PredictedN
		}
		// prompt_n counts only the tokens processed; the cached ones come
		// on top of it.
		if t.PromptN != nil {
			n := *t.PromptN
			if t.CacheN != nil {
				n += *t.CacheN
			}
			out.PromptTokens = &n
		}
		if t.CacheN != nil {
			out.CachedTokens = t.CacheN
		}
		if t.DraftN != nil && t.DraftNAccepted != nil {
			out.DraftTokens, out.DraftAccepted = t.DraftN, t.DraftNAccepted
		}
	} else if out.TTFTMs != nil && out.OutputTokens != nil && *out.OutputTokens > 1 {
		if after := end.Sub(w.firstToken).Seconds(); after > 0 {
			rate := float64(*out.OutputTokens-1) / after
			out.DecodePerSecond = &rate
			out.RatesFrom = "client"
		}
	}
	return out
}

func first(a, b *int) *int {
	if a != nil {
		return a
	}
	return b
}
