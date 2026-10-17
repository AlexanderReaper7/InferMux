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
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// A config reload makes the new server active before the old one, still
// serving a batch request, shuts down. The old server's model holds the card
// until it exits, so the warden does not read the card as holding no model:
// no pause and the batch request is not cancelled. Once the old server has
// shut down, its model counts no more. On 2026-10-17 each Publish paused the
// zbox for 300 s, with 314 MB free beside its loaded embedder.
func TestAReloadWithAModelLoadedDoesNotYield(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the warden's ticks")
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var answered, cut atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			w.WriteHeader(http.StatusOK)
			return
		}
		entered <- struct{}{}
		select {
		case <-release:
			answered.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[]}`))
		case <-r.Context().Done():
			cut.Store(true)
		}
	}))
	defer backend.Close()
	var releasing sync.Once
	let := func() { releasing.Do(func() { close(release) }) }

	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	var free atomic.Int64
	free.Store(6000)
	was := probes
	probes.GPU = func() (warden.Resources, error) {
		zero, f := 0.0, int(free.Load())
		return warden.Resources{ForeignGPUPercent: &zero, GPUPercent: &zero, VRAMFreeMB: &f, VRAMUsedMB: 6144 - f, VRAMTotalMB: 6144, SampledAt: time.Now()}, nil
	}
	t.Cleanup(func() { probes = was })

	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  embed:\n    cmd: sleep 3600\n    proxy: %s\n", backend.URL)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logged := &lockedBuffer{}
	logs := logmon.NewGroup(logged, true, false, false)
	newServer := func() *server.Server {
		srv, err := server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return srv
	}
	old := newServer()
	defer old.Shutdown(5 * time.Second)
	var active atomic.Pointer[server.Server]
	active.Store(old)
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { active.Load().ServeHTTP(w, r) })}
	wfile := filepath.Join(dir, "warden.yaml")
	if err := os.WriteFile(wfile, []byte("host: test\npolicy:\n  poll_seconds: 1\n  min_free_vram_mb: 3000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer startWarden(wfile, "", "", httpServer, active.Load, logs.ProxyLogs)()
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()
	// Before front closes, which waits for the request the backend holds.
	defer let()

	verdict := func() (bool, string) {
		resp, err := http.Get(front.URL + "/warden/verdict")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v struct {
			Verdict struct {
				Yielded bool   `json:"yielded"`
				Reason  string `json:"reason"`
			} `json:"verdict"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v.Verdict.Yielded, v.Verdict.Reason
	}

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/embeddings", strings.NewReader(`{"model":"embed","input":["hello"]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-InferMux-Class", "batch")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatalf("the model never served the request:\n%s", logged.String())
	}
	// The loaded model holds the card.
	free.Store(314)

	// The reload: the new server is active, the old one still serving.
	active.Store(newServer())
	defer active.Load().Shutdown(5 * time.Second)
	time.Sleep(3500 * time.Millisecond)
	if yielded, reason := verdict(); yielded {
		t.Fatalf("yielded during the reload: %s", reason)
	}
	if cut.Load() {
		t.Fatal("the batch request was cancelled during the reload")
	}
	let()
	if code := <-done; code != http.StatusOK || !answered.Load() {
		t.Fatalf("the batch request answered %d", code)
	}

	// The old server shuts down and its model with it: a card with 314 MB
	// free and no model loaded yields again.
	old.Shutdown(5 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		yielded, reason := verdict()
		if yielded {
			if !strings.Contains(reason, "only 314 MB VRAM free") {
				t.Fatalf("yielded for %q", reason)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the old server's model still counted after it shut down")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
