package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/hold"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// While infermux-hold holds the GPU, no model starts there, whatever asks
// for it: every route that dispatches by model, /upstream/ by GET and POST,
// ComfyUI's /comfyui/, and the preload of a server made during the hold.
// Each request is answered 503 gpu_held with the GPU's resume window as
// Retry-After, and the model's command never runs. Once the hold's
// connection closes, a request starts the model (0024, 5). On 2026-10-17
// dikt's warm-up GET to /upstream/qwen3-asr-1.7b/health started the model a
// second after infermux-hold had unloaded it, beside Warframe.
func TestAHoldRefusesEveryStartOnTheGPU(t *testing.T) {
	// A short directory: a socket's path is limited to 108 bytes.
	dir, err := os.MkdirTemp("", "imx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("STATE_DIRECTORY", dir)
	socket := filepath.Join(dir, "s")
	*flagControlSocket = socket
	t.Cleanup(func() { *flagControlSocket = "" })
	t.Cleanup(func() { gateWarden.Store(nil) })

	ran := map[string]string{"gpu": filepath.Join(dir, "gpu-ran"), config.ComfyUIModelID: filepath.Join(dir, "comfyui-ran")}
	models := func(preload string) string {
		return fmt.Sprintf(`healthCheckTimeout: 15
performance:
  disabled: true
%smodels:
  gpu:
    cmd: sh -c "touch %s; exec sleep 3600"
    proxy: http://127.0.0.1:1
  %s:
    cmd: sh -c "touch %s; exec sleep 3600"
    proxy: http://127.0.0.1:1
`, preload, ran["gpu"], config.ComfyUIModelID, ran[config.ComfyUIModelID])
	}
	cfg, err := config.LoadConfigFromReader(strings.NewReader(models("")))
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
	wfile := filepath.Join(dir, "warden.yaml")
	if err := os.WriteFile(wfile, []byte("host: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startWarden(wfile, "", filepath.Join(dir, "derived"), httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	defer stop()
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()

	c := hold.Dial(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := c.Hold(ctx)
	if err != nil {
		t.Fatal(err)
	}

	type request struct {
		method, path, contentType string
		body                      []byte
	}
	var requests []request
	postJSON, postForm, get := server.ModelRoutes()
	for _, path := range postJSON {
		requests = append(requests, request{http.MethodPost, path, "application/json", []byte(`{"model":"gpu","messages":[{"role":"user","content":"hi"}]}`)})
	}
	for _, path := range postForm {
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		mw.WriteField("model", "gpu")
		part, _ := mw.CreateFormFile("file", "a.wav")
		part.Write([]byte("RIFF"))
		mw.Close()
		requests = append(requests, request{http.MethodPost, path, mw.FormDataContentType(), form.Bytes()})
	}
	for _, path := range get {
		requests = append(requests, request{http.MethodGet, path + "?model=gpu", "", nil})
	}
	requests = append(requests,
		request{http.MethodGet, "/upstream/gpu/health", "", nil},
		request{http.MethodPost, "/upstream/gpu/v1/chat/completions", "application/json", []byte(`{"messages":[]}`)},
		request{http.MethodPost, "/comfyui/prompt", "application/json", []byte(`{"prompt":{}}`)},
	)
	if len(requests) < 30 {
		t.Fatalf("only %d routes to walk", len(requests))
	}
	for _, r := range requests {
		req, _ := http.NewRequest(r.method, front.URL+r.path, bytes.NewReader(r.body))
		if r.contentType != "" {
			req.Header.Set("Content-Type", r.contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", r.method, r.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"type":"gpu_held"`) || resp.Header.Get("Retry-After") != "300" {
			t.Errorf("%s %s under a hold: %d %v %s", r.method, r.path, resp.StatusCode, resp.Header, body)
		}
	}

	// A server made during the hold, as a config reload makes one, preloads
	// nothing.
	preloaded := make(chan bool, 1)
	defer event.On(func(e swaputil.ModelPreloadedEvent) { preloaded <- e.Success })()
	cfg, err = config.LoadConfigFromReader(strings.NewReader(models("hooks:\n  on_startup:\n    preload: [gpu]\n")))
	if err != nil {
		t.Fatal(err)
	}
	again, err := server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-preloaded:
		if ok {
			t.Fatal("preloaded under a hold")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preload never answered")
	}
	again.Shutdown(5 * time.Second)
	for model, marker := range ran {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s's command ran under a hold", model)
		}
	}

	// The hold ends with its connection, and the same GET starts the model.
	release()
	started, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(started, http.MethodGet, front.URL+"/upstream/gpu/health", nil)
		go func() {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
		time.Sleep(50 * time.Millisecond)
		if _, err := os.Stat(ran["gpu"]); err == nil {
			return
		}
		if started.Err() != nil {
			t.Fatal("after the hold, the model never started")
		}
	}
}
