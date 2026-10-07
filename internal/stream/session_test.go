package stream

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// clock is a test's time, moved by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// frame is one WebSocket frame, masked as a client's is.
func frame(op byte, fin, masked bool, payload []byte) []byte {
	var b bytes.Buffer
	h := op
	if fin {
		h |= 0x80
	}
	b.WriteByte(h)
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
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	b.Write(mask[:])
	for i, c := range payload {
		b.WriteByte(c ^ mask[i%4])
	}
	return b.Bytes()
}

func closePayload(code int, reason string) []byte {
	p := binary.BigEndian.AppendUint16(nil, uint16(code))
	return append(p, reason...)
}

// feed gives the side b in pieces cut at random, and says after which pieces
// data was seen.
func feed(s *Session, d *side, b []byte, rng *rand.Rand) {
	for len(b) > 0 {
		n := 1 + rng.IntN(min(len(b), 40))
		if rng.IntN(4) == 0 {
			n = len(b)
		}
		s.scan(d, b[:n])
		b = b[n:]
	}
}

func TestFramesAreFollowedHoweverTheReadsCutThem(t *testing.T) {
	big := bytes.Repeat([]byte{0xab}, 70000)
	for _, masked := range []bool{true, false} {
		var stream []byte
		stream = append(stream, frame(9, true, masked, []byte("ping"))...)
		stream = append(stream, frame(2, true, masked, nil)...)
		stream = append(stream, frame(2, true, masked, make([]byte, 125))...)
		stream = append(stream, frame(2, true, masked, make([]byte, 126))...)
		stream = append(stream, frame(1, false, masked, []byte(`{"type":"x`))...)
		stream = append(stream, frame(10, true, masked, nil)...) // a pong between two fragments
		stream = append(stream, frame(0, true, masked, []byte(`"}`))...)
		stream = append(stream, frame(2, true, masked, make([]byte, 65535))...)
		stream = append(stream, frame(2, true, masked, big)...)
		stream = append(stream, frame(8, true, masked, closePayload(1000, "bye"))...)
		for seed := range uint64(200) {
			s := New(t0, time.Now)
			s.fromClient.fromClient = masked
			rng := rand.New(rand.NewPCG(seed, 7))
			feed(s, &s.fromClient, stream, rng)
			if !s.fromClient.between() {
				t.Fatalf("masked %v, seed %d: not between two frames at the end", masked, seed)
			}
			c := s.closing.Load()
			if c == nil || c.code != 1000 {
				t.Fatalf("masked %v, seed %d: close code %+v", masked, seed, c)
			}
		}
	}
}

func TestOnlyDataFramesAreData(t *testing.T) {
	s := New(t0, time.Now)
	for _, c := range []struct {
		name  string
		frame []byte
		data  bool
	}{
		{"ping", frame(9, true, true, []byte("keep")), false},
		{"pong", frame(10, true, true, nil), false},
		{"text", frame(1, true, true, []byte("hi")), true},
		{"binary", frame(2, true, true, []byte{1, 2}), true},
		{"empty binary", frame(2, true, true, nil), true},
		{"continuation", frame(0, true, true, []byte("x")), true},
		{"close", frame(8, true, true, closePayload(1000, "")), false},
	} {
		if got := s.scan(&s.fromClient, c.frame); got != c.data {
			t.Errorf("%s: data %v, want %v", c.name, got, c.data)
		}
	}
	// The payload of a big frame, read on its own, is data too.
	head := frame(2, true, true, make([]byte, 1000))
	s.scan(&s.fromClient, head[:8])
	if !s.scan(&s.fromClient, head[8:500]) {
		t.Error("the middle of a binary frame was not data")
	}
}

// pipe is a session attached to one end of a connection; the test reads
// the other end as the client.
func pipe(t *testing.T, s *Session) (proxied net.Conn, client net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	return s.Attach(server), client
}

func TestASessionMovesOnlyWhileDataCrosses(t *testing.T) {
	c := &clock{t: t0}
	s := New(t0, c.now)
	if moving, up := s.Moving(c.now()); moving || up {
		t.Fatal("a session before its upgrade is neither")
	}
	c.add(2 * time.Second) // the model loaded
	conn, client := pipe(t, s)
	go func() {
		buf := make([]byte, 1<<16)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	if moving, up := s.Moving(c.now()); !moving || !up {
		t.Fatal("the upgrade is the first activity")
	}
	c.add(9 * time.Second)
	conn.Write(frame(9, true, false, []byte("ping")))
	c.add(2 * time.Second)
	if moving, _ := s.Moving(c.now()); moving {
		t.Fatal("a ping kept the session moving")
	}
	conn.Write(frame(2, true, false, []byte("audio")))
	if moving, _ := s.Moving(c.now()); !moving {
		t.Fatal("a binary frame did not move the session")
	}
	last, _ := s.LastData()
	if want := t0.Add(13 * time.Second); !last.Equal(want) {
		t.Fatalf("last data %v, want %v", last, want)
	}
	c.add(ActiveWindow)
	if moving, _ := s.Moving(c.now()); moving {
		t.Fatal("still moving a whole window after the last data")
	}
	c.add(25 * time.Second)
	r := s.Report(c.now())
	// Upgrade at 2 s. 11 s to the binary frame: 10 active, 1 idle. 35 s
	// after it: 10 active, 25 idle.
	if r.Upgrade != 2*time.Second || r.Active != 20*time.Second || r.Idle != 26*time.Second {
		t.Fatalf("upgrade %v, active %v, idle %v", r.Upgrade, r.Active, r.Idle)
	}
}

func TestTheFirstOutputIsTimedFromTheFirstInput(t *testing.T) {
	c := &clock{t: t0}
	s := New(t0, c.now)
	c.add(time.Second)
	conn, client := pipe(t, s)
	go func() {
		buf := make([]byte, 1<<16)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	c.add(time.Second)
	conn.Write(frame(1, true, false, []byte(`{"type":"session.created"}`)))
	conn.Write(frame(1, true, false, []byte(`{"type":"config","useAudioWorklet":true}`)))
	// The client starts talking at 3 s.
	c.add(time.Second)
	go client.Write(frame(2, true, true, []byte("pcm")))
	if _, err := conn.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	c.add(1500 * time.Millisecond)
	conn.Write(frame(1, true, false, []byte(`{"lines":[],"buffer_transcription":""}`)))
	if r := s.Report(c.now()); r.Output != nil {
		t.Fatal("an update without text was taken as output")
	}
	c.add(500 * time.Millisecond)
	msg := frame(1, true, false, []byte(`{"type":"conversation.item.input_audio_transcription.delta","delta":"hej"}`))
	conn.Write(msg[:5])
	conn.Write(msg[5:])
	r := s.Report(c.now())
	if r.Output == nil || *r.Output != 5*time.Second || r.FirstOutput == nil || *r.FirstOutput != 2*time.Second {
		t.Fatalf("output %v, first output %v", r.Output, r.FirstOutput)
	}
	if r.In != int64(len(frame(2, true, true, []byte("pcm")))) {
		t.Errorf("in %d bytes", r.In)
	}
}

func TestOutput(t *testing.T) {
	for msg, want := range map[string]bool{
		`{"type":"response.output_audio.delta","delta":"UklG"}`:                                 true,
		`{"type":"response.output_text.delta","delta":"a"}`:                                     true,
		`{"type":"conversation.item.input_audio_transcription.completed","transcript":"hello"}`: true,
		`{"type":"Results","channel":{"alternatives":[{"transcript":"hello"}]}}`:                true,
		`{"status":"active_transcription","lines":[{"text":"hej"}],"buffer_transcription":""}`:  true,
		`{"status":"active_transcription","lines":[],"buffer_transcription":"he"}`:              true,
		`{"type":"session.created","session":{}}`:                                               false,
		`{"type":"Results","channel":{"alternatives":[{"transcript":""}]}}`:                     false,
		`{"type":"Metadata"}`:                      false,
		`{"type":"config","useAudioWorklet":true}`: false,
		`not json`: false,
	} {
		if got := carriesOutput([]byte(msg)); got != want {
			t.Errorf("%s: %v", msg, got)
		}
	}
}

func TestABinaryFrameFromTheBackendIsOutput(t *testing.T) {
	c := &clock{t: t0}
	s := New(t0, c.now)
	conn, client := pipe(t, s)
	go client.Read(make([]byte, 64))
	c.add(time.Second)
	conn.Write(frame(2, true, false, []byte{0, 1, 2, 3}))
	if r := s.Report(c.now()); r.Output == nil || *r.Output != time.Second {
		t.Fatalf("output %v", r.Output)
	}
}

func TestInferMuxClosesBetweenFramesWithItsCode(t *testing.T) {
	s := New(t0, time.Now)
	if s.Close(TryAgainLater, "x") {
		t.Fatal("closed a session that has not been upgraded")
	}
	conn, client := pipe(t, s)
	sent := frame(2, true, false, []byte("chunk"))
	got := make(chan []byte)
	go func() {
		var all []byte
		buf := make([]byte, 256)
		for {
			n, err := client.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				got <- all
				return
			}
		}
	}()
	conn.Write(sent)
	reason := "infermux: the GPU was yielded: " + strings.Repeat("ö", 80)
	if !s.Close(TryAgainLater, reason) {
		t.Fatal("not closed")
	}
	all := <-got
	if !bytes.HasPrefix(all, sent) {
		t.Fatalf("the frame before the close was not passed on whole: %x", all)
	}
	f := all[len(sent):]
	if len(f) < 4 || f[0] != 0x88 || int(f[1]) != len(f)-2 || len(f)-2 > 125 {
		t.Fatalf("not one close frame within 125 bytes: %x", f)
	}
	if code := binary.BigEndian.Uint16(f[2:]); code != TryAgainLater {
		t.Fatalf("code %d", code)
	}
	if !strings.HasPrefix(reason, string(f[4:])) || !strings.HasPrefix(string(f[4:]), "infermux: the GPU was yielded") {
		t.Fatalf("reason %q", f[4:])
	}
	if r := s.Report(time.Now()); r.CloseCode != TryAgainLater || r.ClosedBy != "infermux" {
		t.Fatalf("recorded %d by %q", r.CloseCode, r.ClosedBy)
	}
	if _, err := conn.Write(sent); err == nil {
		t.Fatal("the proxy could still write after the close")
	}
}

func TestMidFrameTheConnectionIsClosedWithoutAFrame(t *testing.T) {
	s := New(t0, time.Now)
	conn, client := pipe(t, s)
	got := make(chan []byte)
	go func() {
		var all []byte
		buf := make([]byte, 256)
		for {
			n, err := client.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				got <- all
				return
			}
		}
	}()
	half := frame(2, true, false, make([]byte, 100))[:50]
	conn.Write(half)
	s.Close(TryAgainLater, "x")
	if all := <-got; !bytes.Equal(all, half) {
		t.Fatalf("a close frame went into the middle of a frame: %x", all[len(half):])
	}
	if r := s.Report(time.Now()); r.CloseCode != 1006 || r.ClosedBy != "infermux" {
		t.Fatalf("recorded %d by %q", r.CloseCode, r.ClosedBy)
	}
}

func TestTheClientsCloseIsRecorded(t *testing.T) {
	s := New(t0, time.Now)
	conn, client := pipe(t, s)
	go client.Write(frame(8, true, true, closePayload(1000, "done")))
	conn.Read(make([]byte, 64))
	if r := s.Report(time.Now()); r.CloseCode != 1000 || r.ClosedBy != "client" {
		t.Fatalf("recorded %d by %q", r.CloseCode, r.ClosedBy)
	}
}

func TestRoute(t *testing.T) {
	upgrade := map[string]string{"Connection": "keep-alive, Upgrade", "Upgrade": "websocket"}
	for _, c := range []struct {
		method, target string
		header         map[string]string
		want           string
	}{
		{"GET", "/v1/realtime?model=qwen", upgrade, "/upstream/qwen/v1/realtime?model=qwen"},
		{"GET", "/v1/listen?model=whisper&language=sv", upgrade, "/upstream/whisper/v1/listen?model=whisper&language=sv"},
		{"GET", "/asr?language=sv&model=org%2Fwhisper", upgrade, "/upstream/org/whisper/asr?language=sv&model=org%2Fwhisper"},
		{"GET", "/v1/realtime?intent=transcription", upgrade, "/v1/realtime?intent=transcription"},
		{"GET", "/v1/realtime?model=qwen", nil, "/v1/realtime?model=qwen"},
		{"GET", "/v1/realtime?model=qwen", map[string]string{"Upgrade": "websocket"}, "/v1/realtime?model=qwen"},
		{"GET", "/v1/realtime?model=qwen", map[string]string{"Connection": "upgrade", "Upgrade": "h2c, WebSocket"}, "/upstream/qwen/v1/realtime?model=qwen"},
		{"POST", "/v1/realtime?model=qwen", upgrade, "/v1/realtime?model=qwen"},
		{"GET", "/upstream/qwen/ws?model=other", upgrade, "/upstream/qwen/ws?model=other"},
		{"GET", "/comfyui/ws?model=x", upgrade, "/comfyui/ws?model=x"},
		{"GET", "/api/events?model=x", upgrade, "/api/events?model=x"},
		{"GET", "/warden/verdict?model=x", upgrade, "/warden/verdict?model=x"},
	} {
		var got string
		h := Route(httpHandler(func(path, query string) { got = path + "?" + query }))
		req := httptest.NewRequest(c.method, c.target, nil)
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got != c.want {
			t.Errorf("%s %s: %s, want %s", c.method, c.target, got, c.want)
		}
		// The warden tracks what WebSocket says yes to, so that has to cover
		// everything Route sends to a model.
		if Routed(req) != "" && !WebSocket(req) {
			t.Errorf("%s %s %v: routed, and not a WebSocket for the warden", c.method, c.target, c.header)
		}
	}
}

// httpHandler hands the test the path and query a request arrived with.
type httpHandler func(path, query string)

func (h httpHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	h(r.URL.Path, r.URL.RawQuery)
}
