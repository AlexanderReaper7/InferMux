package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var weights = bytes.Repeat([]byte("0123456789abcdef"), 4096)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// host serves content for every path, or 404s when content is nil, and
// keeps the Range and Authorization headers it was sent.
type host struct {
	*httptest.Server
	mu      sync.Mutex
	content []byte
	status  int
	ranges  []string
	keys    []string
}

func newHost(t *testing.T, content []byte) *host {
	h := &host{content: content}
	h.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.ranges = append(h.ranges, r.Header.Get("Range"))
		h.keys = append(h.keys, r.Header.Get("Authorization"))
		content, status := h.content, h.status
		h.mu.Unlock()
		switch {
		case status != 0:
			rw.WriteHeader(status)
		case content == nil:
			http.NotFound(rw, r)
		default:
			http.ServeContent(rw, r, "", time.Time{}, bytes.NewReader(content))
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *host) set(content []byte, status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.content, h.status = content, status
}

func (h *host) seen() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ranges...), append([]string(nil), h.keys...)
}

func run(t *testing.T, f *Fetcher) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func waitFor(t *testing.T, f *Fetcher, path, state string) Status {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if s, _ := f.statusOf(path); s.State == state {
			return s
		}
	}
	s, _ := f.statusOf(path)
	t.Fatalf("%s is %+v, never %s", path, s, state)
	return s
}

func hosts(list ...Host) func() []Host { return func() []Host { return list } }

func TestFromTheHostThatHasIt(t *testing.T) {
	without, with := newHost(t, nil), newHost(t, weights)
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "m.gguf")
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", without.URL, "k1"}, Host{"nuc", with.URL, "k2"})}
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	if f.Ready(path) {
		t.Fatal("ready before it is here")
	}
	run(t, f)
	s := waitFor(t, f, path, Present)
	if got, _ := os.ReadFile(path); !bytes.Equal(got, weights) || s.From != "nuc" {
		t.Errorf("%d bytes from %s", len(got), s.From)
	}
	if _, err := os.Stat(path + partSuffix); err == nil {
		t.Error("the part was left")
	}
	if !f.Ready(path) {
		t.Error("not ready")
	}
	if _, keys := with.seen(); keys[0] != "Bearer k2" {
		t.Errorf("sent %q", keys)
	}
	if _, keys := without.seen(); keys[0] != "Bearer k1" {
		t.Errorf("sent %q", keys)
	}
}

// A host whose copy is not the signed one is passed over, and Hugging Face
// is asked last, without a key.
func TestTheHashDecides(t *testing.T) {
	wrong := newHost(t, append([]byte("x"), weights[1:]...))
	hf := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/org/repo/resolve/main/dir/m.gguf" || r.Header.Get("Authorization") != "" {
			http.NotFound(rw, r)
			return
		}
		http.ServeContent(rw, r, "", time.Time{}, bytes.NewReader(weights))
	}))
	defer hf.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", wrong.URL, "k"}), HFBase: hf.URL}
	f.Want([]File{{Path: path, SHA256: sum(weights), HF: "org/repo/dir/m.gguf"}})
	run(t, f)
	if s := waitFor(t, f, path, Present); s.From != "huggingface.co" {
		t.Errorf("from %s", s.From)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, weights) {
		t.Error("the wrong bytes")
	}
}

func TestNoSourceHasTheSignedFile(t *testing.T) {
	wrong := newHost(t, []byte("not it"))
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", wrong.URL, ""})}
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	run(t, f)
	s := waitFor(t, f, path, Failed)
	if !strings.Contains(s.Error, "zbox: sha256 ") {
		t.Errorf("error %q", s.Error)
	}
	for _, p := range []string{path, path + partSuffix} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s is there", p)
		}
	}
	if f.Ready(path) {
		t.Error("ready")
	}
}

func TestResumesWhatItHas(t *testing.T) {
	h := newHost(t, weights)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(path+partSuffix, weights[:1000], 0o644); err != nil {
		t.Fatal(err)
	}
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", h.URL, ""})}
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	run(t, f)
	waitFor(t, f, path, Present)
	if ranges, _ := h.seen(); len(ranges) != 1 || ranges[0] != "bytes=1000-" {
		t.Errorf("ranges %q", ranges)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, weights) {
		t.Error("the wrong bytes")
	}
}

func TestFailedIsTriedAgain(t *testing.T) {
	h := newHost(t, weights)
	h.set(weights, http.StatusBadGateway)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", h.URL, ""}), Retry: 20 * time.Millisecond}
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	run(t, f)
	if s := waitFor(t, f, path, Failed); !strings.Contains(s.Error, "502") {
		t.Errorf("error %q", s.Error)
	}
	h.set(weights, 0)
	waitFor(t, f, path, Present)
}

// A file already at its path is taken as it is, unhashed, and one outside
// the directories the host may write is never fetched.
func TestPresentAndUnfetchable(t *testing.T) {
	h := newHost(t, weights)
	dir := t.TempDir()
	here := filepath.Join(dir, "here.gguf")
	os.WriteFile(here, []byte("whatever"), 0o644)
	elsewhere := filepath.Join(t.TempDir(), "m.gguf")
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", h.URL, ""})}
	f.Want([]File{{Path: here, SHA256: sum(weights)}, {Path: elsewhere, SHA256: sum(weights)}})
	run(t, f)
	time.Sleep(50 * time.Millisecond)
	got := f.Status()
	if len(got) != 2 || got[0].Path != here || got[0].State != Present || got[1].State != Unfetchable {
		t.Errorf("status %+v", got)
	}
	if ranges, _ := h.seen(); len(ranges) != 0 {
		t.Errorf("asked for %d files", len(ranges))
	}
	if !f.Ready(here) || f.Ready(elsewhere) {
		t.Error("ready")
	}
	if !f.Ready("/not/wanted") {
		t.Error("a file nothing wants holds nothing up")
	}
	if f.writable(dir) || f.writable(dir+"x/m.gguf") || !f.writable(filepath.Join(dir, "a/b.gguf")) {
		t.Error("writable")
	}
}

func TestHandlerServesOnlyListedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	os.WriteFile(path, weights, 0o644)
	listed := map[string]bool{path: true, dir: true, filepath.Join(dir, "gone.gguf"): true}
	mux := http.NewServeMux()
	mux.Handle("GET /warden/files/{path...}", Handler(func(p string) bool { return listed[p] }))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(p, rng string) (int, int) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/warden/files"+p, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Len()
	}
	for _, c := range []struct {
		path, rng    string
		status, size int
	}{
		{path, "", 200, len(weights)},
		{path, "bytes=1000-", 206, len(weights) - 1000},
		{dir, "", 404, -1},
		{filepath.Join(dir, "gone.gguf"), "", 404, -1},
		{filepath.Join(dir, "other.gguf"), "", 404, -1},
		{"/etc/passwd", "", 404, -1},
	} {
		status, size := get(c.path, c.rng)
		if status != c.status || (c.size >= 0 && size != c.size) {
			t.Errorf("%s %s: %d, %d bytes", c.path, c.rng, status, size)
		}
	}
}

func TestGateHoldsAModelUntilItsFilesAreHere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	f := &Fetcher{Dirs: []string{dir}}
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	gate := &Gate{
		Model: func(r *http.Request) (string, bool) {
			m := r.URL.Query().Get("model")
			return m, m != ""
		},
		Files:   func(model string) []string { return map[string][]string{"m": {path}}[model] },
		Fetcher: f,
	}
	h := gate.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(http.StatusTeapot) }))
	serve := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model="+model, nil))
		return rec
	}
	rec := serve("m")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "m is not here yet: "+path+" queued") {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
	for _, other := range []string{"n", ""} {
		if rec := serve(other); rec.Code != http.StatusTeapot {
			t.Errorf("%q: %d", other, rec.Code)
		}
	}
	os.WriteFile(path, weights, 0o644)
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	if rec := serve("m"); rec.Code != http.StatusTeapot {
		t.Errorf("once here: %d", rec.Code)
	}
}

// A source that stops sending is given up on, and what it sent is kept for
// the next try to resume.
func TestASourceThatStalls(t *testing.T) {
	stop := make(chan struct{})
	h := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Length", "16384")
		rw.Write(weights[:1000])
		rw.(http.Flusher).Flush()
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer h.Close()
	// Before Close, which waits for the handler.
	defer close(stop)
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", h.URL, ""}), Stall: 100 * time.Millisecond}
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	run(t, f)
	if s := waitFor(t, f, path, Failed); !strings.Contains(s.Error, "zbox: nothing came for 100ms") {
		t.Errorf("error %q", s.Error)
	}
	if fi, err := os.Stat(path + partSuffix); err != nil || fi.Size() != 1000 {
		t.Errorf("the part: %v", err)
	}
}

// A slow source that keeps sending is not stalled, however long the whole
// file takes.
func TestASlowSourceIsNotStalled(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Length", "65536")
		for i := 0; i < len(weights); i += len(weights) / 8 {
			rw.Write(weights[i : i+len(weights)/8])
			rw.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer h.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	f := &Fetcher{Dirs: []string{dir}, Hosts: hosts(Host{"zbox", h.URL, ""}), Stall: 100 * time.Millisecond}
	f.Want([]File{{Path: path, SHA256: sum(weights)}})
	run(t, f)
	waitFor(t, f, path, Present)
}
