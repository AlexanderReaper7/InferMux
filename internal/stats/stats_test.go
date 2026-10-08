package stats

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clock moves only when the test says so, so each event's time is known.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) wait(ms int)    { c.t = c.t.Add(time.Duration(ms) * time.Millisecond) }
func ptr[T any](v T) *T         { return &v }
func val[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func recorder(c *clock) *Recorder {
	return &Recorder{
		Capacity: 10,
		Model: func(r *http.Request) (string, bool) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "zbox/") {
				return "", false
			}
			return "reaperboi/qwen", true
		},
		Client: func(*http.Request) string { return "opencode" },
		now:    c.now,
	}
}

// reply is a model server that writes each part after a wait.
type part struct {
	wait int
	text string
}

func serve(rec *Recorder, c *clock, contentType, body string, parts []part) {
	h := rec.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", contentType)
		for _, p := range parts {
			c.wait(p.wait)
			io.WriteString(rw, p.text)
			rw.(http.Flusher).Flush()
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
}

const timingsJSON = `"timings":{"cache_n":30,"prompt_n":70,"prompt_ms":140,"prompt_per_second":500,"predicted_n":40,"predicted_ms":800,"predicted_per_second":50,"draft_n":20,"draft_n_accepted":15}`

func TestAStreamsFirstTokenIsTheFirstEventWithOutput(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "text/event-stream", `{"model":"qwen"}`, []part{
		// The role chunk comes at once, before the prompt is read.
		{10, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null}}]}\n\n"},
		{3000, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Hm\"}}]}\n\n"},
		// An event split across two writes.
		{50, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n"},
		{0, "\ndata: {\"choices\":[{\"delta\":{}}]," + timingsJSON + "}\n\ndata: [DONE]\n\n"},
	})
	got := rec.Recent()
	if len(got) != 1 {
		t.Fatalf("%d requests", len(got))
	}
	r := got[0]
	if r.Model != "reaperboi/qwen" || r.Client != "opencode" || r.Status != 200 || !r.Stream {
		t.Fatalf("%+v", r)
	}
	if val(r.TTFTMs) != 3010.0 || r.DurationMs != 3060 {
		t.Errorf("ttft %v duration %v, want the reasoning chunk at 3010 and the end at 3060", val(r.TTFTMs), r.DurationMs)
	}
	if val(r.PrefillMs) != 140.0 || val(r.PrefillPerSecond) != 500.0 || val(r.DecodePerSecond) != 50.0 || r.RatesFrom != "llama-server" {
		t.Errorf("rates %v %v %v %q", val(r.PrefillMs), val(r.PrefillPerSecond), val(r.DecodePerSecond), r.RatesFrom)
	}
	// prompt_n is what was processed; the cached tokens come on top.
	if val(r.PromptTokens) != 100 || val(r.CachedTokens) != 30 || val(r.OutputTokens) != 40 || val(r.DraftTokens) != 20 || val(r.DraftAccepted) != 15 {
		t.Errorf("tokens %v %v %v drafts %v %v", val(r.PromptTokens), val(r.CachedTokens), val(r.OutputTokens), val(r.DraftTokens), val(r.DraftAccepted))
	}
}

func TestTheResponsesAPIsFirstTokenIsADelta(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "text/event-stream; charset=utf-8", `{"model":"qwen"}`, []part{
		{5, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n"},
		{5, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\n"},
		{700, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"Hm\"}\n\n"},
		{100, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":97,\"output_tokens\":183,\"input_tokens_details\":{\"cached_tokens\":0}}},\"timings\":{\"cache_n\":0,\"prompt_n\":97,\"prompt_ms\":139.3,\"prompt_per_second\":696.3,\"predicted_n\":183,\"predicted_per_second\":65.9}}\n\n"},
	})
	r := rec.Recent()[0]
	if val(r.TTFTMs) != 710.0 || val(r.PromptTokens) != 97 || val(r.OutputTokens) != 183 || val(r.DecodePerSecond) != 65.9 {
		t.Errorf("ttft %v prompt %v output %v decode %v", val(r.TTFTMs), val(r.PromptTokens), val(r.OutputTokens), val(r.DecodePerSecond))
	}
}

func TestAReplyThatIsNotStreamedHasNoFirstToken(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "application/json", `{"model":"qwen"}`, []part{
		{900, `{"choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":3},` + timingsJSON + `}`},
	})
	r := rec.Recent()[0]
	if r.TTFTMs != nil || r.Stream || val(r.PrefillMs) != 140.0 || val(r.PromptTokens) != 100 {
		t.Errorf("%+v", r)
	}
}

func TestBothBodiesAreCountedOnce(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	rec.Model = func(r *http.Request) (string, bool) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		return "reaperboi/qwen", true
	}
	request := `{"model":"qwen","messages":[{"role":"user","content":"` + strings.Repeat("x", 5000) + `"}]}`
	h := rec.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(rw, "data: [DONE]\n\n")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(request)))
	r := rec.Recent()[0]
	if r.RequestBytes != int64(len(request)) || r.ResponseBytes != 62 {
		t.Errorf("request %d of %d bytes, response %d of 62", r.RequestBytes, len(request), r.ResponseBytes)
	}
}

// OpenRouter sends usage and no timings: the decode rate is the client's,
// the tokens after the first over the time after it.
func TestACloudReplysRateIsTheClients(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "text/event-stream", `{"model":"openrouter/free"}`, []part{
		{400, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"},
		{2000, "data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":101}}\n\n"},
	})
	r := rec.Recent()[0]
	if val(r.TTFTMs) != 400.0 || val(r.DecodePerSecond) != 50.0 || r.RatesFrom != "client" || r.PrefillMs != nil {
		t.Errorf("ttft %v decode %v from %q prefill %v", val(r.TTFTMs), val(r.DecodePerSecond), r.RatesFrom, val(r.PrefillMs))
	}
}

func TestOnlyThisHostsModelsAreRecordedAndOnlyTheLastAreKept(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "application/json", `{"model":"zbox/embed"}`, []part{{1, "{}"}})
	h := rec.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/models", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/models/unload", nil))
	if n := len(rec.Recent()); n != 0 {
		t.Fatalf("recorded %d", n)
	}
	for range 12 {
		serve(rec, c, "application/json", `{"model":"qwen"}`, []part{{1, "{}"}})
	}
	got := rec.Recent()
	if len(got) != 10 || got[0].ID != 12 || got[9].ID != 3 {
		t.Errorf("kept %d, newest %d, oldest %d", len(got), got[0].ID, got[len(got)-1].ID)
	}
}

func TestAFailedRequestKeepsItsStatus(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	h := rec.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusBadGateway)
		io.WriteString(rw, `{"error":"x"}`)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`)))
	if r := rec.Recent()[0]; r.Status != 502 {
		t.Errorf("%+v", r)
	}
}

func TestTheSummaryCountsSuccessesAndWeighsTokens(t *testing.T) {
	ok := func(ttft, prefill, decode float64, prompt, cached int) Request {
		return Request{Model: "m", Status: 200, TTFTMs: &ttft, PrefillMs: &prefill, DecodePerSecond: &decode, PromptTokens: &prompt, CachedTokens: &cached}
	}
	requests := []Request{
		ok(100, 40, 50, 1000, 900),
		ok(300, 100, 60, 10, 0),
		ok(200, 60, 70, 10, 0),
		{Model: "m", Status: 503, TTFTMs: ptr(99999.0)},
		{Model: "other", Status: 200},
	}
	sums := Summarize(requests)
	if len(sums) != 2 || sums[0].Model != "m" || sums[1].Model != "other" {
		t.Fatalf("%+v", sums)
	}
	s := sums[0]
	if s.Requests != 4 || s.Failed != 1 {
		t.Errorf("requests %d failed %d", s.Requests, s.Failed)
	}
	if s.TTFTMs.Median != 200 || s.TTFTMs.P95 != 300 || s.TTFTMs.N != 3 {
		t.Errorf("ttft %+v: the failed request's 99999 must not count", *s.TTFTMs)
	}
	if s.WaitMs.Median != 140 || s.DecodePerSecond.Median != 60 {
		t.Errorf("wait %+v decode %+v", *s.WaitMs, *s.DecodePerSecond)
	}
	if *s.CacheShare != 900.0/1020 {
		t.Errorf("cache share %v, not the ratio of the sums", *s.CacheShare)
	}
	if s.DraftAcceptance != nil || sums[1].TTFTMs != nil {
		t.Error("a number nobody reported was made up")
	}
}

func TestBothHostsAnswerOnRequest(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "application/json", `{"model":"qwen"}`, []part{{1, "{}"}})
	asked := 0
	h := Handler(rec, "reaperboi", func(ctx context.Context) []Host {
		asked++
		return []Host{{Host: "zbox", Error: "offline"}}
	})
	get := func(path string) []Host {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest("GET", path, nil))
		var out struct{ Hosts []Host }
		json.Unmarshal(rw.Body.Bytes(), &out)
		return out.Hosts
	}
	if hosts := get("/warden/requests"); len(hosts) != 1 || hosts[0].Host != "reaperboi" || len(hosts[0].Requests) != 1 || len(hosts[0].Models) != 1 || asked != 0 {
		t.Errorf("this host: %+v, asked the others %d times", hosts, asked)
	}
	if hosts := get("/warden/requests?hosts=all"); len(hosts) != 2 || hosts[1].Host != "zbox" || hosts[1].Error != "offline" {
		t.Errorf("all: %+v", hosts)
	}
}

// Codex reaches OpenRouter through the Responses API, whose usage sits
// inside response.completed's response, with no timings beside it.
func TestACloudResponsesReplyReadsTheUsageInsideTheResponse(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "text/event-stream", `{"model":"openrouter/free"}`, []part{
		{300, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n"},
		{1000, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":56621,\"output_tokens\":17,\"input_tokens_details\":{\"cached_tokens\":50000}}}}\n\n"},
	})
	r := rec.Recent()[0]
	if val(r.PromptTokens) != 56621 || val(r.CachedTokens) != 50000 || val(r.OutputTokens) != 17 || val(r.DecodePerSecond) != 16.0 {
		t.Errorf("prompt %v cached %v output %v decode %v", val(r.PromptTokens), val(r.CachedTokens), val(r.OutputTokens), val(r.DecodePerSecond))
	}
}

// A request that waited for its model's load gets the time to READY=1; one
// for a model that was already ready gets nothing (0020).
func TestARequestThatWaitedForTheLoadHasItsReadyTime(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	rec := recorder(c)
	var readyAt time.Time
	rec.Ready = func(model string) (time.Time, string, bool) {
		if model != "reaperboi/qwen" {
			t.Errorf("Ready asked for %q", model)
		}
		return readyAt, "notify", true
	}
	load := 812
	h := rec.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if load > 0 {
			c.wait(load)
			readyAt = c.t
			load = 0
		}
		c.wait(3)
		io.WriteString(rw, `{}`)
	}))
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader(`{}`)))
	}

	got := rec.Recent()
	if r := got[1]; val(r.ReadyMs) != 812.0 || r.ReadyBy != "notify" {
		t.Fatalf("the request that waited: ready_ms %v, ready_by %q", val(r.ReadyMs), r.ReadyBy)
	}
	if r := got[0]; r.ReadyMs != nil || r.ReadyBy != "" {
		t.Fatalf("a request to a ready model: ready_ms %v, ready_by %q", val(r.ReadyMs), r.ReadyBy)
	}
}
