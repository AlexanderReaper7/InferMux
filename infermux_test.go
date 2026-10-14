package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
)

// The stop startWarden returns ends the goroutines it started: the warden's
// loop, the remote hosts' polls and the config watchers.
func TestStopEndsTheWardensGoroutines(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer other.Close()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("keys.yaml", "keys: {}\n")
	write("failover.yaml", "{}\n")
	wfile := write("warden.yaml", fmt.Sprintf("host: test\nkeys_file: keys.yaml\nfailover_file: failover.yaml\nremotes:\n  - name: other\n    url: %s\npolicy:\n  enabled: false\n", other.URL))
	cfg, err := config.LoadConfigFromReader(strings.NewReader("performance:\n  disabled: true\nmodels:\n  gpu:\n    cmd: sleep 3600\n    proxy: http://127.0.0.1:1\n"))
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

	// The other tests in this package never stop theirs, so what counts is
	// the difference.
	ours := func() (n int, stacks []string) {
		buf := make([]byte, 1<<22)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "warden.(*Warden).run(") || strings.Contains(g, "remote.(*Router).watch(") || strings.Contains(g, ".startWarden in goroutine") {
				n++
				stacks = append(stacks, g)
			}
		}
		return
	}
	before, _ := ours()
	stop := startWarden(wfile, "", "", &http.Server{Handler: srv}, func() *server.Server { return srv }, logs.ProxyLogs)
	// The loop, the remote's poll and three watchers.
	if started, stacks := ours(); started < before+5 {
		stop()
		t.Fatalf("%d goroutines before the start, %d after it:\n%s", before, started, strings.Join(stacks, "\n\n"))
	}
	stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, stacks := ours()
		if n == before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines before the start, %d after the stop:\n%s", before, n, strings.Join(stacks, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
