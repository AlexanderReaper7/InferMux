package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// The ComfyUI listener (0030) reaches ComfyUI with no key, at the paths its
// frontend asks for, and nothing but ComfyUI: a path the warden or another
// model answers on the main port is ComfyUI's path there. On the main port
// ComfyUI is a model like the others, behind a key and its allow list.
func TestComfyUIsListenerReachesComfyUIAloneAndWithoutAKey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		fmt.Fprint(w, "comfy")
	}))
	defer comfy.Close()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the chat model got %s %s", r.Method, r.URL)
	}))
	defer chat.Close()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("keys.yaml", fmt.Sprintf("keys:\n  me:\n    sha256: %s\n    class: interactive\n  chatter:\n    sha256: %s\n    class: interactive\n    allow: [test/chat]\n", warden.HashKey("me-key"), warden.HashKey("chat-key")))
	wfile := write("warden.yaml", "host: test\nkeys_file: keys.yaml\ntrusted_hosts: [comfyui.example]\npolicy:\n  enabled: false\n")
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  comfyui_auto:\n    cmd: sleep 3600\n    proxy: %s\n    checkEndpoint: /\n  chat:\n    cmd: sleep 3600\n    proxy: %s\n", comfy.URL, chat.URL)))
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
	listener := httptest.NewServer(comfyUIRoot(httpServer.Handler))
	defer listener.Close()

	do := func(base, method, path, key, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		// The browser at the Tailscale name, which serve passes on as Host.
		req.Host = "comfyui.example"
		req.Header.Set("Origin", "https://comfyui.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	for _, c := range []struct {
		method, path, body, want string
	}{
		{"GET", "/", "", "GET /"},
		{"POST", "/api/prompt", `{"prompt":{}}`, "POST /api/prompt"},
		{"GET", "/view?filename=a.png", "", "GET /view?filename=a.png"},
		// The frontend's saved workflows, a slash escaped in the path.
		{"GET", "/api/userdata/workflows%2Fa.json", "", "GET /api/userdata/workflows%2Fa.json"},
		{"GET", "/warden/verdict", "", "GET /warden/verdict"},
		{"POST", "/v1/chat/completions", `{"model":"chat","messages":[]}`, "POST /v1/chat/completions"},
	} {
		code, body := do(listener.URL, c.method, c.path, "", c.body)
		mu.Lock()
		got := slices.Contains(seen, c.want)
		mu.Unlock()
		if code != http.StatusOK || body != "comfy" || !got {
			t.Errorf("listener %s %s: %d %q, ComfyUI saw %v", c.method, c.path, code, body, seen)
		}
	}

	if code, body := do(front.URL, "GET", "/comfyui/", "", ""); code != http.StatusUnauthorized {
		t.Errorf("main port with no key: %d %s", code, body)
	}
	if code, body := do(front.URL, "GET", "/comfyui/", "chat-key", ""); code != http.StatusForbidden || !strings.Contains(body, "may not use test/comfyui_auto") {
		t.Errorf("main port with a key for chat alone: %d %s", code, body)
	}
	if code, body := do(front.URL, "POST", "/comfyui/prompt", "me-key", `{"prompt":{}}`); code != http.StatusOK || body != "comfy" {
		t.Errorf("main port with a key: %d %s", code, body)
	}
}

// The listener takes no key, so it opens only on loopback, where Tailscale's
// serve reaches it and the network does not.
func TestComfyUIsListenerRefusesAnAddressOffLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0", "comfy:0"} {
		if stop, err := serveComfyUI(addr, http.NotFoundHandler()); err == nil {
			stop()
			t.Errorf("%s: opened", addr)
		}
	}
	stop, err := serveComfyUI("127.0.0.1:0", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	stop()
}
