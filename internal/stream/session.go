// Package stream is what InferMux knows about a WebSocket between a client
// and a model's server (0018). The bytes pass through unchanged, binary
// frames included. A Session reads each frame's header in both directions to
// know where frames start and end and which carry data, so the warden can
// tell a session that moves data from one that is only open, the stats can
// time it, and InferMux can end it with a close frame of its own at a frame
// boundary.
//
// It imports nothing from llama-swap, so the warden can use it.
package stream

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ActiveWindow is how long after a data frame a session counts as moving
// data (0018, 6): in flight for the warden, and active time in the stats.
const ActiveWindow = 10 * time.Second

// The close codes InferMux sends (RFC 6455 7.4, and IANA's registry).
const (
	// ServiceRestart: the model configuration reloaded. Reconnect.
	ServiceRestart = 1012
	// TryAgainLater: the GPU was yielded or the models unloaded. Reconnect
	// when there is something to send; a new session loads the model.
	TryAgainLater = 1013
)

// peekLimit is the largest text message of the backend's read for the first
// output. A larger one is passed on unread.
const peekLimit = 64 << 10

// Session is one WebSocket, from the request's arrival to its end. Its
// clock is read once per read or write that carries data, never per byte.
type Session struct {
	start time.Time
	now   func() time.Time

	// Offsets from start, in nanoseconds. 0 is not yet.
	upgraded atomic.Int64
	last     atomic.Int64 // the last data frame, or the upgrade before one
	firstIn  atomic.Int64 // the client's first data frame
	firstOut atomic.Int64 // the backend's first output
	active   atomic.Int64 // up to last; the time after it is added in Report
	idle     atomic.Int64
	in, out  atomic.Int64 // bytes, client to backend and back

	closing atomic.Pointer[closeSeen]
	ended   atomic.Bool

	// wmu serialises writes to the client: the proxy's, and InferMux's
	// close frame, which has to fall between two frames.
	wmu      sync.Mutex
	conn     net.Conn
	toClient side
	// fromClient is only touched by the one goroutine that reads.
	fromClient side
}

type closeSeen struct {
	code int
	by   string
}

// New is a session for a request that arrived at start. now is the clock,
// time.Now outside tests.
func New(start time.Time, now func() time.Time) *Session {
	s := &Session{start: start, now: now}
	s.fromClient.fromClient = true
	return s
}

func (s *Session) offset() int64 { return max(int64(s.now().Sub(s.start)), 1) }

// Attach is the client's connection as the upgrade hands it over. Every byte
// after the upgrade goes through the returned connection unchanged.
func (s *Session) Attach(conn net.Conn) net.Conn {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	at := s.offset()
	s.conn = conn
	s.last.Store(at)
	s.upgraded.Store(at)
	return &clientConn{Conn: conn, s: s}
}

// LastData is when the last data frame crossed either way, or the upgrade
// before the first. False before the upgrade.
func (s *Session) LastData() (time.Time, bool) {
	last := s.last.Load()
	if last == 0 {
		return time.Time{}, false
	}
	return s.start.Add(time.Duration(last)), true
}

// Moving is whether data crossed within ActiveWindow of now. False before the
// upgrade, as upgraded says.
func (s *Session) Moving(now time.Time) (moving, upgraded bool) {
	last, ok := s.LastData()
	if !ok {
		return false, false
	}
	return now.Sub(last) < ActiveWindow, true
}

// touch stamps a data frame. Both directions call it, so the stamp only
// moves forward, and each gap between two stamps is split into active and
// idle time once.
func (s *Session) touch() {
	now := s.offset()
	for {
		prev := s.last.Load()
		if now <= prev {
			return
		}
		if s.last.CompareAndSwap(prev, now) {
			s.account(now - prev)
			return
		}
	}
}

func (s *Session) account(gap int64) {
	if w := int64(ActiveWindow); gap > w {
		s.active.Add(w)
		s.idle.Add(gap - w)
		return
	}
	s.active.Add(gap)
}

// Close ends the session from InferMux's side: a close frame with code and
// reason to the client, when the stream toward it is between two frames,
// and then the client's connection, which ends the backend's too. False
// before the upgrade, when there is no session to close yet. It never waits
// on a client that is slow to read: a write in progress gets no frame.
func (s *Session) Close(code int, reason string) bool {
	if s.upgraded.Load() == 0 || s.ended.Swap(true) {
		return false
	}
	sent := false
	if s.wmu.TryLock() {
		if s.toClient.between() {
			// The connection's deadline is on the real clock, whatever now is.
			s.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, err := s.conn.Write(closeFrame(code, reason))
			sent = err == nil
		}
		s.wmu.Unlock()
	}
	seen := &closeSeen{code: code, by: "infermux"}
	if !sent {
		// What a browser reports for a connection that ended without a
		// close frame. It is never sent.
		seen.code = 1006
	}
	s.closing.CompareAndSwap(nil, seen)
	s.conn.Close()
	return true
}

func closeFrame(code int, reason string) []byte {
	// A control frame carries at most 125 bytes: the code and 123 of reason.
	for len(reason) > 123 {
		_, size := utf8.DecodeLastRuneInString(reason)
		reason = reason[:len(reason)-size]
	}
	f := make([]byte, 4, 4+len(reason))
	f[0], f[1] = 0x88, byte(2+len(reason))
	binary.BigEndian.PutUint16(f[2:], uint16(code))
	return append(f, reason...)
}

// clientConn is the client's connection as the proxy sees it.
type clientConn struct {
	net.Conn
	s *Session
}

func (c *clientConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		s := c.s
		s.in.Add(int64(n))
		if s.scan(&s.fromClient, b[:n]) {
			s.touch()
		}
	}
	return n, err
}

func (c *clientConn) Write(b []byte) (int, error) {
	s := c.s
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.ended.Load() {
		return 0, net.ErrClosed
	}
	// Followed before the write, so the stamp is in place by the time the
	// client has the bytes. A write that fails part way ends the session.
	if s.scan(&s.toClient, b) {
		s.touch()
	}
	n, err := c.Conn.Write(b)
	s.out.Add(int64(n))
	return n, err
}

// CloseWrite passes the backend's end on, as the proxy does to a connection
// that can half-close.
func (c *clientConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// Report is what a session did, for the stats (0018, 8).
type Report struct {
	Upgraded bool
	// Upgrade is from the arrival to the 101.
	Upgrade time.Duration
	// Output is from the arrival to the backend's first output.
	Output *time.Duration
	// FirstOutput is from the client's first data frame to the backend's
	// first output.
	FirstOutput  *time.Duration
	In, Out      int64
	Active, Idle time.Duration
	// CloseCode is the first close frame's, either way; 0 when none was
	// seen, 1005 for one without a code.
	CloseCode int
	ClosedBy  string
}

// Report is the session as of end.
func (s *Session) Report(end time.Time) Report {
	r := Report{In: s.in.Load(), Out: s.out.Load()}
	up := s.upgraded.Load()
	if up == 0 {
		return r
	}
	r.Upgraded, r.Upgrade = true, time.Duration(up)
	active, idle := s.active.Load(), s.idle.Load()
	if tail := int64(end.Sub(s.start)) - s.last.Load(); tail > 0 {
		w := int64(ActiveWindow)
		active += min(tail, w)
		idle += max(tail-w, 0)
	}
	r.Active, r.Idle = time.Duration(active), time.Duration(idle)
	if out := s.firstOut.Load(); out != 0 {
		d := time.Duration(out)
		r.Output = &d
		if in := s.firstIn.Load(); in != 0 && in <= out {
			first := time.Duration(out - in)
			r.FirstOutput = &first
		}
	}
	if c := s.closing.Load(); c != nil {
		r.CloseCode, r.ClosedBy = c.code, c.by
	}
	return r
}

type contextKey struct{}

// With is ctx carrying the session, for the handlers inside the one that
// made it.
func With(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// From is the request's session, or nil.
func From(ctx context.Context) *Session {
	s, _ := ctx.Value(contextKey{}).(*Session)
	return s
}
