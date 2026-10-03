package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type nopLog struct{}

func (nopLog) Infof(string, ...any) {}
func (nopLog) Warnf(string, ...any) {}

// fakeHost is another InferMux: its /v1/models lists one local model, one
// peer model and one it learned from a third host, and every other request
// is echoed back as what arrived.
type fakeHost struct {
	*httptest.Server
	down   atomic.Bool // drops every connection, as a host that is off
	mu     sync.Mutex
	models []string
	// context is every model's derived context window (internal/catalog).
	context int
	seen    []*http.Request
	bodies  []string
}

func newFakeHost(t *testing.T, models ...string) *fakeHost {
	f := &fakeHost{models: models, context: 8192}
	f.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			conn, _, _ := rw.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r)
		f.bodies = append(f.bodies, string(body))
		models, context := f.models, f.context
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/models":
			if r.Header.Get("Authorization") != "Bearer host-key" {
				http.Error(rw, "no", http.StatusUnauthorized)
				return
			}
			data := []any{}
			for _, m := range models {
				data = append(data, map[string]any{"id": m, "meta": map[string]any{
					"llamaswap": map[string]any{"type": "model"},
					"infermux":  map[string]any{"context_window": context, "reasoning_efforts": []string{"none", "medium"}},
				}})
			}
			data = append(data,
				map[string]any{"id": "openrouter/cloud", "meta": map[string]any{"llamaswap": map[string]any{"type": "peer"}}},
				map[string]any{"id": "third/x", "meta": map[string]any{"llamaswap": map[string]any{"type": "remote"}}},
			)
			json.NewEncoder(rw).Encode(map[string]any{"data": data})
		case r.URL.Path == "/v1/stream":
			flusher := rw.(http.Flusher)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(rw, "data: %d\n\n", i)
				flusher.Flush()
				time.Sleep(150 * time.Millisecond)
			}
		case strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
			conn, buf, _ := rw.(http.Hijacker).Hijack()
			defer conn.Close()
			buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			buf.Flush()
			line, _ := buf.ReadString('\n')
			buf.WriteString("echo " + line)
			buf.Flush()
		default:
			json.NewEncoder(rw).Encode(map[string]string{"path": r.URL.Path, "body": string(body)})
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeHost) last() (*http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1], f.bodies[len(f.bodies)-1]
}

// local is this host's llama-swap: it has "qwen" and a peer "openrouter/cloud".
type local struct{ served int }

func (l *local) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	l.served++
	if r.URL.Path == "/v1/models" {
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"object":"list","data":[{"id":"qwen","meta":{"llamaswap":{"type":"model"}}}]}`))
		return
	}
	rw.WriteHeader(http.StatusTeapot)
}

func isLocal(m string) bool { return m == "qwen" || m == "openrouter/cloud" || m == "cloud" }

func newRouter(t *testing.T, stateDir string, hosts ...Host) (*Router, *local) {
	l := &local{}
	rt, err := New(hosts, l, isLocal, stateDir, nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		rt.Poll(h.Name)
	}
	return rt, l
}

func send(rt http.Handler, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	return rec
}

func TestOnlyTheOtherHostsOwnModelsAreListed(t *testing.T) {
	zbox := newFakeHost(t, "embed", "rerank")
	rt, _ := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "host-key"})
	rec := send(rt, "GET", "/v1/models", "", nil)
	var list struct {
		Data []struct {
			ID   string
			Meta struct{ LlamaSwap struct{ Type string } }
		}
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID+":"+m.Meta.LlamaSwap.Type)
	}
	if strings.Join(ids, " ") != "qwen:model zbox/embed:remote zbox/rerank:remote" {
		t.Fatalf("listed %v", ids)
	}
}

func TestARemoteModelGoesThereWithTheClientsKeyAndItsOwnName(t *testing.T) {
	zbox := newFakeHost(t, "embed")
	rt, l := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "host-key"})
	header := map[string]string{"Authorization": "Bearer client-key", "Origin": "https://reaperboi.ts.net:5001"}
	rec := send(rt, "POST", "/v1/embeddings", `{"model":"zbox/embed","input":"x"}`, header)
	req, body := zbox.last()
	if rec.Code != 200 || l.served != 0 {
		t.Fatalf("%d, served locally %d", rec.Code, l.served)
	}
	if !strings.Contains(body, `"model":"embed"`) || !strings.Contains(body, `"input":"x"`) {
		t.Fatalf("body there: %s", body)
	}
	if req.Header.Get("Authorization") != "Bearer client-key" {
		t.Fatalf("the client's key was not passed on: %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get(HopHeader) == "" || req.Header.Get("Origin") != "" {
		t.Fatalf("hop %q origin %q", req.Header.Get(HopHeader), req.Header.Get("Origin"))
	}

	send(rt, "POST", "/v1/embeddings", `{"model":"embed"}`, nil)
	if _, body := zbox.last(); !strings.Contains(body, `"model":"embed"`) || l.served != 0 {
		t.Fatal("an unqualified name only the zbox has did not go there")
	}
	send(rt, "GET", "/upstream/zbox/embed/health", "", nil)
	if req, _ := zbox.last(); req.URL.Path != "/upstream/embed/health" {
		t.Fatalf("upstream path there: %s", req.URL.Path)
	}
}

func TestLocalNamesWinAndAnAmbiguousOneIsNotGuessed(t *testing.T) {
	zbox := newFakeHost(t, "qwen", "both")
	other := newFakeHost(t, "both")
	rt, l := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "host-key"}, Host{Name: "other", URL: other.URL, Key: "host-key"})
	for _, model := range []string{"qwen", "cloud", "openrouter/cloud", "both", "nobody"} {
		before := l.served
		send(rt, "POST", "/v1/chat/completions", `{"model":"`+model+`"}`, nil)
		if l.served != before+1 {
			t.Errorf("%s went to another host", model)
		}
	}
	send(rt, "POST", "/v1/chat/completions", `{"model":"other/both"}`, nil)
	if _, body := other.last(); !strings.Contains(body, `"model":"both"`) {
		t.Error("a qualified name did not settle it")
	}
	// The names the list carries qualify the same way.
	for listed, want := range map[string]string{"zbox/qwen": "zbox/qwen", "other/both": "other/both", "qwen": "", "both": "", "openrouter/cloud": ""} {
		if got, _ := rt.QualifyName(listed); got != want {
			t.Errorf("%s qualified as %q, not %q", listed, got, want)
		}
	}
}

func TestAForwardedRequestIsNeverForwardedAgain(t *testing.T) {
	zbox := newFakeHost(t, "embed")
	rt, l := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "host-key"})
	send(rt, "POST", "/v1/embeddings", `{"model":"zbox/embed"}`, map[string]string{HopHeader: "1"})
	if l.served != 1 {
		t.Fatal("a request that already hopped was forwarded")
	}
}

// Two hosts that each still list a model the other has dropped: the request
// makes one hop and is answered, not passed back and forth.
func TestTwoHostsWithStaleListsDoNotLoop(t *testing.T) {
	var a, b *Router
	hits := 0
	aSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { hits++; a.ServeHTTP(rw, r) }))
	bSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { hits++; b.ServeHTTP(rw, r) }))
	defer aSrv.Close()
	defer bSrv.Close()
	a, _ = New([]Host{{Name: "b", URL: bSrv.URL}}, &local{}, func(string) bool { return false }, "", nopLog{})
	b, _ = New([]Host{{Name: "a", URL: aSrv.URL}}, &local{}, func(string) bool { return false }, "", nopLog{})
	a.hosts["b"].models, a.hosts["b"].online = []string{"gone"}, true
	b.hosts["a"].models, b.hosts["a"].online = []string{"gone"}, true
	resp, err := http.Post(aSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gone"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits != 2 || resp.StatusCode != http.StatusTeapot {
		t.Fatalf("hits %d, status %d", hits, resp.StatusCode)
	}
}

func TestAnOfflineHostKeepsItsModelsAndFailsAtOnce(t *testing.T) {
	state := t.TempDir()
	zbox := newFakeHost(t, "embed")
	rt, _ := newRouter(t, state, Host{Name: "zbox", URL: zbox.URL, Key: "host-key"})
	zbox.Close()
	rt.Poll("zbox")
	if !strings.Contains(send(rt, "GET", "/v1/models", "", nil).Body.String(), "zbox/embed") {
		t.Fatal("an offline host's model was dropped from the list")
	}
	start := time.Now()
	rec := send(rt, "POST", "/v1/embeddings", `{"model":"zbox/embed"}`, nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "zbox is offline") || time.Since(start) > time.Second {
		t.Fatalf("%d %s after %v", rec.Code, rec.Body, time.Since(start))
	}

	// A restart while it is down still knows its models.
	again, err := New([]Host{{Name: "zbox", URL: zbox.URL, Key: "host-key"}}, &local{}, isLocal, state, nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(send(again, "GET", "/v1/models", "", nil).Body.String(), "zbox/embed") {
		t.Fatal("the kept list was not read back")
	}
	if _, err := os.Stat(filepath.Join(state, "remote-zbox.json")); err != nil {
		t.Fatal(err)
	}
}

// Both happen well inside PollEvery, so only the repoll a request triggers
// can account for them.
func TestARequestMakesTheRouterLookAgain(t *testing.T) {
	zbox := newFakeHost(t, "embed")
	rt, err := New([]Host{{Name: "zbox", URL: zbox.URL, Key: "host-key"}}, &local{}, isLocal, "", nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.Start(ctx)
	online := func() bool { return rt.State()[0].Online }
	eventually := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	eventually("the first poll did not find the host", online)

	zbox.down.Store(true)
	if rec := send(rt, "POST", "/v1/embeddings", `{"model":"zbox/embed"}`, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("a host that did not answer: %d", rec.Code)
	}
	eventually("a host that did not answer is still listed as online", func() bool { return !online() })

	zbox.down.Store(false)
	if rec := send(rt, "POST", "/v1/embeddings", `{"model":"zbox/embed"}`, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("an offline host was tried: %d", rec.Code)
	}
	eventually("a host that came back is still offline", online)
}

func TestAStreamIsPassedOnAsItComes(t *testing.T) {
	zbox := newFakeHost(t, "embed")
	rt, _ := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "host-key"})
	srv := httptest.NewServer(rt)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/stream", "application/json", strings.NewReader(`{"model":"zbox/embed"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "data: 0\n" || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("first chunk %q after %v: held back until the end", line, time.Since(start))
	}
}

func TestAWebSocketIsPassedThrough(t *testing.T) {
	zbox := newFakeHost(t, "voice")
	rt, _ := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "host-key"})
	srv := httptest.NewServer(rt)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(conn, "GET /v1/realtime?model=zbox/voice HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	reader := bufio.NewReader(conn)
	status, _ := reader.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("status %q", status)
	}
	for line, _ := reader.ReadString('\n'); line != "\r\n"; line, _ = reader.ReadString('\n') {
	}
	fmt.Fprintf(conn, "hello\n")
	echo, _ := reader.ReadString('\n')
	if echo != "echo hello\n" {
		t.Fatalf("echo %q", echo)
	}
	if req, _ := zbox.last(); req.URL.Query().Get("model") != "voice" {
		t.Fatalf("model there: %q", req.URL.RawQuery)
	}
}

// infermuxOf is meta.infermux of one model in a /v1/models answer.
func infermuxOf(t *testing.T, rt http.Handler, id string) map[string]any {
	t.Helper()
	var list struct {
		Data []struct {
			ID   string
			Meta struct{ InferMux map[string]any }
		}
	}
	json.Unmarshal(send(rt, "GET", "/v1/models", "", nil).Body.Bytes(), &list)
	for _, m := range list.Data {
		if m.ID == id {
			return m.Meta.InferMux
		}
	}
	t.Fatalf("%s is not listed", id)
	return nil
}

func TestTheOtherHostsSettingsComeWithItsModels(t *testing.T) {
	state := t.TempDir()
	zbox := newFakeHost(t, "embed")
	rt, _ := newRouter(t, state, Host{Name: "zbox", URL: zbox.URL, Key: "host-key"})
	if m := infermuxOf(t, rt, "zbox/embed"); m["context_window"] != 8192.0 || m["host"] != "zbox" || m["online"] != true {
		t.Fatalf("listed %v", m)
	}

	// A change in the settings alone is kept, and read back after a restart
	// while the host is down.
	zbox.mu.Lock()
	zbox.context = 4096
	zbox.mu.Unlock()
	rt.Poll("zbox")
	zbox.Close()
	again, err := New([]Host{{Name: "zbox", URL: zbox.URL, Key: "host-key"}}, &local{}, isLocal, state, nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	again.Poll("zbox")
	if m := infermuxOf(t, again, "zbox/embed"); m["context_window"] != 4096.0 || m["online"] != false {
		t.Fatalf("after a restart: %v", m)
	}
}

func TestAListKeptBeforeTheSettingsIsStillRead(t *testing.T) {
	state := t.TempDir()
	os.WriteFile(filepath.Join(state, "remote-zbox.json"), []byte(`["embed"]`), 0o644)
	rt, err := New([]Host{{Name: "zbox", URL: "http://127.0.0.1:1", Key: "host-key"}}, &local{}, isLocal, state, nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	if m := infermuxOf(t, rt, "zbox/embed"); m["host"] != "zbox" {
		t.Fatalf("listed %v", m)
	}
}

func TestEachAsksEveryHostWithThisHostsKey(t *testing.T) {
	zbox := newFakeHost(t, "embed")
	other := newFakeHost(t)
	rt, _ := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "host-key"}, Host{Name: "other", URL: other.URL, Key: "host-key"})
	other.down.Store(true)
	replies := rt.Each(context.Background(), "/warden/requests")
	// by name, as the hosts are kept
	if len(replies) != 2 || replies[0].Host != "other" || replies[1].Host != "zbox" {
		t.Fatalf("%+v", replies)
	}
	if replies[1].Err != nil || !strings.Contains(string(replies[1].Body), `"path":"/warden/requests"`) {
		t.Errorf("zbox: %v %s", replies[1].Err, replies[1].Body)
	}
	if r, _ := zbox.last(); r.Header.Get("Authorization") != "Bearer host-key" || r.Header.Get(HopHeader) == "" {
		t.Errorf("sent %v", r.Header)
	}
	if replies[0].Err == nil {
		t.Error("a host that is down answered")
	}
	// the fake host answers /v1/models 401 to any other key
	wrong, _ := newRouter(t, "", Host{Name: "zbox", URL: zbox.URL, Key: "stale-key"})
	if replies := wrong.Each(context.Background(), "/v1/models"); replies[0].Err == nil {
		t.Errorf("a refusal was an answer: %s", replies[0].Body)
	}
}
