// Package fetch brings a local model's missing weights to this host, from
// another host that has them or else from Hugging Face, and checks each
// against the SHA-256 the user signed (0022, 8). Until every file of a model
// is here, a request for it is answered 503 rather than started, so the
// failover handler on any host moves on to the model's next place.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File is a weight a local model needs.
type File struct {
	Path   string
	SHA256 string
	// HF is org/repo/file on Hugging Face (0012), or empty.
	HF string
}

// Host is another InferMux that may have the file.
type Host struct {
	Name, URL, Key string
}

// The states a file is in.
const (
	Present  = "present"
	Queued   = "queued"
	Fetching = "fetching"
	// Failed is a file no source gave with the right hash; it is tried again
	// after Fetcher.Retry.
	Failed = "failed"
	// Unfetchable is outside every directory this host may write.
	Unfetchable = "unfetchable"
)

// partSuffix marks a download in progress, beside where the file goes. The
// config never names such a path, so no host serves half a file.
const partSuffix = ".infermux-part"

// Status is one file's, for the config's state.
type Status struct {
	Path  string `json:"path"`
	State string `json:"state"`
	From  string `json:"from,omitempty"`
	Done  int64  `json:"done,omitempty"`
	Total int64  `json:"total,omitempty"`
	Error string `json:"error,omitempty"`

	retryAt time.Time
	file    File
}

// Fetcher keeps the wanted files here, one download at a time.
type Fetcher struct {
	// Dirs are where a missing file may be created.
	Dirs []string
	// Hosts are the other hosts, in the order to ask them.
	Hosts func() []Host
	// HFBase is https://huggingface.co unless a test sets it.
	HFBase string
	Client *http.Client
	Logf   func(format string, args ...any)
	// Retry is how long a failed file waits; ten minutes when zero.
	Retry time.Duration
	// Stall is how long a source may send nothing before it is given up on;
	// a minute when zero. A whole file has no time limit: 20 GB takes
	// minutes.
	Stall time.Duration

	mu     sync.Mutex
	status map[string]*Status
	wake   chan struct{}
}

func (f *Fetcher) init() {
	if f.status == nil {
		f.status = map[string]*Status{}
		f.wake = make(chan struct{}, 1)
	}
}

// Want makes files the set kept here. A file already at its path is taken
// as it is, the user's own: hashing would read every weight again at each
// config change and start, at the 1.84 GB/s measured on strix. Only what
// comes from elsewhere is checked, which is what the hash is for (0022).
// One that is queued or failed is looked for again, so a file copied in by
// hand counts, and a failed one is tried at once.
func (f *Fetcher) Want(files []File) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	next := map[string]*Status{}
	for _, file := range files {
		if s, ok := f.status[file.Path]; ok && s.file == file && (s.State == Present || s.State == Fetching) {
			next[file.Path] = s
			continue
		}
		s := &Status{Path: file.Path, file: file}
		switch {
		case regular(file.Path):
			s.State = Present
		case len(f.Dirs) == 0:
			s.State, s.Error = Unfetchable, "this host fetches no file: no directory is set to fetch into"
		case !f.writable(file.Path):
			s.State, s.Error = Unfetchable, "not under a directory this host fetches into: "+strings.Join(f.Dirs, ", ")
		default:
			s.State = Queued
		}
		next[file.Path] = s
	}
	f.status = next
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func regular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func (f *Fetcher) writable(path string) bool {
	for _, dir := range f.Dirs {
		if rel, err := filepath.Rel(dir, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

// Ready is whether path is here.
func (f *Fetcher) Ready(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.status[path]
	return !ok || s.State == Present
}

// Status is every wanted file's, by path.
func (f *Fetcher) Status() []Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Status, 0, len(f.status))
	for _, s := range f.status {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (f *Fetcher) statusOf(path string) (Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.status[path]
	if !ok {
		return Status{}, false
	}
	return *s, true
}

// update changes path's status if it is still wanted for the same file.
func (f *Fetcher) update(file File, change func(s *Status)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.status[file.Path]; ok && s.file == file {
		change(s)
	}
}

// next is the first file due, and how long until another is.
func (f *Fetcher) next(now time.Time) (File, bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	paths := make([]string, 0, len(f.status))
	for p := range f.status {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	wait := time.Duration(0)
	for _, p := range paths {
		s := f.status[p]
		switch {
		case s.State == Queued:
			return s.file, true, 0
		case s.State == Failed && !now.Before(s.retryAt):
			return s.file, true, 0
		case s.State == Failed:
			if d := s.retryAt.Sub(now); wait == 0 || d < wait {
				wait = d
			}
		}
	}
	return File{}, false, wait
}

// Run fetches until ctx ends, which cuts a download short. What it got stays
// beside the file, for the next start to resume.
func (f *Fetcher) Run(ctx context.Context) {
	f.mu.Lock()
	f.init()
	wake := f.wake
	f.mu.Unlock()
	for {
		file, ok, wait := f.next(time.Now())
		if ok {
			f.fetch(ctx, file)
			continue
		}
		var timer <-chan time.Time
		if wait > 0 {
			timer = time.After(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer:
		}
	}
}

// errNotThere is a source without the file, which is no failure.
var errNotThere = errors.New("not there")

var errStalled = errors.New("stalled")

type source struct {
	name, url, key string
}

func (f *Fetcher) sources(file File) []source {
	var out []source
	if f.Hosts != nil {
		for _, h := range f.Hosts() {
			out = append(out, source{name: h.Name, url: strings.TrimRight(h.URL, "/") + "/warden/files" + file.Path, key: h.Key})
		}
	}
	if parts := strings.SplitN(file.HF, "/", 3); len(parts) == 3 {
		base := f.HFBase
		if base == "" {
			base = "https://huggingface.co"
		}
		out = append(out, source{name: "huggingface.co", url: strings.TrimRight(base, "/") + "/" + parts[0] + "/" + parts[1] + "/resolve/main/" + parts[2]})
	}
	return out
}

func (f *Fetcher) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

func (f *Fetcher) fetch(ctx context.Context, file File) {
	if regular(file.Path) {
		f.update(file, func(s *Status) { s.State, s.Error = Present, "" })
		return
	}
	if err := os.MkdirAll(filepath.Dir(file.Path), 0o755); err != nil {
		f.fail(file, err)
		return
	}
	part := file.Path + partSuffix
	var last error
	for _, src := range f.sources(file) {
		f.update(file, func(s *Status) { s.State, s.From, s.Error = Fetching, src.name, "" })
		err := f.download(ctx, file, src, part)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNotThere) {
			continue
		}
		if err == nil {
			err = check(part, file.SHA256)
			if err != nil {
				// A whole file with the wrong hash is no start for the next
				// source.
				os.Remove(part)
			}
		}
		if err != nil {
			last = fmt.Errorf("%s: %w", src.name, err)
			f.logf("Fetch: %s from %v", file.Path, last)
			continue
		}
		if err := os.Rename(part, file.Path); err != nil {
			f.fail(file, err)
			return
		}
		f.logf("Fetch: %s from %s, checked", file.Path, src.name)
		f.update(file, func(s *Status) { s.State, s.Error = Present, "" })
		return
	}
	if last == nil {
		last = errors.New("no host has it, and it names no Hugging Face source")
	}
	f.fail(file, last)
}

func (f *Fetcher) fail(file File, err error) {
	retry := f.Retry
	if retry == 0 {
		retry = 10 * time.Minute
	}
	f.update(file, func(s *Status) {
		s.State, s.Error, s.retryAt = Failed, err.Error(), time.Now().Add(retry)
	})
}

// download brings part up to the source's whole file, resuming what is
// there.
func (f *Fetcher) download(ctx context.Context, file File, src source, part string) (err error) {
	stall := f.Stall
	if stall == 0 {
		stall = time.Minute
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer timer.Stop()
	defer func() {
		if err != nil && errors.Is(context.Cause(ctx), errStalled) {
			err = fmt.Errorf("nothing came for %s", stall)
		}
	}()
	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.url, nil)
	if err != nil {
		return err
	}
	if src.key != "" {
		req.Header.Set("Authorization", "Bearer "+src.key)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotThere
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		// All of it is here already; the hash decides.
		return nil
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("%s", resp.Status)
	}
	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	f.update(file, func(s *Status) { s.Done, s.Total = offset, max(total, 0) })
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	written, err := io.Copy(out, &progress{r: resp.Body, add: func(n int) {
		if n > 0 {
			timer.Reset(stall)
		}
		f.update(file, func(s *Status) { s.Done += int64(n) })
	}})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if total >= 0 && offset+written != total {
		return fmt.Errorf("got %d of %d bytes", offset+written, total)
	}
	return nil
}

func check(path, want string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 %s, the config signs %s", got, want)
	}
	return nil
}

type progress struct {
	r   io.Reader
	add func(int)
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.add(n)
	return n, err
}

// Handler serves another host a file the config names, so it never serves
// anything else on this disk. Mounted at /warden/files/{path...}.
func Handler(listed func(path string) bool) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := "/" + r.PathValue("path")
		if filepath.Clean(p) != p || !listed(p) {
			http.NotFound(rw, r)
			return
		}
		file, err := os.Open(p)
		if err != nil {
			http.NotFound(rw, r)
			return
		}
		defer file.Close()
		fi, err := file.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			http.NotFound(rw, r)
			return
		}
		http.ServeContent(rw, r, "", fi.ModTime(), file)
	})
}

// Gate answers 503 for a local model with a file still missing.
type Gate struct {
	// Model is the local model a request is for, by its name here.
	Model func(r *http.Request) (string, bool)
	// Files are a local model's paths.
	Files   func(model string) []string
	Fetcher *Fetcher
}

func (g *Gate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		model, ok := g.Model(r)
		if !ok {
			next.ServeHTTP(rw, r)
			return
		}
		for _, path := range g.Files(model) {
			if g.Fetcher.Ready(path) {
				continue
			}
			s, _ := g.Fetcher.statusOf(path)
			detail := fmt.Sprintf("%s is not here yet: %s %s", model, path, s.State)
			if s.Total > 0 {
				detail += fmt.Sprintf(", %d%%", s.Done*100/s.Total)
			}
			if s.From != "" {
				detail += " from " + s.From
			}
			if s.Error != "" {
				detail += ": " + s.Error
			}
			rw.Header().Set("Content-Type", "application/json")
			rw.Header().Set("Retry-After", "60")
			rw.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(rw).Encode(map[string]any{"error": map[string]string{"message": detail, "type": "model_files_missing"}})
			return
		}
		next.ServeHTTP(rw, r)
	})
}
