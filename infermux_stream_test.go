package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/remote"
	"github.com/mostlygeek/llama-swap/internal/server"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/stream/wstest"
)

// seen is the upgrades a backend was sent, as it got them.
type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
}

func (s *seen) last(t *testing.T) *http.Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		t.Fatal("the backend got no upgrade")
	}
	return s.reqs[len(s.reqs)-1]
}

// streamChain is InferMux as startWarden builds it, in front of a llama-swap
// whose model "stub" is an echo backend, with a remote host "zbox" that lists
// "stub" and "gone" and answers 502 for "gone". gone fails over from zbox to
// this host's stub. speech, when set, is the stub's /v1/audio/speech.
func streamChain(t *testing.T, speech http.Handler) (front *httptest.Server, local, zbox *seen) {
	t.Helper()
	local, zbox = &seen{}, &seen{}
	echo := wstest.Echo()
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/audio/speech" && speech != nil {
			speech.ServeHTTP(rw, r)
			return
		}
		if r.Header.Get("Upgrade") == "" {
			return // the health check, and the catalog reading /v1/models
		}
		local.add(r)
		echo.ServeHTTP(rw, r)
	}))
	t.Cleanup(backend.Close)
	other := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(rw, `{"object":"list","data":[{"id":"stub","meta":{"llamaswap":{"type":"model"}}},{"id":"gone","meta":{"llamaswap":{"type":"model"}}}]}`)
			return
		}
		zbox.add(r)
		if r.URL.Query().Get("model") == "gone" {
			http.Error(rw, "no such place", http.StatusBadGateway)
			return
		}
		echo.ServeHTTP(rw, r)
	}))
	t.Cleanup(other.Close)

	dir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", dir)
	sum := sha256.Sum256([]byte("chain-key"))
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("keys.yaml", fmt.Sprintf("keys:\n  chain:\n    sha256: %s\n    class: interactive\n", hex.EncodeToString(sum[:])))
	write("failover.yaml", "gone: [zbox, test/stub]\n")
	wardenFile := write("warden.yaml", fmt.Sprintf("host: test\nkeys_file: keys.yaml\nfailover_file: failover.yaml\nremotes:\n  - name: zbox\n    url: %s\npolicy:\n  enabled: false\n", other.URL))

	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`
healthCheckTimeout: 15
logLevel: warn
performance:
  disabled: true
models:
  stub:
    cmd: sleep 3600
    proxy: %s
    checkEndpoint: /health
`, backend.URL)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	logs := logmon.NewGroup(io.Discard, true, true, true)
	srv, err := server.New(cfg, logs, nil, st, server.BuildInfo{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Shutdown(5 * time.Second)
		st.Close()
	})
	httpServer := &http.Server{Handler: srv}
	startWarden(wardenFile, "", "", httpServer, func() *server.Server { return srv }, logs.ProxyLogs)
	front = httptest.NewServer(httpServer.Handler)
	t.Cleanup(front.Close)

	// The remote's list is read once at the start.
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer chain-key")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), `"zbox/stub"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("zbox's models never listed: %s %s", resp.Status, body)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("zbox's models never listed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return front, local, zbox
}

var chainKey = http.Header{"Sec-Websocket-Protocol": {"realtime, openai-insecure-api-key.chain-key"}}

func echoes(t *testing.T, c *wstest.Client) {
	t.Helper()
	if op, p := c.RoundTrip(wstest.Text, []byte(`{"type":"session.update"}`)); op != wstest.Text || string(p) != `{"type":"session.update"}` {
		t.Fatalf("text came back as %d %q", op, p)
	}
	audio := make([]byte, 40000)
	for i := range audio {
		audio[i] = byte(i * 13)
	}
	if op, p := c.RoundTrip(wstest.Binary, audio); op != wstest.Binary || string(p) != string(audio) {
		t.Fatalf("binary came back as %d, %d bytes", op, len(p))
	}
}

// A WebSocket that names its model in ?model= reaches that model's server at
// the path and query the client sent, with the key in the subprotocol, the
// way a browser has to send it (0018, 5).
func TestAWebSocketReachesTheModelItNames(t *testing.T) {
	front, local, _ := streamChain(t, nil)

	if c, resp := wstest.Open(t, front.URL, "/v1/realtime?model=stub&intent=transcription", nil); c != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %s, want 401", resp.Status)
	}
	c, resp := wstest.Open(t, front.URL, "/v1/realtime?model=stub&intent=transcription", chainKey)
	if c == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upgrade: %s %s", resp.Status, body)
	}
	echoes(t, c)
	got := local.last(t)
	if got.URL.Path != "/v1/realtime" || got.URL.RawQuery != "model=stub&intent=transcription" {
		t.Fatalf("the backend got %s", got.URL)
	}
	if !strings.Contains(got.Header.Get("Sec-Websocket-Protocol"), "realtime") {
		t.Fatalf("subprotocols %q", got.Header.Get("Sec-Websocket-Protocol"))
	}

	// Any path: Deepgram's and WhisperLiveKit's too.
	for _, target := range []string{"/v1/listen?model=stub&encoding=linear16", "/asr?model=stub"} {
		c, resp := wstest.Open(t, front.URL, target, chainKey)
		if c == nil {
			t.Fatalf("%s: %s", target, resp.Status)
		}
		echoes(t, c)
		if got := local.last(t).URL.RequestURI(); got != target {
			t.Fatalf("%s reached the backend as %s", target, got)
		}
	}

	if c, resp := wstest.Open(t, front.URL, "/v1/realtime?model=nobody", chainKey); c != nil || resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("a model nobody serves upgraded")
	}
}

// Another host's model goes to that host as a request for its own name, with
// the hop marked, so the host there routes it (0006, 2).
func TestAWebSocketForAnotherHostsModelGoesThere(t *testing.T) {
	front, local, zbox := streamChain(t, nil)
	c, resp := wstest.Open(t, front.URL, "/v1/realtime?model=zbox/stub", chainKey)
	if c == nil {
		t.Fatalf("upgrade: %s", resp.Status)
	}
	echoes(t, c)
	got := zbox.last(t)
	if got.URL.Path != "/v1/realtime" || got.URL.Query().Get("model") != "stub" || got.Header.Get(remote.HopHeader) == "" {
		t.Fatalf("zbox got %s, hop %q", got.URL, got.Header.Get(remote.HopHeader))
	}
	if len(local.reqs) != 0 {
		t.Fatal("this host's backend got it too")
	}
}

// Failover decides before the upgrade: a place that answers 502 is passed
// over, and the next one's session is the client's (0018, 9).
func TestAWebSocketFailsOverBeforeTheUpgrade(t *testing.T) {
	front, local, zbox := streamChain(t, nil)
	c, resp := wstest.Open(t, front.URL, "/v1/realtime?model=gone", chainKey)
	if c == nil {
		t.Fatalf("upgrade: %s", resp.Status)
	}
	echoes(t, c)
	if got := zbox.last(t); got.URL.Query().Get("model") != "gone" {
		t.Fatalf("zbox got %s", got.URL)
	}
	if got := local.last(t); got.URL.Path != "/v1/realtime" || got.URL.Query().Get("model") != "stub" {
		t.Fatalf("the fallback got %s", got.URL)
	}
}

// A session through the whole chain is followed by the warden while it is
// open and leaves one stats row when it ends (0018, 6 and 8).
func TestASessionThroughTheChainIsOneStatsRow(t *testing.T) {
	front, _, _ := streamChain(t, nil)
	get := func(path string, into any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
		req.Header.Set("Authorization", "Bearer chain-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	c, resp := wstest.Open(t, front.URL, "/v1/realtime?model=stub", chainKey)
	if c == nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upgrade: %s %s", resp.Status, body)
	}
	echoes(t, c)

	var verdict struct {
		Traffic struct {
			InFlight []struct {
				Path    string `json:"path"`
				Session *struct {
					Moving bool `json:"moving"`
				} `json:"session"`
			} `json:"in_flight"`
		} `json:"traffic"`
	}
	get("/warden/verdict", &verdict)
	if f := verdict.Traffic.InFlight; len(f) != 1 || f[0].Session == nil || !f[0].Session.Moving {
		t.Fatalf("in flight %+v", f)
	}

	if op, _ := c.RoundTrip(wstest.Close, []byte{0x03, 0xe8}); op != wstest.Close {
		t.Fatal("the close did not come back")
	}
	c.Conn.Close()
	var requests struct {
		Hosts []struct {
			Requests []struct {
				Model         string `json:"model"`
				Status        int    `json:"status"`
				RequestBytes  int64  `json:"request_bytes"`
				ResponseBytes int64  `json:"response_bytes"`
				Session       *struct {
					ClosedBy  string `json:"closed_by"`
					CloseCode int    `json:"close_code"`
				} `json:"session"`
			} `json:"requests"`
			Models []struct {
				Model    string `json:"model"`
				Sessions int    `json:"sessions"`
			} `json:"models"`
		} `json:"hosts"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		get("/warden/requests", &requests)
		if len(requests.Hosts) == 1 && len(requests.Hosts[0].Requests) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := requests.Hosts[0]
	r := h.Requests[0]
	// Each frame with its header: 25 bytes of text, 40000 of binary, and the
	// close's code, the client's masked.
	in, out := int64(6+25+8+40000+6+2), int64(2+25+4+40000+2+2)
	if len(h.Requests) != 1 || r.Model != "test/stub" || r.Status != 101 || r.Session == nil || r.RequestBytes != in || r.ResponseBytes != out {
		t.Fatalf("rows %+v, want one session of %d bytes in and %d out", h.Requests, in, out)
	}
	if r.Session.ClosedBy != "client" || r.Session.CloseCode != 1000 {
		t.Errorf("closed %+v", *r.Session)
	}
	if len(h.Models) != 1 || h.Models[0].Sessions != 1 {
		t.Errorf("summary %+v", h.Models)
	}
}

// Text to speech in chunks of unknown length reaches the client chunk by
// chunk (0018, 5): the stub sends the next chunk only once the client has the
// last one, so a reply held anywhere in the chain never completes. The stats
// time its first chunk as its first output (0018, 8).
func TestSpeechChunksArriveAsTheyAreSent(t *testing.T) {
	// Smaller than net/http's 4 KB write buffer, so a flush lost on the way holds it.
	const chunks, size = 5, 1000
	received := make(chan int)
	front, _, _ := streamChain(t, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "audio/pcm")
		for i := range chunks {
			rw.Write(bytes.Repeat([]byte{byte(i + 1)}, size))
			rw.(http.Flusher).Flush()
			select {
			case got := <-received:
				if got != i {
					t.Errorf("the client had chunk %d, want %d", got, i)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("chunk %d never reached the client", i)
				return
			}
		}
	}))
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/audio/speech", strings.NewReader(`{"model":"stub","input":"hej","response_format":"pcm"}`))
	req.Header.Set("Authorization", "Bearer chain-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength != -1 {
		t.Fatalf("%s, length %d", resp.Status, resp.ContentLength)
	}
	for i := range chunks {
		chunk := make([]byte, size)
		if _, err := io.ReadFull(resp.Body, chunk); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if !bytes.Equal(chunk, bytes.Repeat([]byte{byte(i + 1)}, size)) {
			t.Fatalf("chunk %d came changed", i)
		}
		received <- i
	}
	if rest, _ := io.ReadAll(resp.Body); len(rest) != 0 {
		t.Fatalf("%d bytes after the last chunk", len(rest))
	}

	var requests struct {
		Hosts []struct {
			Requests []struct {
				Path          string   `json:"path"`
				TTFTMs        *float64 `json:"ttft_ms"`
				DurationMs    float64  `json:"duration_ms"`
				ResponseBytes int64    `json:"response_bytes"`
			} `json:"requests"`
		} `json:"hosts"`
	}
	get, _ := http.NewRequest(http.MethodGet, front.URL+"/warden/requests", nil)
	get.Header.Set("Authorization", "Bearer chain-key")
	r2, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	json.NewDecoder(r2.Body).Decode(&requests)
	rows := requests.Hosts[0].Requests
	if len(rows) != 1 || rows[0].Path != "/v1/audio/speech" || rows[0].TTFTMs == nil || *rows[0].TTFTMs > rows[0].DurationMs || rows[0].ResponseBytes != chunks*size {
		t.Fatalf("rows %+v", rows)
	}
}
