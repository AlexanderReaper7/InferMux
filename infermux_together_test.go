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

// groupRig is the daemon with a signed config whose GPU keeps the agent
// and the reranker loaded together (0029), on the part it derives, through
// a real llama-swap. Each model's command touches <name>.ran in dir; the
// agent's /health answers only once agentReady is set.
type groupRig struct {
	t          *testing.T
	dir        string
	agentReady *atomic.Bool
	srv        *server.Server
	front      *httptest.Server
}

// newGroupRig starts it. agentCmd is the agent's command after its touch.
func newGroupRig(t *testing.T, agentCmd string) *groupRig {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	rig := &groupRig{t: t, dir: dir, agentReady: new(atomic.Bool)}
	model := func(name, then string, ready func() bool) string {
		backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" && !ready() {
				http.Error(rw, "loading", http.StatusServiceUnavailable)
				return
			}
			io.WriteString(rw, `{}`)
		}))
		t.Cleanup(backend.Close)
		ran := filepath.Join(dir, name+".ran")
		return fmt.Sprintf("cmd: sh -c \"touch %s; %s\"\nproxy: %s\ncheckEndpoint: /health\nvram_mb: 3000\n", ran, then, backend.URL)
	}
	always := func() bool { return true }
	files := map[string][]byte{
		"infermux.yaml": []byte("hosts:\n  test:\n    url: http://127.0.0.1:1\n    devices:\n      gpu:\n        kind: gpu\n        together:\n          deep-search: [agent, reranker]\n" +
			"balance:\n  agent: [test/gpu]\n  reranker: [test/gpu]\n  chat: [test/gpu]\n"),
		"models/agent.yaml":    []byte(model("agent", agentCmd, rig.agentReady.Load)),
		"models/reranker.yaml": []byte(model("reranker", "exec sleep 3600", always)),
		"models/chat.yaml":     []byte(model("chat", "exec sleep 3600", always)),
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
	t.Cleanup(func() { *flagConfigTrustedKeys, *flagConfigSeed, *flagOffFile = "", "", "" })
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
	t.Cleanup(func() { st.Close() })
	logs := logmon.NewGroup(io.Discard, true, true, true)
	rig.srv, err = server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rig.srv.Shutdown(5 * time.Second) })
	httpServer := &http.Server{Handler: rig.srv}
	wfile := filepath.Join(dir, "warden.yaml")
	if err := os.WriteFile(wfile, []byte("host: test\npolicy:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startWarden(wfile, "", filepath.Join(dir, "derived"), httpServer, func() *server.Server { return rig.srv }, logs.ProxyLogs)
	t.Cleanup(stop)
	rig.front = httptest.NewServer(httpServer.Handler)
	t.Cleanup(rig.front.Close)
	return rig
}

func (r *groupRig) ran(name string) bool {
	_, err := os.Stat(filepath.Join(r.dir, name+".ran"))
	return err == nil
}

func (r *groupRig) ask(name string) chan int {
	status := make(chan int, 1)
	go func() {
		resp, err := http.Post(r.front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+name+`","messages":[]}`))
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

func (r *groupRig) until(what string, ok func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.t.Fatalf("never: %s; running %v", what, r.srv.RunningModelStates())
		}
	}
}

// answer is the status a request answered with, within 10 s; the GPU start
// slot's own limit is 60 s.
func (r *groupRig) answer(name string, status chan int) int {
	r.t.Helper()
	select {
	case code := <-status:
		return code
	case <-time.After(10 * time.Second):
		r.t.Fatalf("%s never answered; running %v", name, r.srv.RunningModelStates())
	}
	return 0
}

func (r *groupRig) running() []string {
	var names []string
	for name, state := range r.srv.RunningModelStates() {
		if state == "ready" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// The reranker asked for while the agent starts waits for the agent's start
// to end before its own command runs, so its fit check reads a card the
// agent is on; once both are up neither has stopped the other; and a chat
// model on the same GPU stops both.
func TestAGroupStaysLoadedTogetherAndItsStartsTakeTurns(t *testing.T) {
	r := newGroupRig(t, "exec sleep 3600")
	agent := r.ask("agent")
	r.until("the agent's command ran", func() bool { return r.ran("agent") })
	reranker := r.ask("reranker")
	time.Sleep(300 * time.Millisecond)
	if r.ran("reranker") {
		t.Fatal("the reranker started while the agent was still starting, so its fit check read a card without the agent")
	}
	r.agentReady.Store(true)
	for name, status := range map[string]chan int{"the agent": agent, "the reranker": reranker} {
		if code := r.answer(name, status); code != http.StatusOK {
			t.Fatalf("%s answered %d", name, code)
		}
	}
	if got := r.running(); !slices.Equal(got, []string{"agent", "reranker"}) {
		t.Fatalf("running %v, want the agent beside the reranker", got)
	}

	if code := r.answer("chat", r.ask("chat")); code != http.StatusOK {
		t.Fatalf("chat answered %d", code)
	}
	if got := r.running(); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("running %v, want chat alone", got)
	}
}

// A start that fails frees the slot as one that is ready does: the reranker
// waiting behind the agent starts once the agent's server has exited, not at
// the slot's limit (review of 0029, 2).
func TestAFailedStartFreesTheGPUForTheNext(t *testing.T) {
	r := newGroupRig(t, "sleep 1; exit 1")
	agent := r.ask("agent")
	r.until("the agent's command ran", func() bool { return r.ran("agent") })
	reranker := r.ask("reranker")
	if code := r.answer("the reranker", reranker); code != http.StatusOK {
		t.Fatalf("the reranker answered %d", code)
	}
	if code := r.answer("the agent", agent); code == http.StatusOK {
		t.Fatal("the agent, whose server exited, answered 200")
	}
}

// An off file written while a start waits for the GPU's start slot refuses
// that start (0025): the gate read the file before the wait (review of
// 0029, 1). The flag is set once the daemon runs, so no watcher polls the
// file and the gate alone answers, as it must between two polls.
func TestAnOffFileWrittenWhileAStartWaitsRefusesIt(t *testing.T) {
	off := filepath.Join(t.TempDir(), "off")
	r := newGroupRig(t, "exec sleep 3600")
	*flagOffFile = off
	r.ask("agent")
	r.until("the agent's command ran", func() bool { return r.ran("agent") })
	reranker := r.ask("reranker")
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(off, []byte("quiet hours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.agentReady.Store(true)
	if code := r.answer("the reranker", reranker); code == http.StatusOK {
		t.Fatal("the reranker answered 200 after the off file was written")
	}
	if r.ran("reranker") {
		t.Fatal("the reranker's command ran after the off file was written")
	}
	// The refused start left the slot free: once the file is gone the
	// reranker starts beside the agent, not at the slot's limit.
	if err := os.Remove(off); err != nil {
		t.Fatal(err)
	}
	if code := r.answer("the reranker", r.ask("reranker")); code != http.StatusOK {
		t.Fatalf("the reranker answered %d once the off file was gone", code)
	}
}
