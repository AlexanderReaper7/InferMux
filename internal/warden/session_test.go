package warden

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- sessions (0018) -----------------------------------------------------------

// lockedClock is the harness's time for a test whose proxy goroutines read
// it while the test moves it.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) at(d time.Duration) {
	c.mu.Lock()
	c.t = t0.Add(d)
	c.mu.Unlock()
}

// wsFrame is one frame, masked as a client's must be.
func wsFrame(op byte, payload []byte, masked bool) []byte {
	var b bytes.Buffer
	b.WriteByte(0x80 | op)
	m := byte(0)
	if masked {
		m = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b.WriteByte(m | byte(n))
	case n < 1<<16:
		b.WriteByte(m | 126)
		binary.Write(&b, binary.BigEndian, uint16(n))
	default:
		b.WriteByte(m | 127)
		binary.Write(&b, binary.BigEndian, uint64(n))
	}
	if !masked {
		b.Write(payload)
		return b.Bytes()
	}
	mask := [4]byte{9, 8, 7, 6}
	b.Write(mask[:])
	for i, c := range payload {
		b.WriteByte(c ^ mask[i%4])
	}
	return b.Bytes()
}

func readWSFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var e [2]byte
		io.ReadFull(r, e[:])
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		io.ReadFull(r, e[:])
		n = binary.BigEndian.Uint64(e[:])
	}
	var mask [4]byte
	if h[1]&0x80 != 0 {
		io.ReadFull(r, mask[:])
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	if h[1]&0x80 != 0 {
		for i := range p {
			p[i] ^= mask[i%4]
		}
	}
	return h[0] & 0x0f, p, nil
}

// echoBackend is a model's server that speaks WebSocket: it sends every data
// frame back, answers a ping, and returns a close.
func echoBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(rw).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		brw.Flush()
		for {
			op, p, err := readWSFrame(brw.Reader)
			if err != nil {
				return
			}
			switch op {
			case 8:
				conn.Write(wsFrame(8, p, false))
				return
			case 9:
				conn.Write(wsFrame(10, p, false))
			default:
				conn.Write(wsFrame(op, p, false))
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wsClient is a client with one session open through the warden.
type wsClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func openSession(t *testing.T, front *httptest.Server, key string) *wsClient {
	t.Helper()
	u, _ := url.Parse(front.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET /v1/realtime?model=qwen HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer %s\r\n\r\n", u.Host, key)
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	return &wsClient{t: t, conn: conn, r: r}
}

// roundTrip sends one frame and waits for the backend's answer.
func (c *wsClient) roundTrip(op byte, payload string) (byte, []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(wsFrame(op, []byte(payload), true)); err != nil {
		c.t.Fatal(err)
	}
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, p, err := readWSFrame(c.r)
	if err != nil {
		c.t.Fatalf("no answer to %q: %v", payload, err)
	}
	return got, p
}

// closed is the close frame InferMux sent, and whether the connection then
// ended.
func (c *wsClient) closed() (code int, reason string) {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	op, p, err := readWSFrame(c.r)
	if err != nil || op != 8 || len(p) < 2 {
		c.t.Fatalf("no close frame: op %d %x %v", op, p, err)
	}
	if _, _, err := readWSFrame(c.r); err == nil {
		c.t.Fatal("the connection went on after the close frame")
	}
	return int(binary.BigEndian.Uint16(p)), string(p[2:])
}

func sessionHarness(t *testing.T) (*harness, *lockedClock, *httptest.Server) {
	t.Helper()
	h := newHarness(t, nil)
	c := &lockedClock{t: t0}
	h.w.now = c.now
	h.w.announcer.now = c.now
	backend, _ := url.Parse(echoBackend(t).URL)
	front := httptest.NewServer(h.w.Wrap(httputil.NewSingleHostReverseProxy(backend)))
	t.Cleanup(front.Close)
	return h, c, front
}

func (h *harness) sessions() []FlightState {
	var out []FlightState
	for _, f := range h.w.traffic.state().InFlight {
		if f.Session != nil {
			out = append(out, f)
		}
	}
	return out
}

func TestAnIdleSessionNoLongerHoldsTheCard(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	if op, p := client.roundTrip(2, "audio"); op != 2 || string(p) != "audio" {
		t.Fatalf("echo %d %q", op, p)
	}
	h.reading = busy(60)
	c.at(2 * time.Second)
	h.w.Tick()
	if h.models.unloads != 0 || !h.w.State().PendingUnload {
		t.Fatal("unloaded under a session that just moved data")
	}
	if s := h.sessions(); len(s) != 1 || !s[0].Session.Moving {
		t.Fatalf("sessions %+v", s)
	}
	// Open, and idle: it counts from its last data, as a request's end.
	c.at(time.Second + 599*time.Second)
	h.w.Tick()
	if h.models.unloads != 0 {
		t.Fatal("unloaded within interactive_recent_seconds of the last data")
	}
	if s := h.sessions(); len(s) != 1 || s[0].Session.Moving || s[0].Session.IdleSeconds != 599 {
		t.Fatalf("sessions %+v", s)
	}
	c.at(time.Second + 600*time.Second)
	h.w.Tick()
	if h.models.unloads != 1 {
		t.Fatal("an open, idle session kept the models loaded")
	}
	code, reason := client.closed()
	if code != 1013 || reason != "infermux: the GPU was yielded: "+h.w.State().Verdict.Reason {
		t.Fatalf("closed with %d %q", code, reason)
	}
}

func TestAPingDoesNotKeepASessionMoving(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	client.roundTrip(1, `{"type":"session.update"}`)
	h.reading = obsWithCUDA()
	for at := 5 * time.Second; at <= 9*time.Second; at += 4 * time.Second {
		c.at(at)
		if op, _ := client.roundTrip(9, "keep"); op != 10 {
			t.Fatalf("no pong: %d", op)
		}
		h.w.Tick()
		if h.models.unloads != 0 {
			t.Fatalf("a priority process unloaded %s after the last data", at-time.Second)
		}
	}
	c.at(11 * time.Second)
	client.roundTrip(9, "keep")
	h.w.Tick()
	if h.models.unloads != 1 {
		t.Fatal("pings kept a session in flight past its window")
	}
	if code, _ := client.closed(); code != 1013 {
		t.Fatalf("closed with %d", code)
	}
}

func TestASessionMovingDataDefersEvenAPriorityUnload(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	h.reading = obsWithCUDA()
	for at := time.Second; at <= 41*time.Second; at += 8 * time.Second {
		c.at(at)
		client.roundTrip(2, "pcm")
		c.at(at + 7*time.Second)
		h.w.Tick()
		if h.models.unloads != 0 {
			t.Fatalf("unloaded under the user speaking, at %s", at+7*time.Second)
		}
	}
	c.at(41*time.Second + 10*time.Second)
	h.w.Tick()
	if h.models.unloads != 1 {
		t.Fatal("not unloaded once the session went quiet")
	}
	client.closed()
}

func TestABatchSessionIsClosedWhenTheGPUYields(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "batch-key")
	c.at(time.Second)
	client.roundTrip(2, "pcm")
	h.reading = busy(60)
	h.w.Tick()
	code, reason := client.closed()
	if code != 1013 || !strings.HasPrefix(reason, "infermux: batch session cancelled, the GPU was yielded: ") {
		t.Fatalf("closed with %d %q", code, reason)
	}
	if h.w.State().Traffic.CancelledBatch != 1 {
		t.Fatal("not counted as a cancelled batch request")
	}
}

func TestTheEndOfAnIdleSessionIsNotActivity(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	client.roundTrip(2, "pcm")
	c.at(time.Hour)
	if op, _ := client.roundTrip(8, "\x03\xe8"); op != 8 {
		t.Fatal("the backend did not return the close")
	}
	client.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.w.traffic.state().InFlight) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	last := h.w.State().Traffic.LastInteractive
	if last == nil || !last.Equal(t0.Add(time.Second)) {
		t.Fatalf("last interactive %v, want the last data at %v", last, t0.Add(time.Second))
	}
}

func TestUnloadByHandWaitsOnlyForAMovingSession(t *testing.T) {
	h, c, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	c.at(time.Second)
	client.roundTrip(2, "pcm")
	c.at(2 * time.Second)
	if _, err := h.w.UnloadNow(); err == nil {
		t.Fatal("unloaded by hand under a session moving data")
	}
	c.at(12 * time.Second)
	if _, err := h.w.UnloadNow(); err != nil {
		t.Fatalf("an idle session refused the unload: %v", err)
	}
	if code, reason := client.closed(); code != 1013 || reason != "infermux: the models were unloaded by hand" {
		t.Fatalf("closed with %d %q", code, reason)
	}
}

func TestAReloadClosesSessionsWithServiceRestart(t *testing.T) {
	h, _, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	if n := h.w.CloseSessions(1012, "infermux: the model configuration was reloaded"); n != 1 {
		t.Fatalf("closed %d", n)
	}
	if code, _ := client.closed(); code != 1012 {
		t.Fatalf("closed with %d", code)
	}
}

func TestBinaryFramesPassThroughUnchanged(t *testing.T) {
	_, _, front := sessionHarness(t)
	client := openSession(t, front, "my-key")
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, 300000} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i * 7)
		}
		op, got := client.roundTrip(2, string(payload))
		if op != 2 || !bytes.Equal(got, payload) {
			t.Fatalf("%d bytes came back as op %d, %d bytes", n, op, len(got))
		}
	}
}
