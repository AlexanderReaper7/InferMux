package muxui

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HFSource is where a model's files come from on Hugging Face, as
// `org/repo/file.gguf` (0012). Stored in the model file as metadata.hf; the
// paths in the command are derived from it.
type HFSource struct {
	Model  string `json:"model" yaml:"model"`
	MMProj string `json:"mmproj,omitempty" yaml:"mmproj,omitempty"`
}

// sources is every file the source names.
func (h *HFSource) sources() []string {
	if h == nil {
		return nil
	}
	out := []string{}
	for _, s := range []string{h.Model, h.MMProj} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// checkHF refuses a name that is not org/repo/path.gguf, or that would leave
// the download directory.
func checkHF(source string) error {
	parts := strings.Split(source, "/")
	if len(parts) < 3 || !strings.HasSuffix(source, ".gguf") || path.Clean(source) != source {
		return fmt.Errorf("%q: a Hugging Face file is org/repo/file.gguf", source)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "-") {
			return fmt.Errorf("%q: a Hugging Face file is org/repo/file.gguf", source)
		}
	}
	return nil
}

// Downloads fetches Hugging Face files into Dir, following each repository's
// main branch: a fetch compares main's ETag with the one the file was
// downloaded at and downloads again only when they differ.
type Downloads struct {
	Dir string
	// TokenFile holds a Hugging Face token, for gated and private repos.
	// Empty or missing: no token is sent.
	TokenFile string
	// BaseURL is https://huggingface.co unless a test sets it.
	BaseURL string
	Client  *http.Client

	mu    sync.Mutex
	state map[string]*Download
}

// Download is one file's progress, as /api/downloads reports it.
type Download struct {
	Source  string    `json:"source"`
	Path    string    `json:"path"`
	Total   int64     `json:"total"`
	Done    int64     `json:"done"`
	Running bool      `json:"running"`
	Error   string    `json:"error,omitempty"`
	Result  string    `json:"result,omitempty"` // "downloaded" or "up to date"
	Updated time.Time `json:"updated"`
}

// Path is where source is kept: Dir/org/repo/file.gguf.
func (d *Downloads) Path(source string) string {
	return filepath.Join(d.Dir, filepath.FromSlash(source))
}

// State is every file fetched since the UI started, by source.
func (d *Downloads) State() []Download {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Download, 0, len(d.state))
	for _, s := range d.state {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// Fetch starts a check, and a download if needed, of each source not already
// being fetched. It returns at once.
func (d *Downloads) Fetch(sources ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == nil {
		d.state = map[string]*Download{}
	}
	for _, src := range sources {
		if s := d.state[src]; s != nil && s.Running {
			continue
		}
		s := &Download{Source: src, Path: d.Path(src), Running: true, Updated: time.Now()}
		d.state[src] = s
		go d.run(s)
	}
}

func (d *Downloads) update(s *Download, f func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f()
	s.Updated = time.Now()
}

func (d *Downloads) run(s *Download) {
	result, err := d.fetch(s)
	d.update(s, func() {
		s.Running = false
		s.Result = result
		if err != nil {
			s.Error = err.Error()
		}
	})
}

// resolve is main's URL for a source: org/repo/resolve/main/file.
func (d *Downloads) resolve(source string) string {
	base := d.BaseURL
	if base == "" {
		base = "https://huggingface.co"
	}
	parts := strings.SplitN(source, "/", 3)
	return strings.TrimSuffix(base, "/") + "/" + parts[0] + "/" + parts[1] + "/resolve/main/" + parts[2]
}

func (d *Downloads) request(method, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if d.TokenFile != "" {
		if raw, err := os.ReadFile(d.TokenFile); err == nil && strings.TrimSpace(string(raw)) != "" {
			// Go drops it on a redirect to another host, the CDN.
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(raw)))
		}
	}
	return req, nil
}

func (d *Downloads) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

// etag is main's version of the file: Hugging Face's X-Linked-Etag, the
// LFS object's SHA-256, or the plain ETag of a file kept in git. Read from
// the last response on Hugging Face's own host, before the redirect to the
// CDN; a renamed repository first redirects to its new name on the same host.
func (d *Downloads) etag(source string) (string, int64, error) {
	req, err := d.request(http.MethodHead, d.resolve(source))
	if err != nil {
		return "", 0, err
	}
	sameHost := *d.client()
	sameHost.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Host != via[0].URL.Host {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return fmt.Errorf("%s: too many redirects", source)
		}
		return nil
	}
	resp, err := sameHost.Do(req)
	if err != nil {
		return "", 0, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", 0, fmt.Errorf("%s: %s", source, resp.Status)
	}
	tag := resp.Header.Get("X-Linked-Etag")
	if tag == "" {
		tag = resp.Header.Get("Etag")
	}
	size, _ := strconv.ParseInt(resp.Header.Get("X-Linked-Size"), 10, 64)
	if size == 0 {
		size = resp.ContentLength
	}
	if tag == "" {
		return "", 0, fmt.Errorf("%s: Hugging Face sent no ETag", source)
	}
	return strings.Trim(strings.TrimPrefix(tag, "W/"), `"`), size, nil
}

// fetch brings s.Path to main's version. A .part file left at the same ETag
// is resumed; one at another is started over. The ETag the file was taken at
// is kept beside it as file.etag.
func (d *Downloads) fetch(s *Download) (string, error) {
	tag, size, err := d.etag(s.Source)
	if err != nil {
		return "", err
	}
	d.update(s, func() { s.Total = size })
	have, _ := os.ReadFile(s.Path + ".etag")
	if _, err := os.Stat(s.Path); err == nil && string(have) == tag {
		d.update(s, func() { s.Done = size })
		return "up to date", nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return "", err
	}
	part := s.Path + ".part"
	var offset int64
	if partTag, _ := os.ReadFile(part + ".etag"); string(partTag) == tag {
		if fi, err := os.Stat(part); err == nil {
			offset = fi.Size()
		}
	}
	if err := os.WriteFile(part+".etag", []byte(tag), 0o644); err != nil {
		return "", err
	}

	req, err := d.request(http.MethodGet, d.resolve(s.Source))
	if err != nil {
		return "", err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	default:
		return "", fmt.Errorf("%s: %s", s.Source, resp.Status)
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return "", err
	}
	d.update(s, func() { s.Done = offset })
	written, err := io.Copy(out, &progress{r: resp.Body, add: func(n int) { d.update(s, func() { s.Done += int64(n) }) }})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if size > 0 && offset+written != size {
		return "", fmt.Errorf("%s: got %d of %d bytes", s.Source, offset+written, size)
	}
	if err := os.Rename(part, s.Path); err != nil {
		return "", err
	}
	os.Remove(part + ".etag")
	return "downloaded", os.WriteFile(s.Path+".etag", []byte(tag), 0o644)
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
