// Package jobs is InferMux's job envelope (0023). A first-party client hands
// InferMux an OpenAI request as a job and follows its events. A yield on a
// device the job runs on pauses it: InferMux stops the request, keeps the
// reply so far, and sends the rest of the reply when a place in the model's
// lanes takes it again, by continuing the partial message. The client sees
// one reply. Jobs live in memory and are written to disk only on a shutdown
// signal.
//
// A job's every attempt goes through the whole stack as any request does,
// with the client's key, so the failover handler picks the place and the
// warden gates it. Only how a yield ends the attempt differs: the warden
// tells the job (warden.WithPauser), and the job stops the request itself.
package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// State is where a job is.
type State string

const (
	// Queued is a job whose attempt has not been answered yet.
	Queued State = "queued"
	// Running is a job a place is answering.
	Running State = "running"
	// Paused is a job a yield stopped, or that every place refused.
	Paused    State = "paused"
	Done      State = "done"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

func (s State) final() bool { return s == Done || s == Failed || s == Cancelled }

// Event is one entry in a job's stream. Type is "state", "chunk" (an OpenAI
// stream chunk, unchanged) or "response" (a whole answer, for an endpoint
// that does not stream).
type Event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type stateData struct {
	State   State  `json:"state"`
	Place   string `json:"place,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Attempt int    `json:"attempt"`
	// Status and Body are a failed job's last answer.
	Status int             `json:"status,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type responseData struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// kind is how a job's reply is kept across a pause.
type kind int

const (
	// chat continues the final assistant message (continue_final_message).
	chat kind = iota
	// completion appends the text so far to the prompt.
	completion
	// whole is one forward pass, seconds long: held before it starts, never
	// paused inside, sent again whole if something else cut it.
	whole
)

var endpoints = map[string]kind{
	"/v1/chat/completions": chat,
	"/v1/completions":      completion,
	"/v1/embeddings":       whole,
	"/v1/rerank":           whole,
	"/v1/reranking":        whole,
	"/rerank":              whole,
}

// Store holds the jobs.
type Store struct {
	// Serve is the stack an attempt goes through, the failover handler in
	// front. Set before Start.
	Serve http.Handler
	// Client names the key of a request inside the warden's Wrap.
	Client func(*http.Request) string
	// KeysRequired says whether a key is needed, and so whether a job read
	// back after a restart waits for its client to bring the key again.
	KeysRequired func() bool
	// File is where Stop writes the unfinished jobs and Start reads them;
	// empty for nowhere.
	File string
	Logf func(format string, args ...any)
	Now  func() time.Time
	// Probe is how often a job every place refused asks again on its own,
	// besides when a device here resumes.
	Probe time.Duration
	// Keep is how long a finished job stays, for a client that reconnects.
	Keep time.Duration
	// Limit is how many unfinished jobs the store holds before it refuses
	// more with 429.
	Limit int

	// draining: a deploy's stop waits for the attempts running and starts
	// no new one (0026). attempting is how many run. begin and Drain take
	// drainMu, so no attempt starts uncounted once Drain has returned.
	drainMu    sync.Mutex
	draining   atomic.Bool
	attempting atomic.Int64

	mu       sync.Mutex
	jobs     map[string]*Job
	stopping bool
	closing  chan struct{}
	runners  sync.WaitGroup
	base     context.Context
	stopAll  context.CancelCauseFunc
	started  bool

	// lineMu is taken last: under a job's mu, never the other way round.
	lineMu sync.Mutex
	lines  map[string][]*Job
}

// Job is one client request held by the store.
type Job struct {
	id      string
	client  string
	path    string
	model   string
	kind    kind
	request json.RawMessage
	created time.Time

	mu        sync.Mutex
	auth      http.Header // the client's key headers; never written to disk
	state     State
	place     string
	reason    string
	events    []Event
	changed   chan struct{}
	attempts  int
	failures  int
	reply     reply
	held      [][]byte // tool-call chunks, sent once the call is whole
	stop      context.CancelCauseFunc
	cancelled bool
	removed   bool
	// notBefore holds the next attempt back, through any wake: a cancel by
	// hand, or a failure's backoff.
	notBefore time.Time
	// waiting is a job every place refused. It asks again when woken or at
	// retryAt.
	waiting bool
	retryAt time.Time
	ended   time.Time
	nudge   chan struct{}
}

// reply is what the client has been sent: the text to continue from, and
// how many tokens it took.
type reply struct {
	Reasoning string `json:"reasoning,omitempty"`
	Content   string `json:"content,omitempty"`
	Text      string `json:"text,omitempty"`
	Tokens    int    `json:"tokens,omitempty"`
}

var (
	errCancelled = errors.New("the job was cancelled")
	errStopping  = errors.New("InferMux is shutting down")
)

// pauseCause is an attempt the job stopped for a yield or a cancel by hand.
type pauseCause struct {
	reason string
	hold   time.Duration
}

func (p *pauseCause) Error() string { return "paused: " + p.reason }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Store) probe() time.Duration {
	if s.Probe > 0 {
		return s.Probe
	}
	return 30 * time.Second
}

func (s *Store) keep() time.Duration {
	if s.Keep > 0 {
		return s.Keep
	}
	return 10 * time.Minute
}

func (s *Store) limit() int {
	if s.Limit > 0 {
		return s.Limit
	}
	return 10000
}

func (s *Store) keysRequired() bool { return s.KeysRequired != nil && s.KeysRequired() }

// Start reads back the jobs Stop wrote, and runs them. Call it once, after
// Serve is set.
func (s *Store) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.jobs = map[string]*Job{}
	s.closing = make(chan struct{})
	s.base, s.stopAll = context.WithCancelCause(context.Background())
	s.mu.Unlock()
	restored := s.load()
	s.mu.Lock()
	for _, j := range restored {
		s.jobs[j.id] = j
	}
	s.mu.Unlock()
	for _, j := range restored {
		s.runners.Add(1)
		go s.run(j)
	}
	go s.reap()
}

// Create makes a job of an OpenAI request for path, with the key headers of
// the client that sent it.
func (s *Store) Create(client, path string, request json.RawMessage, auth http.Header) (*Job, int, error) {
	k, ok := endpoints[path]
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("a job is for %s, not %s", endpointList(), path)
	}
	model, err := check(k, request)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	j := &Job{
		id: newID(), client: client, path: path, model: model, kind: k,
		request: request, created: s.now(), auth: auth,
		changed: make(chan struct{}), nudge: make(chan struct{}, 1),
	}
	s.mu.Lock()
	if s.stopping || !s.started {
		s.mu.Unlock()
		return nil, http.StatusServiceUnavailable, errStopping
	}
	live := 0
	for _, other := range s.jobs {
		if !other.final() {
			live++
		}
	}
	if live >= s.limit() {
		s.mu.Unlock()
		return nil, http.StatusTooManyRequests, fmt.Errorf("%d unfinished jobs already, the most this host holds", live)
	}
	s.jobs[j.id] = j
	s.runners.Add(1)
	s.mu.Unlock()
	j.mu.Lock()
	j.setState(Queued, "", "")
	j.mu.Unlock()
	go s.run(j)
	return j, http.StatusCreated, nil
}

// check reads the model, and refuses what a job cannot continue.
func check(k kind, request json.RawMessage) (string, error) {
	var body struct {
		Model    string          `json:"model"`
		N        *int            `json:"n"`
		Prompt   json.RawMessage `json:"prompt"`
		Messages []any           `json:"messages"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		return "", fmt.Errorf("the request is not a JSON object: %v", err)
	}
	if body.Model == "" {
		return "", errors.New("the request names no model")
	}
	if body.N != nil && *body.N != 1 && k != whole {
		return "", errors.New("a job continues one choice, so n must be 1")
	}
	switch k {
	case chat:
		if len(body.Messages) == 0 {
			return "", errors.New("a chat job needs messages")
		}
	case completion:
		var prompt string
		if json.Unmarshal(body.Prompt, &prompt) != nil {
			return "", errors.New("a completion job's prompt must be one string, so the text so far can be appended to it")
		}
	}
	return body.Model, nil
}

func endpointList() string {
	var paths []string
	for p := range endpoints {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return fmt.Sprint(paths)
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "job_" + hex.EncodeToString(b)
}

// Get is the job with id, when client may see it: its own, or any without
// keys.
func (s *Store) Get(id, client string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	if j == nil || j.client != client {
		return nil
	}
	return j
}

// List is client's jobs, oldest first.
func (s *Store) List(client string) []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Job
	for _, j := range s.jobs {
		if j.client == client {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].created.Before(out[b].created) })
	return out
}

// Cancel ends a job. An attempt in flight is stopped, and the runner ends
// the job when it returns.
func (s *Store) Cancel(j *Job) {
	j.mu.Lock()
	if j.state.final() {
		j.mu.Unlock()
		return
	}
	j.cancelled = true
	if j.stop != nil {
		j.stop(errCancelled)
	} else {
		j.finish(Cancelled, "cancelled by its client", 0, nil)
	}
	j.mu.Unlock()
	j.poke()
}

// Rekey gives a job the key headers of a request from its own client, which
// a job read back after a restart waits for.
func (s *Store) Rekey(j *Job, auth http.Header) {
	j.mu.Lock()
	missing := j.auth == nil
	if missing {
		j.auth = auth
	}
	j.mu.Unlock()
	if missing {
		j.poke()
	}
}

// Wake asks every job that every place refused to ask again now: a device
// here resumed, or the places changed.
func (s *Store) Wake() {
	s.mu.Lock()
	var woken []*Job
	for _, j := range s.jobs {
		woken = append(woken, j)
	}
	s.mu.Unlock()
	for _, j := range woken {
		j.wake()
	}
}

func (j *Job) poke() {
	select {
	case j.nudge <- struct{}{}:
	default:
	}
}

func (j *Job) final() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state.final()
}

// reap drops finished jobs once Keep has passed.
func (s *Store) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.closing:
			return
		case <-t.C:
		}
		now := s.now()
		s.mu.Lock()
		for id, j := range s.jobs {
			j.mu.Lock()
			old := j.state.final() && now.Sub(j.ended) >= s.keep()
			if old {
				j.removed = true
			}
			j.mu.Unlock()
			if old {
				delete(s.jobs, id)
			}
		}
		s.mu.Unlock()
	}
}

// emit appends an event and wakes whoever follows the job. Under j.mu.
func (j *Job) emit(typ string, data any) {
	raw, ok := data.(json.RawMessage)
	if !ok {
		raw, _ = json.Marshal(data)
	}
	var seq int64 = 1
	if n := len(j.events); n > 0 {
		seq = j.events[n-1].Seq + 1
	}
	j.events = append(j.events, Event{Seq: seq, Type: typ, Data: raw})
	close(j.changed)
	j.changed = make(chan struct{})
}

// setState records a change of state. Under j.mu.
func (j *Job) setState(state State, place, reason string) {
	if j.state == state && j.place == place && j.reason == reason {
		return
	}
	j.state, j.place, j.reason = state, place, reason
	j.emit("state", stateData{State: state, Place: place, Reason: reason, Attempt: j.attempts})
}

// finish ends the job. Under j.mu.
func (j *Job) finish(state State, reason string, status int, body []byte) {
	j.state, j.reason, j.place = state, reason, ""
	j.ended = time.Now()
	d := stateData{State: state, Reason: reason, Attempt: j.attempts, Status: status}
	if len(body) > 0 {
		if json.Valid(body) {
			d.Body = json.RawMessage(body)
		} else {
			d.Body, _ = json.Marshal(string(body))
		}
	}
	j.emit("state", d)
}

// Snapshot is a job as GET /warden/jobs shows it.
type Snapshot struct {
	ID       string    `json:"id"`
	Path     string    `json:"path"`
	Model    string    `json:"model"`
	State    State     `json:"state"`
	Place    string    `json:"place,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Attempts int       `json:"attempts"`
	LastSeq  int64     `json:"last_seq"`
	Created  time.Time `json:"created"`
	// WaitingForKey is a job read back after a restart, which waits for its
	// client to reconnect with the key.
	WaitingForKey bool `json:"waiting_for_key,omitempty"`
}

func (j *Job) Snapshot(keysRequired bool) Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{ID: j.id, Path: j.path, Model: j.model, State: j.state, Place: j.place, Reason: j.reason,
		Attempts: j.attempts, Created: j.created, WaitingForKey: keysRequired && j.auth == nil && !j.state.final()}
	if n := len(j.events); n > 0 {
		s.LastSeq = j.events[n-1].Seq
	}
	return s
}

// since is the events after seq, whether the job has ended, and a channel
// closed at the next event.
func (j *Job) since(seq int64) ([]Event, bool, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	i := sort.Search(len(j.events), func(i int) bool { return j.events[i].Seq > seq })
	out := append([]Event(nil), j.events[i:]...)
	return out, j.state.final() || j.removed, j.changed
}

// Summary is the jobs in /warden/verdict: how many in each state, and why the
// paused ones are.
type Summary struct {
	Counts  map[State]int `json:"counts"`
	Reasons []string      `json:"paused_reasons"`
}

func (s *Store) Summary() Summary {
	s.mu.Lock()
	jobs := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()
	sum := Summary{Counts: map[State]int{}, Reasons: []string{}}
	seen := map[string]bool{}
	for _, j := range jobs {
		j.mu.Lock()
		sum.Counts[j.state]++
		if j.state == Paused && j.reason != "" && !seen[j.reason] {
			seen[j.reason] = true
			sum.Reasons = append(sum.Reasons, j.reason)
		}
		j.mu.Unlock()
	}
	sort.Strings(sum.Reasons)
	return sum
}

// compact is JSON without the spaces a client may have sent, so an event's
// data is one SSE line.
func compact(raw []byte) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		out, _ := json.Marshal(string(raw))
		return out
	}
	return b.Bytes()
}
