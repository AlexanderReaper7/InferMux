package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// drainDaemon is a daemon with a local model, gpu, and a cloud's, m,
// both answered by one backend. Each reply streams a word, says which model
// it is in on entered, and ends on release or when the request does. The
// first request with max_tokens 7 is refused instead, with Retry-After 1, and
// entered hears "refused".
type drainDaemon struct {
	front   *httptest.Server
	stop    func()
	dir     string
	entered chan string
	release chan struct{}
}

func newDrainDaemon(t *testing.T, timeout time.Duration) *drainDaemon {
	was := drainTimeout
	drainTimeout = timeout
	t.Cleanup(func() { drainTimeout = was })
	d := &drainDaemon{dir: t.TempDir(), entered: make(chan string, 8), release: make(chan struct{})}
	var refused sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return
		}
		var body struct {
			Model     string
			MaxTokens int `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.MaxTokens == 7 {
			first := false
			refused.Do(func() { first = true })
			if first {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				d.entered <- "refused"
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first \"}}]}\n\n")
		w.(http.Flusher).Flush()
		d.entered <- body.Model
		select {
		case <-d.release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"last\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(backend.Close)
	t.Setenv("STATE_DIRECTORY", d.dir)
	write := func(name, body string) string {
		p := filepath.Join(d.dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("keys.yaml", fmt.Sprintf("keys:\n  me:\n    sha256: %s\n    class: interactive\n  episteme:\n    sha256: %s\n    class: batch\n", warden.HashKey("me-key"), warden.HashKey("episteme-key")))
	wfile := write("warden.yaml", "host: test\nkeys_file: keys.yaml\npolicy:\n  enabled: true\n  gpu_busy_percent: 100\n  min_free_vram_mb: 0\n  poll_seconds: 3600\n")
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  gpu:\n    cmd: sleep 3600\n    proxy: %s\npeers:\n  cloud:\n    proxy: %s\n    models: [m]\n", backend.URL, backend.URL)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := logmon.NewGroup(io.Discard, true, true, true)
	srv, err := server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })
	httpServer := &http.Server{Handler: srv}
	d.stop = sync.OnceFunc(startWarden(wfile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs))
	// Before backend.Close, which waits for its handlers.
	t.Cleanup(d.stop)
	d.front = httptest.NewServer(httpServer.Handler)
	t.Cleanup(d.front.Close)
	return d
}

func (d *drainDaemon) do(t *testing.T, method, path, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, d.front.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-InferMux", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (d *drainDaemon) chat(t *testing.T, model string) *http.Response {
	return d.do(t, "POST", "/v1/chat/completions", "me-key", `{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

func (d *drainDaemon) in(t *testing.T, model string) {
	t.Helper()
	select {
	case got := <-d.entered:
		if got != model {
			t.Fatalf("the backend has %s, want %s", got, model)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never reached the backend", model)
	}
}

// stopping runs the stop as SIGTERM would and closes the channel it returns
// once the stop is over.
func (d *drainDaemon) stopping() chan struct{} {
	stopped := make(chan struct{})
	go func() {
		d.stop()
		close(stopped)
	}()
	return stopped
}

// A deploy's stop (0026): the request running finishes whole, a new one gets
// 503 draining with Retry-After, and the stop ends once the request has.
func TestADeployLetsTheRequestRunningFinish(t *testing.T) {
	d := newDrainDaemon(t, 20*time.Second)
	running := d.chat(t, "gpu")
	d.in(t, "gpu")
	stopped := d.stopping()

	// A read answers at once either way, so it tells when the drain began.
	deadline := time.Now().Add(5 * time.Second)
	for d.do(t, "GET", "/running", "me-key", "").StatusCode != http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("never refused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp := d.chat(t, "gpu")
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"draining"`) || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("a new request: %d %v %s", resp.StatusCode, resp.Header, raw)
	}
	select {
	case <-stopped:
		t.Fatal("the stop did not wait for the request running")
	case <-time.After(300 * time.Millisecond):
	}

	close(d.release)
	raw, _ = io.ReadAll(running.Body)
	if !strings.Contains(string(raw), "last") || !strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("the request running was cut: %s", raw)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop went on after the request ended")
	}
}

// A job attempt is waited for like a request; past the drain it is paused
// and written, and a request still running is cut.
func TestADeployPausesTheJobStillRunningAtTheDeadline(t *testing.T) {
	d := newDrainDaemon(t, 500*time.Millisecond)
	resp := d.do(t, "POST", "/warden/jobs", "episteme-key", `{"path":"/v1/chat/completions","body":{"model":"gpu","messages":[{"role":"user","content":"forever"}],"max_tokens":50}}`)
	var job struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&job)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	d.in(t, "gpu")
	// The job's attempt runs inside the daemon: no request of the drain's
	// counts it.
	start := time.Now()
	stopped := d.stopping()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the stop never ended")
	}
	if waited := time.Since(start); waited < drainTimeout {
		t.Fatalf("stopped after %s, before the drain's %s", waited, drainTimeout)
	}
	raw, err := os.ReadFile(filepath.Join(d.dir, "jobs.json"))
	if err != nil || !strings.Contains(string(raw), job.ID) {
		t.Fatalf("jobs.json: %v %s", err, raw)
	}
}

// A job waiting to try again starts no attempt while the stop drains: the
// drain would wait for it, and cut it at the deadline anyway.
func TestADrainStartsNoJobAttempt(t *testing.T) {
	d := newDrainDaemon(t, 20*time.Second)
	running := d.chat(t, "gpu")
	d.in(t, "gpu")
	resp := d.do(t, "POST", "/warden/jobs", "episteme-key", `{"path":"/v1/chat/completions","body":{"model":"gpu","messages":[{"role":"user","content":"again"}],"max_tokens":7}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	d.in(t, "refused")
	stopped := d.stopping()

	// The job tries again 1 s after its refusal, inside the drain.
	select {
	case got := <-d.entered:
		t.Fatalf("%s started during the drain", got)
	case <-time.After(1500 * time.Millisecond):
	}
	close(d.release)
	io.ReadAll(running.Body)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop went on after the request ended")
	}
}

// With the local models off the stop is a safety stop's: no drain, and the
// cloud's request, which off lets run, is cut at once.
func TestAStopWhileOffDoesNotDrain(t *testing.T) {
	off := filepath.Join(t.TempDir(), "off")
	os.WriteFile(off, []byte("too hot"), 0o644)
	was := *flagOffFile
	*flagOffFile = off
	t.Cleanup(func() { *flagOffFile = was })
	d := newDrainDaemon(t, 20*time.Second)
	running := d.chat(t, "cloud/m")
	d.in(t, "m")

	start := time.Now()
	stopped := d.stopping()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop drained while off")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("stopped after %s", waited)
	}
	read := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(running.Body)
		read <- string(raw)
	}()
	select {
	case raw := <-read:
		if strings.Contains(raw, "[DONE]") {
			t.Fatalf("the cloud's request finished: %s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cloud's request outlived the stop")
	}
}
