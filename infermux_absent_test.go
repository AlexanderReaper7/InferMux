package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/configstore"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/warden"
)

// The daemon on a host whose GPU is absent, strix's 3080 bound to vfio-pci
// (0032): a request for a model on the GPU is refused at once with 503
// gpu_absent and its command never runs, so it cannot start on the CPU
// threads instead. Once the card is there, the same request starts it, with
// no restart.
func TestAModelOnAnAbsentGPUIsRefusedUntilTheCardAppears(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	ran := filepath.Join(dir, "ran")
	cmd := fmt.Sprintf("sh -c \"touch %s; exec sleep 3600\"", ran)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := configstore.Sign(map[string][]byte{
		"infermux.yaml":        []byte("hosts:\n  test:\n    url: http://127.0.0.1:1\n    devices:\n      gpu: {kind: gpu}\nbalance:\n  reranker: [test/gpu]\n"),
		"models/reranker.yaml": []byte("cmd: " + cmd + "\nproxy: http://127.0.0.1:1\nvram_mb: 3100\n"),
	}, priv, "test", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(dir, "seed")
	if err := configstore.WriteBundle(seed, bundle); err != nil {
		t.Fatal(err)
	}
	*flagConfigTrustedKeys, *flagConfigSeed = write("trusted", configstore.PublicKeyString(pub)+"\n"), seed
	t.Cleanup(func() { *flagConfigTrustedKeys, *flagConfigSeed = "", "" })
	reloadSignal = func() {}
	t.Cleanup(func() { reloadSignal = func() { syscall.Kill(os.Getpid(), syscall.SIGHUP) } })

	var present atomic.Bool
	was := probes
	probes.GPU = func() (warden.Resources, error) {
		if !present.Load() {
			return warden.Resources{}, warden.GPUAbsent{Detail: "nvml init: Driver Not Loaded"}
		}
		zero, f := 0.0, 9000
		return warden.Resources{ForeignGPUPercent: &zero, GPUPercent: &zero, VRAMFreeMB: &f, VRAMUsedMB: 10240 - f, VRAMTotalMB: 10240, SampledAt: time.Now()}, nil
	}
	t.Cleanup(func() { probes = was; gateWarden.Store(nil) })

	cfg, err := config.LoadConfigFromReader(strings.NewReader("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  reranker:\n    cmd: " + cmd + "\n    proxy: http://127.0.0.1:1\n"))
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
	wfile := write("warden.yaml", "host: test\npolicy:\n  poll_seconds: 1\n")
	stop := startWarden(wfile, "", filepath.Join(dir, "derived"), httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	defer stop()
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()

	rerank := `{"model":"reranker","query":"q","documents":["a"]}`
	started := time.Now()
	resp, err := http.Post(front.URL+"/v1/rerank", "application/json", strings.NewReader(rerank))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"type":"gpu_absent"`) || !strings.Contains(string(body), "reranker is placed on the GPU and this host has none now: nvml init: Driver Not Loaded") || resp.Header.Get("Retry-After") != "30" || resp.Header.Get(warden.AbsentHeader) != "gpu" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the refusal took %s", took)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the model's command ran with no GPU")
	}

	// The card handed to the host, and the same request starts it once the
	// absence is a poll old.
	present.Store(true)
	time.Sleep(1100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/rerank", strings.NewReader(rerank))
	req.Header.Set("Content-Type", "application/json")
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	for {
		if _, err := os.Stat(ran); err == nil {
			return
		}
		if ctx.Err() != nil {
			t.Fatal("with the card present, the model never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
