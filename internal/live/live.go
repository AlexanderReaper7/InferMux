// Package live shows the requests this host serves a model for while they
// run (0031): the request's body as it arrived and the reply as the model
// writes it, chunk by chunk, to anyone following GET /warden/live. The last
// finished ones stay beside the running ones, so a short request is still
// there to read when the page catches up.
//
// llama-swap captures a request and its reply too, but stores the pair only
// once the reply has ended, and its in-flight list carries no body. This is
// InferMux's own handler, so no upstream file changes.
package live

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Entry is one request, as kept. A body that is not text, such as audio or
// a multipart upload, is not kept; its type and size are.
type Entry struct {
	ID     uint64    `json:"id"`
	Time   time.Time `json:"time"`
	Model  string    `json:"model"`
	Client string    `json:"client"`
	Path   string    `json:"path"`
	// RequestType and ResponseType are the bodies' Content-Type.
	RequestType string `json:"request_type"`
	Request     string `json:"request"`
	// RequestBytes is the body's Content-Length, -1 when the client sent
	// none and the body was not read.
	RequestBytes     int64  `json:"request_bytes"`
	RequestTruncated bool   `json:"request_truncated,omitempty"`
	RequestKept      bool   `json:"request_kept"`
	Status           int    `json:"status"`
	ResponseType     string `json:"response_type"`
	Response         string `json:"response"`
	// ResponseBytes counts every byte written, kept or not.
	ResponseBytes     int64   `json:"response_bytes"`
	ResponseTruncated bool    `json:"response_truncated,omitempty"`
	ResponseKept      bool    `json:"response_kept"`
	Done              bool    `json:"done"`
	DurationMs        float64 `json:"duration_ms,omitempty"`
	// Wiped is set by Wipe: the bodies are gone, and a running request keeps
	// no more of its reply. The sizes, times and status stay.
	Wiped bool `json:"wiped,omitempty"`

	tail []byte // the reply's last bytes, an unfinished UTF-8 character
}

// Event is one message to a follower. Type is its SSE event name.
type Event struct {
	Type string
	Data any
}

// Head is the reply's status and type, sent once it has them.
type Head struct {
	ID           uint64 `json:"id"`
	Status       int    `json:"status"`
	ResponseType string `json:"response_type"`
	ResponseKept bool   `json:"response_kept"`
}

// Chunk is the reply's next text.
type Chunk struct {
	ID   uint64 `json:"id"`
	Text string `json:"text"`
}

// End is the request's end.
type End struct {
	ID                uint64  `json:"id"`
	Status            int     `json:"status"`
	DurationMs        float64 `json:"duration_ms"`
	ResponseBytes     int64   `json:"response_bytes"`
	ResponseTruncated bool    `json:"response_truncated,omitempty"`
}

// Wiped is sent when Wipe has run: every entry up to Through has lost its
// bodies, and a page drops what it kept of them.
type Wiped struct {
	Through uint64 `json:"through"`
}

// Board keeps the running requests and the last Keep finished ones.
type Board struct {
	// Keep is how many finished requests stay listed.
	Keep int
	// Limit is how much of each body is kept, request and reply apart.
	Limit int
	// Model names the model a request is for, and whether this host serves
	// it. A request for another host's model is shown there.
	Model func(r *http.Request) (model string, here bool)
	// Client names the key the request came with.
	Client func(r *http.Request) string

	mu        sync.Mutex
	next      uint64
	running   []*Entry
	finished  []*Entry
	followers map[chan Event]struct{}
	now       func() time.Time
	// wipedThrough is the last request the last Wipe reached, told to each
	// follower after its snapshot, so a page that was reconnecting during
	// the wipe drops what it kept too.
	wipedThrough uint64
}

// Followed is how many events a follower may fall behind by before it is
// dropped. Its EventSource reconnects and starts again from a snapshot.
const Followed = 4096

func (b *Board) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *Board) limit() int {
	if b.Limit > 0 {
		return b.Limit
	}
	return 4 << 20
}

// text is whether a body of this Content-Type is kept. A missing type is
// decided by the bytes.
func text(contentType string) bool {
	t, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(t, "text/") || t == "application/json" || strings.HasSuffix(t, "+json") || t == "application/x-ndjson"
}

// Snapshot is every running request and the finished ones kept, oldest first.
func (b *Board) Snapshot() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshot()
}

func (b *Board) snapshot() []Entry {
	out := make([]Entry, 0, len(b.finished)+len(b.running))
	for _, e := range b.finished {
		out = append(out, e.copy())
	}
	for _, e := range b.running {
		out = append(out, e.copy())
	}
	slices.SortFunc(out, func(x, y Entry) int { return cmp.Compare(x.ID, y.ID) })
	return out
}

func (e *Entry) copy() Entry {
	c := *e
	c.tail = nil
	return c
}

// Follow returns the board as it is and every event after it, until stop.
// The channel is closed if the follower falls behind.
// A board that has been wiped puts its last wipe first on the channel.
func (b *Board) Follow() (snapshot []Entry, events <-chan Event, stop func()) {
	ch := make(chan Event, Followed)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.followers == nil {
		b.followers = map[chan Event]struct{}{}
	}
	b.followers[ch] = struct{}{}
	if b.wipedThrough > 0 {
		ch <- Event{"wiped", Wiped{Through: b.wipedThrough}}
	}
	return b.snapshot(), ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.followers[ch]; ok {
			delete(b.followers, ch)
			close(ch)
		}
	}
}

// send is under b.mu, so a follower sees the events in the order the board
// changed, after the snapshot it was given.
func (b *Board) send(e Event) {
	for ch := range b.followers {
		select {
		case ch <- e:
		default:
			delete(b.followers, ch)
			close(ch)
		}
	}
}

func (b *Board) begin(e *Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	e.ID = b.next
	b.running = append(b.running, e)
	b.send(Event{"start", e.copy()})
}

func (b *Board) head(e *Entry, status int, contentType string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e.Status, e.ResponseType = status, contentType
	// llama-server names its type; a backend that names none is kept as
	// text, and a byte that is not UTF-8 reaches the page as U+FFFD.
	e.ResponseKept = contentType == "" || text(contentType)
	b.send(Event{"head", Head{ID: e.ID, Status: status, ResponseType: contentType, ResponseKept: e.ResponseKept}})
}

func (b *Board) write(e *Entry, p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e.ResponseBytes += int64(len(p))
	if !e.ResponseKept || e.ResponseTruncated || e.Wiped {
		return
	}
	data := append(e.tail, p...)
	cut := complete(data)
	if len(e.Response)+cut > b.limit() {
		e.ResponseTruncated = true
		e.tail = nil
		return
	}
	e.tail = slices.Clone(data[cut:])
	if cut == 0 {
		return
	}
	s := string(data[:cut])
	e.Response += s
	b.send(Event{"chunk", Chunk{ID: e.ID, Text: s}})
}

// Wipe drops every body the board holds, the running requests' included,
// and keeps each entry's sizes, times and status. A request running now
// keeps nothing more of its reply. It returns how many entries it wiped.
func (b *Board) Wipe() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, list := range [][]*Entry{b.finished, b.running} {
		for _, e := range list {
			e.Request, e.Response, e.tail, e.Wiped = "", "", nil, true
			n++
		}
	}
	b.wipedThrough = b.next
	b.send(Event{"wiped", Wiped{Through: b.next}})
	return n
}

// complete is how much of data ends on a whole UTF-8 character, so a
// character split across two writes is sent whole with the second.
func complete(data []byte) int {
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if utf8.FullRune(data[i:]) {
				return len(data)
			}
			return i
		}
	}
	return len(data)
}

func (b *Board) end(e *Entry, start time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(e.tail) > 0 {
		s := string(e.tail)
		e.Response += s
		e.tail = nil
		b.send(Event{"chunk", Chunk{ID: e.ID, Text: s}})
	}
	if e.Status == 0 {
		e.Status = http.StatusOK
	}
	e.Done = true
	e.DurationMs = float64(b.clock().Sub(start).Microseconds()) / 1000
	b.running = slices.DeleteFunc(b.running, func(x *Entry) bool { return x == e })
	b.finished = append(b.finished, e)
	if keep := max(b.Keep, 0); len(b.finished) > keep {
		b.finished = slices.Delete(b.finished, 0, len(b.finished)-keep)
	}
	b.send(Event{"end", End{ID: e.ID, Status: e.Status, DurationMs: e.DurationMs, ResponseBytes: e.ResponseBytes, ResponseTruncated: e.ResponseTruncated}})
}

// Wrap shows every POST for a model this host serves.
func (b *Board) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/warden/") {
			next.ServeHTTP(rw, r)
			return
		}
		start := b.clock()
		e := &Entry{Time: start, Path: r.URL.Path, RequestType: r.Header.Get("Content-Type"), RequestBytes: r.ContentLength}
		// Read before Model, which puts back a copy of what it reads. Only
		// the kept part is read here; the rest follows from the client.
		if r.Body != nil && (text(e.RequestType) || e.RequestType == "") {
			read, err := io.ReadAll(io.LimitReader(r.Body, int64(b.limit())+1))
			body := r.Body
			r.Body = readCloser{io.MultiReader(bytes.NewReader(read), body), body}
			if err == nil && (e.RequestType != "" || utf8.Valid(read)) {
				e.RequestKept = true
				if len(read) > b.limit() {
					read = read[:complete(read[:b.limit()])]
					e.RequestTruncated = true
				} else if e.RequestBytes < 0 {
					e.RequestBytes = int64(len(read))
				}
				e.Request = string(read)
			}
		}
		model, here := b.Model(r)
		if !here {
			next.ServeHTTP(rw, r)
			return
		}
		e.Model = model
		if b.Client != nil {
			e.Client = b.Client(r)
		}
		b.begin(e)
		w := &writer{ResponseWriter: rw, board: b, entry: e}
		defer b.end(e, start)
		next.ServeHTTP(w, r)
	})
}

type readCloser struct {
	io.Reader
	io.Closer
}

// writer passes the reply on unchanged and shows it as it goes.
type writer struct {
	http.ResponseWriter
	board   *Board
	entry   *Entry
	started bool
}

func (w *writer) WriteHeader(code int) {
	// A 1xx is followed by the reply's own status.
	if !w.started && code >= 200 {
		w.started = true
		w.board.head(w.entry, code, w.Header().Get("Content-Type"))
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *writer) Write(p []byte) (int, error) {
	if !w.started {
		w.started = true
		w.board.head(w.entry, http.StatusOK, w.Header().Get("Content-Type"))
	}
	w.board.write(w.entry, p)
	return w.ResponseWriter.Write(p)
}

func (w *writer) Flush() {
	http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the writer beneath.
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// keepalive is how often a follower with nothing to read gets a comment, so
// a proxy between does not close the stream as idle.
const keepalive = 15 * time.Second

// Handler answers GET /warden/live as server-sent events: a snapshot of
// every entry, then start, head, chunk and end as they happen. A follower
// that falls behind is cut, and its EventSource starts again.
func Handler(b *Board) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		snapshot, events, stop := b.Follow()
		defer stop()
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.Header().Set("Cache-Control", "no-cache")
		rw.Header().Set("X-Accel-Buffering", "no")
		rw.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(rw)
		emit := func(e Event) bool {
			data, err := json.Marshal(e.Data)
			if err != nil {
				return false
			}
			_, err = io.WriteString(rw, "event: "+e.Type+"\ndata: "+string(data)+"\n\n")
			return err == nil
		}
		if !emit(Event{"snapshot", snapshot}) {
			return
		}
		rc.Flush()
		tick := time.NewTicker(keepalive)
		defer tick.Stop()
		for {
			select {
			case e, ok := <-events:
				if !ok {
					return
				}
				if !emit(e) {
					return
				}
				// Whatever else is already queued goes in the same flush.
				for more := true; more; {
					select {
					case e, ok := <-events:
						if !ok {
							rc.Flush()
							return
						}
						if !emit(e) {
							return
						}
					default:
						more = false
					}
				}
				rc.Flush()
			case <-tick.C:
				if _, err := io.WriteString(rw, ": keepalive\n\n"); err != nil {
					return
				}
				rc.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
}
