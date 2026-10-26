package live

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func board() *Board {
	return &Board{Keep: 2, Model: func(r *http.Request) (string, bool) {
		if r.URL.Query().Get("elsewhere") != "" {
			return "", false
		}
		return "strix/qwen", true
	}, Client: func(*http.Request) string { return "phone" }}
}

// follower reads /warden/live's events from a real server.
type follower struct {
	t      *testing.T
	events chan Event
}

func follow(t *testing.T, b *Board) *follower {
	t.Helper()
	srv := httptest.NewServer(Handler(b))
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	f := &follower{t: t, events: make(chan Event, 100)}
	go func() {
		defer close(f.events)
		sc := bufio.NewScanner(res.Body)
		var name string
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				f.events <- Event{name, json.RawMessage(v)}
			}
		}
	}()
	return f
}

func (f *follower) next(want string, into any) {
	f.t.Helper()
	select {
	case e, ok := <-f.events:
		if !ok {
			f.t.Fatalf("stream ended, want %s", want)
		}
		if e.Type != want {
			f.t.Fatalf("event %s %s, want %s", e.Type, e.Data, want)
		}
		if err := json.Unmarshal(e.Data.(json.RawMessage), into); err != nil {
			f.t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		f.t.Fatalf("no %s event", want)
	}
}

func TestLive_StreamedReplyIsSeenWhileItRuns(t *testing.T) {
	b := board()
	f := follow(t, b)
	var snap []Entry
	f.next("snapshot", &snap)
	if len(snap) != 0 {
		t.Fatalf("snapshot %v, want empty", snap)
	}

	release := make(chan struct{})
	backend := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"messages":[{"role":"user","content":"hi"}]}` {
			t.Errorf("backend got %q", body)
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(rw, "data: {\"x\":\"hel\"}\n\n")
		http.NewResponseController(rw).Flush()
		<-release
		io.WriteString(rw, "data: [DONE]\n\n")
	})
	srv := httptest.NewServer(b.Wrap(backend))
	defer srv.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Error(err)
			return
		}
		io.ReadAll(res.Body)
		res.Body.Close()
	}()

	var start Entry
	f.next("start", &start)
	if start.Model != "strix/qwen" || start.Client != "phone" || start.Path != "/v1/chat/completions" || !start.RequestKept || start.Request != `{"messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("start %+v", start)
	}
	var head Head
	f.next("head", &head)
	if head.ID != start.ID || head.Status != 200 || !head.ResponseKept {
		t.Fatalf("head %+v", head)
	}
	// The first chunk arrives while the backend is still holding the reply.
	var chunk Chunk
	f.next("chunk", &chunk)
	if chunk.Text != "data: {\"x\":\"hel\"}\n\n" {
		t.Fatalf("chunk %q", chunk.Text)
	}
	if got := b.Snapshot(); len(got) != 1 || got[0].Done || got[0].Response != chunk.Text {
		t.Fatalf("running snapshot %+v", got)
	}
	close(release)
	f.next("chunk", &chunk)
	var end End
	f.next("end", &end)
	if end.ID != start.ID || end.Status != 200 || end.ResponseBytes != int64(len("data: {\"x\":\"hel\"}\n\ndata: [DONE]\n\n")) {
		t.Fatalf("end %+v", end)
	}
	<-done
	got := b.Snapshot()
	if len(got) != 1 || !got[0].Done || !strings.HasSuffix(got[0].Response, "data: [DONE]\n\n") {
		t.Fatalf("finished snapshot %+v", got)
	}
}

func TestLive_SnapshotKeepsTheLastFinished(t *testing.T) {
	b := board()
	h := b.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		io.WriteString(rw, "ok")
	}))
	for range 3 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{}`)))
	}
	got := b.Snapshot()
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("snapshot %+v, want requests 2 and 3", got)
	}
	f := follow(t, b)
	var snap []Entry
	f.next("snapshot", &snap)
	if len(snap) != 2 || snap[1].Response != "ok" || !snap[1].Done {
		t.Fatalf("followed snapshot %+v", snap)
	}
}

func TestLive_OnlyThisHostsModelPosts(t *testing.T) {
	b := board()
	var got []string
	h := b.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, string(body))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions?elsewhere=1", strings.NewReader(`{"a":1}`)))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/models/unload", strings.NewReader(`{}`)))
	if n := len(b.Snapshot()); n != 0 {
		t.Fatalf("%d entries, want none", n)
	}
	// The body read for the board still reaches the model whole.
	if got[0] != `{"a":1}` {
		t.Fatalf("backend got %q", got[0])
	}
}

func TestLive_BinaryBodiesAreSizedNotKept(t *testing.T) {
	b := board()
	audio := strings.Repeat("\xff\x00", 1000)
	var reached int
	h := b.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reached = len(body)
		rw.Header().Set("Content-Type", "audio/wav")
		io.WriteString(rw, audio)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(audio))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	h.ServeHTTP(httptest.NewRecorder(), req)
	e := b.Snapshot()[0]
	if reached != len(audio) {
		t.Fatalf("backend read %d bytes, want %d", reached, len(audio))
	}
	if e.RequestKept || e.Request != "" || e.RequestBytes != int64(len(audio)) {
		t.Fatalf("request kept %v %q, %d bytes", e.RequestKept, e.Request, e.RequestBytes)
	}
	if e.ResponseKept || e.Response != "" || e.ResponseBytes != int64(len(audio)) || e.ResponseType != "audio/wav" {
		t.Fatalf("response %+v", e)
	}
}

func TestLive_BodiesPastTheLimitAreCut(t *testing.T) {
	b := board()
	b.Limit = 10
	long := `{"prompt":"` + strings.Repeat("a", 100) + `"}`
	var reached string
	h := b.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reached = string(body)
		rw.Header().Set("Content-Type", "application/json")
		io.WriteString(rw, "12345")
		io.WriteString(rw, "67890")
		io.WriteString(rw, "abc")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(long)))
	e := b.Snapshot()[0]
	if reached != long {
		t.Fatalf("backend got %d bytes, want %d", len(reached), len(long))
	}
	if !e.RequestTruncated || e.Request != long[:10] {
		t.Fatalf("request %q truncated %v", e.Request, e.RequestTruncated)
	}
	if !e.ResponseTruncated || e.Response != "1234567890" || e.ResponseBytes != 13 {
		t.Fatalf("response %q truncated %v, %d bytes", e.Response, e.ResponseTruncated, e.ResponseBytes)
	}
}

func TestLive_ACharacterSplitAcrossWritesIsSentWhole(t *testing.T) {
	b := board()
	_, events, stop := b.Follow()
	defer stop()
	h := b.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain")
		ö := []byte("ö")
		rw.Write([]byte{'a', ö[0]})
		rw.Write([]byte{ö[1], 'b'})
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{}`)))
	var chunks []string
	for e := range events {
		if c, ok := e.Data.(Chunk); ok {
			chunks = append(chunks, c.Text)
		}
		if e.Type == "end" {
			break
		}
	}
	if strings.Join(chunks, "|") != "a|öb" {
		t.Fatalf("chunks %q, want a then öb", chunks)
	}
}

func TestLive_AFollowerThatFallsBehindIsCutNotWaitedFor(t *testing.T) {
	b := board()
	_, events, stop := b.Follow()
	defer stop()
	h := b.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain")
		for range Followed + 10 {
			io.WriteString(rw, "x")
		}
	}))
	finished := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{}`)))
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the request waited for a follower that reads nothing")
	}
	n := 0
	for range events {
		n++
	}
	if n != Followed {
		t.Fatalf("follower got %d events before its channel closed, want %d", n, Followed)
	}
}
