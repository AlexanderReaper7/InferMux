package main

import (
	"bufio"
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
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	wfile := filepath.Join(dir, "warden.yaml")
	if err := os.WriteFile(wfile, []byte("host: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  chat:\n    cmd: sleep 3600\n    proxy: %s\n", model.URL)))
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
	stop := startWarden(wfile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	defer stop()
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()
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
