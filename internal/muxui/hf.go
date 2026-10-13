package muxui

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

// checkHF refuses a name that is not org/repo/path.gguf.
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

// HuggingFace is where Add model reads a file's SHA-256 (0022, 9). The
// daemons download it (internal/fetch); the UI only asks.
type HuggingFace struct {
	// TokenFile holds a Hugging Face token, for gated and private repos.
	// Empty or missing: no token is sent.
	TokenFile string
	// BaseURL is https://huggingface.co unless a test sets it.
	BaseURL string
	Client  *http.Client
}

// resolve is main's URL for a source: org/repo/resolve/main/file.
func (d *HuggingFace) resolve(source string) string {
	base := d.BaseURL
	if base == "" {
		base = "https://huggingface.co"
	}
	parts := strings.SplitN(source, "/", 3)
	return strings.TrimSuffix(base, "/") + "/" + parts[0] + "/" + parts[1] + "/resolve/main/" + parts[2]
}

func (d *HuggingFace) request(method, url string) (*http.Request, error) {
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

func (d *HuggingFace) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

// etag is main's version of the file: Hugging Face's X-Linked-Etag, the
// LFS object's SHA-256, or the plain ETag of a file kept in git. Read from
// the last response on Hugging Face's own host, before the redirect to the
// CDN; a renamed repository first redirects to its new name on the same host.
func (d *HuggingFace) etag(source string) (string, int64, error) {
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
