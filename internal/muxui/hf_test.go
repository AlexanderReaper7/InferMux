package muxui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeHF is Hugging Face: resolve/main answers with the LFS object's ETag and
// size and redirects to a CDN on another host name.
type fakeHF struct {
	mu      sync.Mutex
	content map[string]string // resolve path -> bytes
	moved   map[string]string // an old /org/repo/ -> its new name, as Hugging Face redirects a renamed repo
	auth    []string          // Authorization as each server saw it, "hf:" or "cdn:"
	hf, cdn *httptest.Server
}

func newFakeHF(t *testing.T) *fakeHF {
	f := &fakeHF{content: map[string]string{}}
	f.cdn = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, "cdn:"+r.Header.Get("Authorization"))
		f.mu.Unlock()
		rw.Write([]byte(f.content[r.URL.Path]))
	}))
	f.hf = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.content[r.URL.Path]
		f.auth = append(f.auth, "hf:"+r.Header.Get("Authorization"))
		moved := f.moved
		f.mu.Unlock()
		for from, to := range moved {
			if rest, found := strings.CutPrefix(r.URL.Path, from); found {
				// Relative, with no ETag, as huggingface.co sends it.
				rw.Header().Set("Location", to+rest)
				rw.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
		}
		if !ok {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("X-Linked-Etag", fmt.Sprintf(`"%x"`, len(body)*7919+int(body[0])))
		rw.Header().Set("X-Linked-Size", strconv.Itoa(len(body)))
		// Another host name for the same address, so Go treats it as a
		// different host on the redirect.
		cdn := strings.Replace(f.cdn.URL, "127.0.0.1", "localhost", 1)
		http.Redirect(rw, r, cdn+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(func() { f.hf.Close(); f.cdn.Close() })
	return f
}

func TestAHuggingFaceNameIsOrgRepoFile(t *testing.T) {
	for _, ok := range []string{"unsloth/Qwen3-GGUF/Qwen3-Q4_K_M.gguf", "org/repo/sub/dir/x.gguf"} {
		if err := checkHF(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"repo/x.gguf", "org/repo/x.bin", "org/../x/y.gguf", "org/repo/../../x.gguf", "/org/repo/x.gguf", "org//repo/x.gguf", "org/repo/-x.gguf", "../repo/x.gguf"} {
		if err := checkHF(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestTheETagIsReadOnHuggingFacesHostWithTheTokenAndARenameFollowed(t *testing.T) {
	hf := newFakeHF(t)
	hf.content["/org/new/resolve/main/m.gguf"] = "weights"
	hf.moved = map[string]string{"/org/old/": "/org/new/"}
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("hf_secret\n"), 0o600)
	d := &HuggingFace{BaseURL: hf.hf.URL, TokenFile: token}

	tag, size, err := d.etag("org/old/m.gguf")
	if want := fmt.Sprintf("%x", 7*7919+int('w')); err != nil || tag != want || size != 7 {
		t.Fatalf("etag = %q %d %v, want %q 7", tag, size, err, want)
	}
	if len(hf.auth) != 2 || hf.auth[0] != "hf:Bearer hf_secret" || hf.auth[1] != "hf:Bearer hf_secret" {
		t.Errorf("the token went elsewhere, or the CDN was asked: %v", hf.auth)
	}
	if _, _, err := d.etag("org/repo/missing.gguf"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing file: %v", err)
	}
}
