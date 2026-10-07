package stats

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/stream"
	"github.com/mostlygeek/llama-swap/internal/stream/wstest"
)

// session runs a handler that takes the connection over as the reverse
// proxy does, on a pipe whose client end client drives, all on the test's
// clock.
func session(t *testing.T, rec *Recorder, c *clock, client func(net.Conn), backend func(conn net.Conn)) {
	t.Helper()
	s := stream.New(c.t, c.now)
	h := rec.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		server, far := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer far.Close()
			client(far)
		}()
		backend(s.Attach(server))
		server.Close()
		<-done
	}))
	req := httptest.NewRequest("GET", "/v1/realtime?model=whisper", nil)
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(stream.With(req.Context(), s)))
}

func TestASessionIsOneRow(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	rec.Model = func(*http.Request) (string, bool) { return "reaperboi/whisper", true }
	audio := wstest.Frame(wstest.Binary, make([]byte, 3200), true)
	bye := wstest.Frame(wstest.Close, []byte{0x03, 0xe8}, true)
	delta := wstest.Frame(wstest.Text, []byte(`{"type":"conversation.item.input_audio_transcription.delta","delta":"hej"}`), false)
	session(t, rec, c, func(conn net.Conn) {
		conn.Write(audio)
		io.ReadFull(conn, make([]byte, len(delta)))
		conn.Write(bye)
	}, func(conn net.Conn) {
		c.wait(1000) // the user starts to speak
		io.ReadFull(conn, make([]byte, len(audio)))
		c.wait(300)
		conn.Write(delta)
		c.wait(30000) // and stops
		io.ReadFull(conn, make([]byte, len(bye)))
	})
	got := rec.Recent()
	if len(got) != 1 {
		t.Fatalf("%d rows", len(got))
	}
	r := got[0]
	if r.Status != 101 || r.Model != "reaperboi/whisper" || r.Path != "/v1/realtime" || r.Session == nil {
		t.Fatalf("%+v", r)
	}
	s := r.Session
	// The upgrade came at once; the first output 300 ms after the audio; the
	// 10 s after each data frame are active, the close frame is not data.
	if s.UpgradeMs != 0 || val(r.TTFTMs) != 1300.0 || val(s.FirstOutputMs) != 300.0 || r.DurationMs != 31300 {
		t.Errorf("upgrade %v ttft %v first output %v duration %v", s.UpgradeMs, val(r.TTFTMs), val(s.FirstOutputMs), r.DurationMs)
	}
	if s.ActiveMs != 11300 || s.IdleMs != 20000 {
		t.Errorf("active %v idle %v, want 1000+300+10000 and 20000", s.ActiveMs, s.IdleMs)
	}
	if r.RequestBytes != int64(len(audio)+len(bye)) || r.ResponseBytes != int64(len(delta)) {
		t.Errorf("bytes %d %d", r.RequestBytes, r.ResponseBytes)
	}
	if s.CloseCode != 1000 || s.ClosedBy != "client" {
		t.Errorf("closed %d by %q", s.CloseCode, s.ClosedBy)
	}
	raw, _ := json.Marshal(r)
	for _, field := range []string{`"status":101`, `"session":{"upgrade_ms":0,"first_output_ms":300,"active_ms":11300,"idle_ms":20000,"close_code":1000,"closed_by":"client"}`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("%s has no %s", raw, field)
		}
	}
}

func TestARefusedUpgradeIsARequest(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	rec.Model = func(*http.Request) (string, bool) { return "reaperboi/whisper", true }
	h := rec.Wrap(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c.wait(40)
		http.Error(rw, "no", http.StatusBadGateway)
	}))
	req := httptest.NewRequest("GET", "/v1/realtime?model=whisper", nil)
	s := stream.New(c.t, c.now)
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(stream.With(req.Context(), s)))
	if r := rec.Recent()[0]; r.Status != 502 || r.Session != nil || r.DurationMs != 40 {
		t.Errorf("%+v", r)
	}
}

// A GET that the warden does not follow as a session is not recorded.
func TestAGetWithoutASessionIsNotRecorded(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	rec.Model = func(*http.Request) (string, bool) { return "reaperboi/whisper", true }
	rec.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/upstream/whisper/props", nil))
	if n := len(rec.Recent()); n != 0 {
		t.Fatalf("recorded %d", n)
	}
}

func TestAnAudioReplysFirstChunkIsItsFirstOutput(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rec := recorder(c)
	serve(rec, c, "audio/pcm", `{"model":"kokoro","input":"hej"}`, []part{{0, ""}, {250, "\x01\x02"}, {100, "\x03\x04"}})
	r := rec.Recent()[0]
	if val(r.TTFTMs) != 250.0 || r.Stream || r.ResponseBytes != 4 || r.DurationMs != 350 {
		t.Errorf("ttft %v stream %v bytes %d duration %v", val(r.TTFTMs), r.Stream, r.ResponseBytes, r.DurationMs)
	}
}

func TestSessionsAreSummarisedApart(t *testing.T) {
	session := func(ttft, first float64) Request {
		return Request{Model: "m", Status: 101, TTFTMs: &ttft, Session: &Session{FirstOutputMs: &first}}
	}
	ttft := 200.0
	sums := Summarize([]Request{
		{Model: "m", Status: 200, TTFTMs: &ttft},
		session(60000, 300),
		session(9000, 500),
		session(4000, 400),
		{Model: "m", Status: 502},
	})
	s := sums[0]
	if s.Requests != 2 || s.Failed != 1 || s.Sessions != 3 {
		t.Errorf("requests %d failed %d sessions %d", s.Requests, s.Failed, s.Sessions)
	}
	if s.TTFTMs.N != 1 || s.TTFTMs.Median != 200 {
		t.Errorf("ttft %+v: a session's must stay out", *s.TTFTMs)
	}
	if s.FirstOutputMs == nil || s.FirstOutputMs.Median != 400 || s.FirstOutputMs.P95 != 500 || s.FirstOutputMs.N != 3 {
		t.Errorf("first output %+v", s.FirstOutputMs)
	}
}
