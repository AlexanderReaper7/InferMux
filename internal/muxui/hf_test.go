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
	"time"
)

// fakeHF is Hugging Face: resolve/main answers with the LFS object's ETag and
// size and redirects to a CDN on another host name, which serves the bytes
// and honours Range.
type fakeHF struct {
	mu      sync.Mutex
	content map[string]string // resolve path -> bytes
	// ignoreRange sends the whole file to a Range request; cut sends
	// that many bytes fewer than the size Hugging Face reported; hold
	// keeps the CDN from answering until it is closed.
	ignoreRange bool
	cut         int
	hold        chan struct{}
	moved       map[string]string // an old /org/repo/ -> its new name, as Hugging Face redirects a renamed repo
	gets        []string          // the CDN's requests, with their Range
	auth        []string          // Authorization as each server saw it, "hf:" or "cdn:"
	hf, cdn     *httptest.Server
}

func newFakeHF(t *testing.T) *fakeHF {
	f := &fakeHF{content: map[string]string{}}
	f.cdn = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.content[r.URL.Path]
		f.gets = append(f.gets, r.URL.Path+" "+r.Header.Get("Range"))
		f.auth = append(f.auth, "cdn:"+r.Header.Get("Authorization"))
		ignoreRange, cut, hold := f.ignoreRange, f.cut, f.hold
		f.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if ignoreRange || cut > 0 {
			rw.Write([]byte(body[:len(body)-cut]))
			return
		}
		http.ServeContent(rw, r, "", time.Time{}, strings.NewReader(body))
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

func (f *fakeHF) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content[path] = body
}

func (f *fakeHF) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.gets...)
}

func wait(t *testing.T, d *Downloads, source string) Download {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range d.State() {
			if s.Source == source && !s.Running {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not finish: %+v", source, d.State())
	return Download{}
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

func TestAFileFollowsMainAndIsFetchedOnlyWhenMainChanged(t *testing.T) {
	hf := newFakeHF(t)
	hf.set("/org/repo/resolve/main/m.gguf", "first version")
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("hf_secret\n"), 0o600)
	d := &Downloads{Dir: t.TempDir(), BaseURL: hf.hf.URL, TokenFile: token}

	d.Fetch("org/repo/m.gguf")
	got := wait(t, d, "org/repo/m.gguf")
	path := filepath.Join(d.Dir, "org", "repo", "m.gguf")
	if raw, _ := os.ReadFile(path); got.Error != "" || got.Result != "downloaded" || string(raw) != "first version" || got.Done != 13 || got.Total != 13 {
		t.Fatalf("first fetch: %+v, file %q", got, raw)
	}
	for _, a := range hf.auth {
		if (strings.HasPrefix(a, "hf:") && a != "hf:Bearer hf_secret") || (strings.HasPrefix(a, "cdn:") && a != "cdn:") {
			t.Fatalf("the token went to the wrong host: %v", hf.auth)
		}
	}
	if _, err := os.Stat(path + ".part"); err == nil {
		t.Fatal("the .part file was left behind")
	}

	d.Fetch("org/repo/m.gguf")
	if got := wait(t, d, "org/repo/m.gguf"); got.Result != "up to date" || len(hf.requests()) != 1 {
		t.Fatalf("an unchanged main was fetched again: %+v %v", got, hf.requests())
	}

	hf.set("/org/repo/resolve/main/m.gguf", "second, longer version")
	d.Fetch("org/repo/m.gguf")
	if got := wait(t, d, "org/repo/m.gguf"); got.Result != "downloaded" || len(hf.requests()) != 2 {
		t.Fatalf("a changed main was not fetched: %+v", got)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "second, longer version" {
		t.Fatalf("the file is %q", raw)
	}
}

func TestAnInterruptedDownloadResumesOnlyAtTheSameVersion(t *testing.T) {
	hf := newFakeHF(t)
	body := "0123456789abcdefghij"
	hf.set("/org/repo/resolve/main/m.gguf", body)
	d := &Downloads{Dir: t.TempDir(), BaseURL: hf.hf.URL}
	tag, _, err := d.etag("org/repo/m.gguf")
	if err != nil {
		t.Fatal(err)
	}
	path := d.Path("org/repo/m.gguf")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path+".part", []byte(body[:8]), 0o644)
	os.WriteFile(path+".part.etag", []byte(tag), 0o644)

	d.Fetch("org/repo/m.gguf")
	got := wait(t, d, "org/repo/m.gguf")
	if raw, _ := os.ReadFile(path); got.Error != "" || string(raw) != body {
		t.Fatalf("resumed: %+v, file %q", got, raw)
	}
	if reqs := hf.requests(); len(reqs) != 1 || !strings.HasSuffix(reqs[0], "bytes=8-") {
		t.Fatalf("the resume asked for %v, not the rest from byte 8", reqs)
	}

	// A .part from another version is not appended to.
	os.Remove(path)
	os.Remove(path + ".etag")
	os.WriteFile(path+".part", []byte("stale bytes"), 0o644)
	os.WriteFile(path+".part.etag", []byte("an older etag"), 0o644)
	d.Fetch("org/repo/m.gguf")
	wait(t, d, "org/repo/m.gguf")
	if raw, _ := os.ReadFile(path); string(raw) != body {
		t.Fatalf("after a stale .part the file is %q", raw)
	}
	if reqs := hf.requests(); strings.Contains(reqs[len(reqs)-1], "bytes=") {
		t.Fatalf("a stale .part was resumed: %v", reqs)
	}
}

func TestAMissingFileIsAnError(t *testing.T) {
	hf := newFakeHF(t)
	d := &Downloads{Dir: t.TempDir(), BaseURL: hf.hf.URL}
	d.Fetch("org/repo/none.gguf")
	if got := wait(t, d, "org/repo/none.gguf"); !strings.Contains(got.Error, "404") {
		t.Fatalf("a missing file: %+v", got)
	}
}

func TestAModelNamingHuggingFaceGetsItsPathsAndItsFiles(t *testing.T) {
	f := newFixture(t)
	hf := newFakeHF(t)
	hf.set("/org/repo/resolve/main/q.gguf", "weights")
	hf.set("/org/repo/resolve/main/mmproj.gguf", "projector")
	f.store.HF = &Downloads{Dir: filepath.Join(t.TempDir(), "hf"), BaseURL: hf.hf.URL}

	m := f.model(t, "qwen")
	m.HF = &HFSource{Model: "org/repo/q.gguf", MMProj: "org/repo/mmproj.gguf"}
	if err := f.store.SaveModel("qwen", m); err != nil {
		t.Fatal(err)
	}
	saved := f.model(t, "qwen")
	if saved.GGUF != f.store.HF.Path("org/repo/q.gguf") || saved.HF == nil || saved.HF.MMProj != "org/repo/mmproj.gguf" {
		t.Fatalf("saved: %+v", saved)
	}
	mmproj := ""
	for _, fl := range saved.Flags {
		if fl.Name == "--mmproj" {
			mmproj = *fl.Value
		}
	}
	if mmproj != f.store.HF.Path("org/repo/mmproj.gguf") {
		t.Fatalf("--mmproj is %q", mmproj)
	}
	file := f.file(t, "qwen.yaml")
	if !strings.Contains(file, "metadata:\n      hf:\n        model: org/repo/q.gguf\n        mmproj: org/repo/mmproj.gguf") ||
		!strings.Contains(file, "CUDA_VISIBLE_DEVICES=0 # keep") {
		t.Fatalf("the file:\n%s", file)
	}
	for src, want := range map[string]string{"org/repo/q.gguf": "weights", "org/repo/mmproj.gguf": "projector"} {
		wait(t, f.store.HF, src)
		if raw, _ := os.ReadFile(f.store.HF.Path(src)); string(raw) != want {
			t.Fatalf("%s is %q", src, raw)
		}
	}

	// An existing --mmproj is replaced, not doubled; an empty source removes
	// metadata.hf and keeps the paths as they are.
	saved.HF.MMProj = "org/repo/mmproj.gguf"
	if err := f.store.SaveModel("qwen", saved); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(f.file(t, "qwen.yaml"), "--mmproj"); n != 1 {
		t.Fatalf("--mmproj appears %d times", n)
	}
	wait(t, f.store.HF, "org/repo/q.gguf")
	cleared := f.model(t, "qwen")
	cleared.HF = &HFSource{}
	if err := f.store.SaveModel("qwen", cleared); err != nil {
		t.Fatal(err)
	}
	if file := f.file(t, "qwen.yaml"); strings.Contains(file, "metadata") || !strings.Contains(file, f.store.HF.Path("org/repo/q.gguf")) {
		t.Fatalf("after clearing:\n%s", file)
	}
}

func TestAModelCannotNameHuggingFaceWithoutADownloadDirOrAsText(t *testing.T) {
	f := newFixture(t)
	m := f.model(t, "qwen")
	m.HF = &HFSource{Model: "org/repo/q.gguf"}
	if err := f.store.SaveModel("qwen", m); err == nil || !strings.Contains(err.Error(), "download directory") {
		t.Fatalf("without -hf-dir: %v", err)
	}
	f.store.HF = &Downloads{Dir: t.TempDir()}
	for _, bad := range []*HFSource{{MMProj: "org/repo/p.gguf"}, {Model: "repo/q.gguf"}} {
		m.HF = bad
		if err := f.store.SaveModel("qwen", m); err == nil {
			t.Fatalf("%+v was saved", bad)
		}
	}
	m.HF = &HFSource{Model: "org/repo/q.gguf"}
	m.Raw = true
	if err := f.store.SaveModel("qwen", m); err == nil {
		t.Fatal("a raw command took a Hugging Face source")
	}
	if strings.Contains(f.file(t, "qwen.yaml"), "metadata") {
		t.Fatal("a refused save was written")
	}
}

func TestOneFileIsFetchedOnceAtATime(t *testing.T) {
	hf := newFakeHF(t)
	hf.set("/org/repo/resolve/main/m.gguf", "weights")
	hf.hold = make(chan struct{})
	d := &Downloads{Dir: t.TempDir(), BaseURL: hf.hf.URL}
	d.Fetch("org/repo/m.gguf")
	d.Fetch("org/repo/m.gguf")
	close(hf.hold)
	wait(t, d, "org/repo/m.gguf")
	time.Sleep(50 * time.Millisecond)
	if reqs := hf.requests(); len(reqs) != 1 {
		t.Fatalf("a running fetch was started again: %v", reqs)
	}
}

func TestAFileWhoseVersionIsKnownButIsGoneIsFetchedAgain(t *testing.T) {
	hf := newFakeHF(t)
	hf.set("/org/repo/resolve/main/m.gguf", "weights")
	d := &Downloads{Dir: t.TempDir(), BaseURL: hf.hf.URL}
	d.Fetch("org/repo/m.gguf")
	wait(t, d, "org/repo/m.gguf")
	os.Remove(d.Path("org/repo/m.gguf"))
	d.Fetch("org/repo/m.gguf")
	if got := wait(t, d, "org/repo/m.gguf"); got.Result != "downloaded" {
		t.Fatalf("a deleted file was taken as up to date: %+v", got)
	}
}

func TestAServerThatIgnoresTheRangeOrStopsShortIsHandled(t *testing.T) {
	hf := newFakeHF(t)
	body := "0123456789abcdefghij"
	hf.set("/org/repo/resolve/main/m.gguf", body)
	hf.ignoreRange = true
	d := &Downloads{Dir: t.TempDir(), BaseURL: hf.hf.URL}
	tag, _, _ := d.etag("org/repo/m.gguf")
	path := d.Path("org/repo/m.gguf")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path+".part", []byte(body[:8]), 0o644)
	os.WriteFile(path+".part.etag", []byte(tag), 0o644)
	d.Fetch("org/repo/m.gguf")
	got := wait(t, d, "org/repo/m.gguf")
	if raw, _ := os.ReadFile(path); got.Error != "" || string(raw) != body || got.Done != int64(len(body)) {
		t.Fatalf("a 200 to a Range request: %+v, file %q", got, raw)
	}

	hf.ignoreRange, hf.cut = false, 5
	other := "org/repo/short.gguf"
	hf.set("/org/repo/resolve/main/short.gguf", body)
	d.Fetch(other)
	if got := wait(t, d, other); !strings.Contains(got.Error, "15 of 20") {
		t.Fatalf("a short body: %+v", got)
	}
	if _, err := os.Stat(d.Path(other)); err == nil {
		t.Fatal("a short body became the model file")
	}
}

func TestARenamedRepositoryIsFollowedToItsNewName(t *testing.T) {
	hf := newFakeHF(t)
	hf.set("/org/new/resolve/main/m.gguf", "weights")
	hf.moved = map[string]string{"/org/old/": "/org/new/"}
	d := &Downloads{Dir: t.TempDir(), BaseURL: hf.hf.URL}

	d.Fetch("org/old/m.gguf")
	if got := wait(t, d, "org/old/m.gguf"); got.Error != "" || got.Result != "downloaded" {
		t.Fatalf("a renamed repo: %+v", got)
	}
	if raw, _ := os.ReadFile(d.Path("org/old/m.gguf")); string(raw) != "weights" {
		t.Errorf("file = %q", raw)
	}
}
