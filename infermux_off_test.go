package main

import (
	"context"
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
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestTheOffFileSaysWhy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "off")
	if got := offReason(path); got != "" {
		t.Fatalf("no file: %q", got)
	}
	os.WriteFile(path, []byte("too hot: CPU 84.0 C\n"), 0o644)
	if got := offReason(path); got != "too hot: CPU 84.0 C" {
		t.Fatalf("%q", got)
	}
	os.WriteFile(path, nil, 0o644)
	if got := offReason(path); got != path+" is there" {
		t.Fatalf("empty: %q", got)
	}
	// A directory reads as an error, as a file nobody may read would.
	if got := offReason(dir); got == "" {
		t.Fatal("an unreadable off file left the models on")
	}
}

// A daemon restarted at night is off before startWarden returns, so before
// the server takes a request (0025).
func TestTheOffFileIsReadBeforeTheFirstRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "off")
	os.WriteFile(path, []byte("quiet hours"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var first []string
	watchOff(ctx, path, func(reason string) {
		if first == nil {
			first = []string{reason}
		}
	})
	if len(first) != 1 || first[0] != "quiet hours" {
		t.Fatalf("not set when watchOff returned: %v", first)
	}
}

// Off holds at the one place every local model starts (0025; sol, review
// 10): not preload as the server is made, before the warden exists, nor a
// GET to /upstream/, which no gate of the warden's sees, starts a model. The
// refusal is the gate's own 503.
func TestOffRefusesEveryStartPreloadAndGETsIncluded(t *testing.T) {
	dir := t.TempDir()
	off := filepath.Join(dir, "off")
	ran := filepath.Join(dir, "ran")
	os.WriteFile(off, []byte("quiet hours"), 0o644)
	was := *flagOffFile
	*flagOffFile = off
	t.Cleanup(func() { *flagOffFile = was })

	preloaded := make(chan bool, 1)
	defer event.On(func(e swaputil.ModelPreloadedEvent) { preloaded <- e.Success })()
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`performance:
  disabled: true
healthCheckTimeout: 15
hooks:
  on_startup:
    preload: [gpu]
models:
  gpu:
    cmd: sh -c "touch %s; exec sleep 3600"
    proxy: http://127.0.0.1:1
`, ran)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := server.New(cfg, logmon.NewGroup(io.Discard, true, true, true), nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(5 * time.Second)

	select {
	case ok := <-preloaded:
		if ok {
			t.Fatal("preloaded while off")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preload never answered")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/upstream/gpu/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"type":"off"`) || !strings.Contains(rec.Body.String(), "quiet hours") {
		t.Fatalf("GET /upstream/gpu/: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the model's command ran while off")
	}

	// On again, the same GET starts it.
	os.Remove(off)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/upstream/gpu/", nil).WithContext(ctx))
	for {
		if _, err := os.Stat(ran); err == nil {
			return
		}
		if ctx.Err() != nil {
			t.Fatal("on again, the model never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
