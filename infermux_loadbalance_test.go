package main

import (
	"bytes"
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
	"github.com/mostlygeek/llama-swap/internal/failover"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/remote"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
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

// A model whose command exits before it is ready fails over to the next
// place, through a real llama-swap and its own answer to the failed start. A
// model that answers 500 itself had the request, so the client gets that 500
// and the next place gets nothing (0016, 3), whatever the 500 says: llama-swap's
// own words, or InferMux's headers.
func TestProxy_AModelThatDoesNotStartFailsOver(t *testing.T) {
	served := make(chan string, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			served <- "up"
			w.Write([]byte("up"))
		}
	}))
	defer backend.Close()
	// What each model answers, by the name the attempt gave it.
	answers := map[string]string{
		"broken": `{"error":{"code":500,"message":"failed to decode image","type":"server_error"}}`,
		"mimic":  `{"src":"llama-swap","error":{"message":"unspecific error: upstream command exited prematurely","type":"server_error","param":null,"code":"internal_error"}}`,
		"marked": `{"error":{"code":500,"message":"failed to decode image","type":"server_error"}}`,
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Reading the body sends the 100 Continue a request asked for.
			model, _ := swaputil.ExtractModel(r)
			switch model {
			case "marked":
				w.Header().Set("X-InferMux-Not-Started", "true")
				w.Header().Set("X-InferMux-Busy", "capacity")
			case "continues":
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			case "trailing":
				w.Header().Set("Trailer", "X-InferMux-Busy, X-Done")
				w.Write([]byte("trailing"))
				w.Header().Set("X-InferMux-Busy", "capacity")
				w.Header().Set("X-Done", "yes")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(answers[model]))
		}
	}))
	defer broken.Close()
	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("failover.yaml", "embed: [{place: test/dies}, {place: test/up}]\nown: [{place: test/broken}, {place: test/up}]\nmimics: [{place: test/mimic}, {place: test/up}]\nmarks: [{place: test/marked}, {place: test/up}]\ncontinued: [{place: test/continues}, {place: test/up}]\ntrailers: [{place: test/trailing}, {place: test/up}]\n")
	wfile := write("warden.yaml", "host: test\nfailover_file: failover.yaml\npolicy:\n  enabled: false\n")
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf("healthCheckTimeout: 15\nperformance:\n  disabled: true\nmodels:\n  dies:\n    cmd: \"false\"\n    proxy: http://127.0.0.1:9\n  up:\n    cmd: sleep 3600\n    proxy: %s\n  broken:\n    cmd: sleep 3600\n    proxy: %s\n  mimic:\n    cmd: sleep 3600\n    proxy: %s\n  marked:\n    cmd: sleep 3600\n    proxy: %s\n  continues:\n    cmd: sleep 3600\n    proxy: %s\n  trailing:\n    cmd: sleep 3600\n    proxy: %s\n", backend.URL, broken.URL, broken.URL, broken.URL, broken.URL, broken.URL)))
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
	srv, err := server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(5 * time.Second)
	httpServer := &http.Server{Handler: srv}
	defer startWarden(wfile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs)()
	front := httptest.NewServer(httpServer.Handler)
	defer front.Close()
	var header, trailer http.Header
	postWith := func(model string, extra http.Header) (int, string, string) {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/embeddings", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":["hello"]}`, model)))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range extra {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		header, trailer = resp.Header, resp.Trailer
		return resp.StatusCode, string(raw), resp.Header.Get(failover.PlaceHeader)
	}
	post := func(model string) (int, string, string) { return postWith(model, nil) }
	if code, body, place := post("embed"); code != 200 || body != "up" || place != "test/up" {
		t.Fatalf("a model that did not start: %d %q at %q, want the next place's 200", code, body, place)
	}
	<-served
	// What held the card, as the tests' GPU reads it (TestMain).
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logged.String(), "<dies> stopped before it was ready; the card "); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no line for what held the card in:\n%s", logged.String())
		}
	}
	// Its next start, with the card no freer, is refused at once rather
	// than run to fail again, and the request still fails over (0028).
	if code, body, place := post("embed"); code != 200 || body != "up" || place != "test/up" {
		t.Fatalf("after a failed start: %d %q at %q, want the next place's 200", code, body, place)
	}
	<-served
	if !strings.Contains(logged.String(), "<dies> not started: its last start on the GPU stopped before it was ready") {
		t.Errorf("the failed start was tried again:\n%s", logged.String())
	}
	for _, c := range []struct{ name, model, sent string }{
		{"a model's own 500", "own", answers["broken"]},
		{"a model's 500 in llama-swap's words", "mimics", answers["mimic"]},
		{"a model's 500 with InferMux's headers", "marks", answers["marked"]},
	} {
		if code, body, _ := post(c.model); code != 500 || body != c.sent {
			t.Errorf("%s: %d %q, want it as the model sent it", c.name, code, body)
		}
		if header.Get("X-InferMux-Not-Started") != "" || header.Get("X-InferMux-Busy") != "" {
			t.Errorf("%s: the client got %v", c.name, header)
		}
		select {
		case <-served:
			t.Errorf("%s: sent on to the next place", c.name)
		default:
		}
	}
	// A client that asks for 100 Continue gets it from InferMux, and the
	// model is not asked: llama-swap would take its 100 for the status.
	if code, body, place := postWith("continued", http.Header{"Expect": {"100-continue"}}); code != 200 || body != "up" || place != "test/up" {
		t.Errorf("a 503 after a 100 Continue: %d %q at %q, want the next place's 200", code, body, place)
	} else {
		<-served
	}
	// A trailer named as InferMux's does not reach the client.
	if code, body, _ := post("trailers"); code != 200 || body != "trailing" || trailer.Get("X-Done") != "yes" || trailer.Get("X-InferMux-Busy") != "" {
		t.Errorf("a model's trailers: %d %q, trailers %v", code, body, trailer)
	}
}

// lockedBuffer is the proxy log, read while the server writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
