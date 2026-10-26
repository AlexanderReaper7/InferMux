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
	"github.com/mostlygeek/llama-swap/internal/live"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
)

// /warden/live shows a request's body and its reply's first chunk while the
// model is still writing the rest, through the daemon's own chain (0031).
func TestLiveShowsARequestWhileItRuns(t *testing.T) {
	release := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer model.Close()
	front := liveDaemon(t, model.URL)
	// Before front.Close, which waits for the request the model is holding.
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()

	follow, err := http.Get(front.URL + "/warden/live")
	if err != nil {
		t.Fatal(err)
	}
	defer follow.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(follow.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	body := `{"model":"chat","stream":true,"messages":[{"role":"user","content":"hello there"}]}`
	go func() {
		resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	var seen []string
	wait := func(want string) {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the stream ended before %q:\n%s", want, strings.Join(seen, "\n"))
				}
				seen = append(seen, l)
				if strings.Contains(l, want) {
					return
				}
			case <-deadline:
				t.Fatalf("no %q:\n%s", want, strings.Join(seen, "\n"))
			}
		}
	}
	wait(`"path":"/v1/chat/completions","request_type":"application/json","request":"{\"model\":\"chat\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hello there\"}]}"`)
	// The model is still holding the rest of its reply.
	wait(`\"content\":\"first\"`)
	free()
	wait("event: end")
}

// liveDaemon is the daemon's own chain over llama-swap, with one model
// "chat" whose server is at proxy.
func liveDaemon(t *testing.T, proxy string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	wfile := filepath.Join(dir, "warden.yaml")
	if err := os.WriteFile(wfile, []byte("host: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  chat:\n    cmd: sleep 3600\n    proxy: %s\n", proxy)))
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
	stop := startWarden(wfile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	t.Cleanup(stop)
	front := httptest.NewServer(httpServer.Handler)
	t.Cleanup(front.Close)
	return front
}

// POST /warden/live/wipe drops every body this host holds: the board's and
// llama-swap's captures. The activity log's row and the board's entry stay,
// with their sizes and status (0031).
func TestAWipeDropsTheBoardsBodiesAndLlamaSwapsCaptures(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"the secret answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":3}}`)
	}))
	defer model.Close()
	front := liveDaemon(t, model.URL)

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"the secret question"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	// llama-swap stores its capture once the reply has ended, after the client
	// has it.
	var activity []struct {
		ID         int  `json:"id"`
		HasCapture bool `json:"has_capture"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body := get("/api/metrics/activity")
		var page struct {
			Data []struct {
				ID         int  `json:"id"`
				HasCapture bool `json:"has_capture"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(body), &page) == nil && len(page.Data) == 1 && page.Data[0].HasCapture {
			activity = page.Data
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no captured activity row: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	capture := fmt.Sprintf("/api/captures/%d", activity[0].ID)
	if status, body := get(capture); status != 200 || !strings.Contains(body, `"resp_body"`) {
		t.Fatalf("capture before the wipe: %d %s", status, body)
	}
	if _, snap := liveSnapshot(t, front.URL); !strings.Contains(snap, "the secret question") || !strings.Contains(snap, "the secret answer") {
		t.Fatalf("board before the wipe: %s", snap)
	}

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/warden/live/wipe", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a wipe without X-InferMux: %v %v, want 403", resp, err)
	}
	req.Header.Set("X-InferMux", "test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var wiped struct {
		Requests     int `json:"requests"`
		CaptureBytes int `json:"capture_bytes"`
	}
	json.NewDecoder(resp.Body).Decode(&wiped)
	resp.Body.Close()
	if resp.StatusCode != 200 || wiped.Requests != 1 || wiped.CaptureBytes <= 0 {
		t.Fatalf("wipe: %d %+v", resp.StatusCode, wiped)
	}

	if status, body := get(capture); status != http.StatusNotFound {
		t.Fatalf("capture after the wipe: %d %s", status, body)
	}
	if _, body := get("/api/metrics/activity"); !strings.Contains(body, `"has_capture":false`) || !strings.Contains(body, `"resp_status_code":200`) {
		t.Fatalf("activity after the wipe: %s", body)
	}
	entries, snap := liveSnapshot(t, front.URL)
	if strings.Contains(snap, "secret") || len(entries) != 1 || !entries[0].Wiped || entries[0].RequestBytes <= 0 || entries[0].ResponseBytes <= 0 || entries[0].Status != 200 {
		t.Fatalf("board after the wipe: %s", snap)
	}
}

// liveSnapshot is the first event of /warden/live, parsed and as sent.
func liveSnapshot(t *testing.T, url string) ([]live.Entry, string) {
	t.Helper()
	resp, err := http.Get(url + "/warden/live")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var entries []live.Entry
			if err := json.Unmarshal([]byte(data), &entries); err != nil {
				t.Fatal(err)
			}
			return entries, data
		}
	}
	t.Fatal("no snapshot")
	return nil, ""
}
