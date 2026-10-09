package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/failover"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/remote"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

func TestProxy_LoadBalanceAuthenticatesAndUsesBatchCPU(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return
		}
		close(started)
		<-release
		w.Write([]byte("gpu"))
	}))
	defer backend.Close()
	cpu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(failover.MaxInflightHeader) != "" {
			t.Error("CPU backend received routing hint")
		}
		w.Write([]byte("cpu"))
	}))
	defer cpu.Close()
	zbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"embed","meta":{"llamaswap":{"type":"model"}}}]}`))
			return
		}
		if r.Header.Get(failover.MaxInflightHeader) == "1" {
			w.Header().Set(failover.BusyHeader, "capacity")
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("queued on zbox"))
	}))
	defer zbox.Close()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("keys.yaml", fmt.Sprintf("keys:\n  interactive:\n    sha256: %s\n    class: interactive\n  batch:\n    sha256: %s\n    class: batch\n  forbidden:\n    sha256: %s\n    class: batch\n    allow: []\n", warden.HashKey("interactive"), warden.HashKey("batch"), warden.HashKey("forbidden")))
	write("failover.yaml", "embed: [{place: zbox, max_inflight: 1}, {place: test/gpu, max_inflight: 1, only_if_idle: true}, {place: test/cpu/embed, batch_only: true}]\n")
	wfile := write("warden.yaml", fmt.Sprintf("host: test\nkeys_file: keys.yaml\nfailover_file: failover.yaml\nremotes:\n  - name: zbox\n    url: %s\npolicy:\n  enabled: false\n", zbox.URL))
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  gpu:\n    cmd: sleep 3600\n    proxy: %s\npeers:\n  cpu:\n    proxy: %s\n    models: [embed]\n", backend.URL, cpu.URL)))
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
	startWarden(wfile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()
	request := func(model, key string, forwarded bool) (int, string, error) {
		req, _ := http.NewRequest("POST", front.URL+"/v1/embeddings", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":["hello"]}`, model)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		if forwarded {
			req.Header.Set(remote.HopHeader, "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), err
	}
	go func() {
		defer close(done)
		if code, body, err := request("gpu", "interactive", true); err != nil || code != 200 || body != "gpu" {
			t.Errorf("held GPU request: %d %q %v", code, body, err)
		}
	}()
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		close(release)
		<-done
		t.Fatal("GPU backend never started")
	}
	defer func() { close(release); <-done }()
	for _, tc := range []struct {
		key  string
		code int
		body string
	}{
		{"batch", 200, "cpu"},
		{"interactive", 200, "queued on zbox"},
		{"unknown", 401, ""},
		{"forbidden", 403, ""},
	} {
		code, body, err := request("embed", tc.key, false)
		if err != nil || code != tc.code || (tc.body != "" && body != tc.body) {
			t.Fatalf("%s got %d %q %v, want %d %q", tc.key, code, body, err, tc.code, tc.body)
		}
	}
}

// While the warden is yielded, overflow puts no model on this card, even for
// an interactive key: the request queues at the zbox (0021).
func TestProxy_OverflowSkipsYieldedGPU(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte("gpu"))
		}
	}))
	defer backend.Close()
	zbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"embed","meta":{"llamaswap":{"type":"model"}}}]}`))
			return
		}
		if r.Header.Get(failover.MaxInflightHeader) == "1" {
			w.Header().Set(failover.BusyHeader, "capacity")
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("queued on zbox"))
	}))
	defer zbox.Close()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("keys.yaml", fmt.Sprintf("keys:\n  interactive:\n    sha256: %s\n    class: interactive\n", warden.HashKey("interactive")))
	write("failover.yaml", "embed: [{place: zbox, max_inflight: 1}, {place: test/gpu, max_inflight: 1, only_if_idle: true}]\n")
	wfile := write("warden.yaml", fmt.Sprintf("host: test\nkeys_file: keys.yaml\nfailover_file: failover.yaml\nremotes:\n  - name: zbox\n    url: %s\npolicy:\n  enabled: true\n", zbox.URL))
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  gpu:\n    cmd: sleep 3600\n    proxy: %s\n", backend.URL)))
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
	startWarden(wfile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", front.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer interactive")
		req.Header.Set("X-InferMux", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	embed := `{"model":"embed","input":["hello"]}`
	for _, tc := range []struct{ action, want string }{
		{"pause", "queued on zbox"},
		{"resume", "gpu"},
	} {
		if code, body := post("/warden/manual", fmt.Sprintf(`{"action":%q}`, tc.action)); code != 200 {
			t.Fatalf("%s: %d %s", tc.action, code, body)
		}
		if code, body := post("/v1/embeddings", embed); code != 200 || body != tc.want {
			t.Fatalf("after %s got %d %q, want %q", tc.action, code, body, tc.want)
		}
	}
}
