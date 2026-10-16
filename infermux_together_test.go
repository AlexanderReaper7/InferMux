package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/configstore"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/placement"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// The deep search agent and the reranker stay loaded together on the GPU
// (0029), through llama-swap's matrix router as the daemon derives it. The
// reranker asked for while the agent starts waits for the agent's start to
// end before its own command runs, so its fit check reads a card the agent
// is on; once both are up neither has stopped the other; and a chat model
// on the same GPU stops both.
func TestAGroupStaysLoadedTogetherAndItsStartsTakeTurns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	var agentReady atomic.Bool
	model := func(name string, ready func() bool) string {
		backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" && !ready() {
				http.Error(rw, "loading", http.StatusServiceUnavailable)
				return
			}
			io.WriteString(rw, `{}`)
		}))
		t.Cleanup(backend.Close)
		ran := filepath.Join(dir, name+".ran")
		return fmt.Sprintf("cmd: sh -c \"touch %s; exec sleep 3600\"\nproxy: %s\ncheckEndpoint: /health\nvram_mb: 3000\n", ran, backend.URL)
	}
	always := func() bool { return true }
	files := map[string][]byte{
		"infermux.yaml": []byte("hosts:\n  test:\n    url: http://127.0.0.1:1\n    devices:\n      gpu:\n        kind: gpu\n        together:\n          deep-search: [agent, reranker]\n" +
			"balance:\n  agent: [test/gpu]\n  reranker: [test/gpu]\n  chat: [test/gpu]\n"),
		"models/agent.yaml":    []byte(model("agent", agentReady.Load)),
		"models/reranker.yaml": []byte(model("reranker", always)),
		"models/chat.yaml":     []byte(model("chat", always)),
	}
	ran := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name+".ran"))
		return err == nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := configstore.Sign(files, priv, "test", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(dir, "seed")
	if err := configstore.WriteBundle(seed, bundle); err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(dir, "trusted")
	if err := os.WriteFile(trusted, []byte(configstore.PublicKeyString(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	*flagConfigTrustedKeys, *flagConfigSeed = trusted, seed
	t.Cleanup(func() { *flagConfigTrustedKeys, *flagConfigSeed = "", "" })
	reloadSignal = func() {}
	t.Cleanup(func() { reloadSignal = func() { syscall.Kill(os.Getpid(), syscall.SIGHUP) } })
	was := probes
	probes.GPU = func() (warden.Resources, error) {
		zero, free := 0.0, 8000
		return warden.Resources{ForeignGPUPercent: &zero, GPUPercent: &zero, VRAMFreeMB: &free, VRAMUsedMB: 10240 - free, VRAMTotalMB: 10240, SampledAt: time.Now()}, nil
	}
	t.Cleanup(func() { probes = was; gateWarden.Store(nil) })

	// llama-swap runs the part the daemon derives, which a reload would load.
	parsed, _, err := placement.Parse(files)
	if err != nil {
		t.Fatal(err)
	}
	part, err := parsed.Derive("test")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigFromReader(strings.NewReader("healthCheckTimeout: 15\nperformance:\n  disabled: true\n" + string(part.LlamaSwap)))
	if err != nil {
		t.Fatalf("%v\n%s", err, part.LlamaSwap)
	}
	if cfg.Routing.Router.Use != "matrix" {
		t.Fatalf("router %q", cfg.Routing.Router.Use)
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
	wfile := filepath.Join(dir, "warden.yaml")
	if err := os.WriteFile(wfile, []byte("host: test\npolicy:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startWarden(wfile, "", filepath.Join(dir, "derived"), httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	defer stop()
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()

	ask := func(name string) chan int {
		status := make(chan int, 1)
		go func() {
			resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+name+`","messages":[]}`))
			if err != nil {
				status <- 0
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			status <- resp.StatusCode
		}()
		return status
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s; running %v", what, srv.RunningModelStates())
			}
		}
	}
	running := func() []string {
		var names []string
		for name, state := range srv.RunningModelStates() {
			if state == "ready" {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		return names
	}

	agent := ask("agent")
	until("the agent's command ran", func() bool { return ran("agent") })
	reranker := ask("reranker")
	time.Sleep(300 * time.Millisecond)
	if ran("reranker") {
		t.Fatal("the reranker started while the agent was still starting, so its fit check read a card without the agent")
	}
	agentReady.Store(true)
	answer := func(name string, status chan int) {
		t.Helper()
		select {
		case code := <-status:
			if code != http.StatusOK {
				t.Fatalf("%s answered %d", name, code)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never answered; running %v", name, srv.RunningModelStates())
		}
	}
	answer("the agent", agent)
	answer("the reranker", reranker)
	if got := running(); !slices.Equal(got, []string{"agent", "reranker"}) {
		t.Fatalf("running %v, want the agent beside the reranker", got)
	}

	answer("chat", ask("chat"))
	if got := running(); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("running %v, want chat alone", got)
	}
}
