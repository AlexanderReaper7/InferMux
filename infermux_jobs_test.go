package main

import (
	"bufio"
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

// A job through the whole stack (0023): the warden's pause by hand stops it
// mid-reply, the resume wakes it, and it continues from the word after the
// last one sent. A shutdown then writes an unfinished job to disk.
func TestProxy_AJobPausedByTheWardenGoesOnAfterTheResume(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	cut := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		from := 0
		msgs := body["messages"].([]any)
		if last := msgs[len(msgs)-1].(map[string]any); last["role"] == "assistant" {
			content, _ := last["content"].(string)
			from = len(strings.Fields(content))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := from; i < 10; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"w%d \"}}]}\n\n", i)
			w.(http.Flusher).Flush()
			if n == 1 && i == 2 {
				close(cut)
				<-r.Context().Done()
				return
			}
			if body["max_tokens"] == float64(1000) {
				// The job for the shutdown: it never ends on its own.
				<-r.Context().Done()
				return
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("keys.yaml", fmt.Sprintf("keys:\n  me:\n    sha256: %s\n    class: interactive\n  episteme:\n    sha256: %s\n    class: batch\n", warden.HashKey("me-key"), warden.HashKey("episteme-key")))
	write("failover.yaml", "gpu: [{place: test}, {place: test/spare}]\n")
	// The thresholds keep this machine's own card from yielding the test's
	// warden: only the hand moves it.
	wfile := write("warden.yaml", "host: test\nkeys_file: keys.yaml\nfailover_file: failover.yaml\npolicy:\n  enabled: true\n  gpu_busy_percent: 100\n  min_free_vram_mb: 0\n  poll_seconds: 3600\n")
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  gpu:\n    cmd: sleep 3600\n    proxy: %s\n  spare:\n    cmd: sleep 3600\n    proxy: %s\n", backend.URL, backend.URL)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs := logmon.NewGroup(io.Discard, true, true, true)
	srv, err := server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(5 * time.Second)
	httpServer := &http.Server{Handler: srv}
	// Deferred too, so a failure stops the job's attempt before
	// backend.Close waits for its handler.
	stop := sync.OnceFunc(startWarden(wfile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs))
	defer stop()
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()
	do := func(method, path, key, body string) *http.Response {
		req, _ := http.NewRequest(method, front.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-InferMux", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	snapshot := func(id string) (s struct{ State, Reason, Place string }) {
		json.NewDecoder(do("GET", "/warden/jobs/"+id, "episteme-key", "").Body).Decode(&s)
		return
	}
	until := func(id, state string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for s := snapshot(id); s.State != state; s = snapshot(id) {
			if time.Now().After(deadline) {
				t.Fatalf("job %s still %+v, want %s", id, s, state)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	manual := func(action string) {
		if resp := do("POST", "/warden/manual", "me-key", fmt.Sprintf(`{"action":%q}`, action)); resp.StatusCode != 200 {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: %d %s", action, resp.StatusCode, raw)
		}
	}

	resp := do("POST", "/warden/jobs", "episteme-key", `{"path":"/v1/chat/completions","body":{"model":"gpu","messages":[{"role":"user","content":"count"}],"max_tokens":50}}`)
	var job struct{ ID string }
	json.NewDecoder(resp.Body).Decode(&job)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	select {
	case <-cut:
	case <-time.After(20 * time.Second):
		t.Fatal("the model never started")
	}
	// The job is running once the proxy has passed the answer's headers on,
	// which can come after the backend has flushed its chunks.
	until(job.ID, "running")
	if s := snapshot(job.ID); s.Place != "test/gpu" {
		t.Fatalf("running as %+v", s)
	}
	manual("pause")
	until(job.ID, "paused")
	if s := snapshot(job.ID); !strings.Contains(s.Reason, "paused by hand") {
		t.Fatalf("paused for %q", s.Reason)
	}
	var verdict struct {
		Jobs struct{ Counts map[string]int }
	}
	json.NewDecoder(do("GET", "/warden/verdict", "me-key", "").Body).Decode(&verdict)
	if verdict.Jobs.Counts["paused"] != 1 {
		t.Fatalf("the verdict shows the jobs as %+v", verdict.Jobs)
	}
	manual("resume")
	until(job.ID, "done")

	events := do("GET", "/warden/jobs/"+job.ID+"/events", "episteme-key", "")
	var heard strings.Builder
	sc := bufio.NewScanner(events.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var c struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		json.Unmarshal([]byte(data), &c)
		for _, choice := range c.Choices {
			heard.WriteString(choice.Delta.Content)
		}
	}
	if heard.String() != "w0 w1 w2 w3 w4 w5 w6 w7 w8 w9 " {
		t.Fatalf("heard %q", heard.String())
	}
	mu.Lock()
	continued := bodies[len(bodies)-1]
	mu.Unlock()
	if continued["continue_final_message"] != "content" || continued["max_tokens"] != float64(47) {
		t.Fatalf("continued with %v", continued)
	}

	// A shutdown with a job unfinished, past a drain kept short (0026).
	was := drainTimeout
	drainTimeout = 100 * time.Millisecond
	defer func() { drainTimeout = was }()
	resp = do("POST", "/warden/jobs", "episteme-key", `{"path":"/v1/chat/completions","body":{"model":"gpu","messages":[{"role":"user","content":"forever"}],"max_tokens":1000}}`)
	json.NewDecoder(resp.Body).Decode(&job)
	until(job.ID, "running")
	stop()
	raw, err := os.ReadFile(filepath.Join(dir, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), job.ID) || strings.Contains(string(raw), "episteme-key") {
		t.Fatalf("jobs.json: %s", raw)
	}
}
