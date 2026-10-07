package stream

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"
)

// sink takes every write, as a fast client would.
type sink struct{ net.Conn }

func (sink) Write(b []byte) (int, error) { return len(b), nil }

// source hands out one frame per read, as a client's frames arrive.
type source struct {
	net.Conn
	frame []byte
}

func (s source) Read(b []byte) (int, error) { return copy(b, s.frame), nil }

// The cost the session adds to one frame, each way: the proxy calls Read on
// the client's connection and Write toward it once per frame or more. raw is
// the connection without the session.
func BenchmarkFrame(b *testing.B) {
	for _, size := range []int{16, 32 << 10} {
		payload := make([]byte, size)
		toClient := frame(2, true, false, payload)
		fromClient := frame(2, true, true, payload)
		// A text event that is not output, read whole and parsed: what every
		// backend text frame costs until the first output.
		event := frame(1, true, false, fmt.Appendf(nil, `{"type":"session.updated","pad":"%s"}`, bytes.Repeat([]byte("a"), max(size-40, 0))))
		for _, c := range []struct {
			name string
			conn func() net.Conn
			call func(net.Conn, []byte)
			buf  []byte
		}{
			{"write", func() net.Conn { return sink{} }, func(c net.Conn, b []byte) { c.Write(b) }, toClient},
			{"write-text-before-output", func() net.Conn { return sink{} }, func(c net.Conn, b []byte) { c.Write(b) }, event},
			{"read", func() net.Conn { return source{frame: fromClient} }, func(c net.Conn, b []byte) { c.Read(b) }, make([]byte, len(fromClient))},
		} {
			for _, wrapped := range []bool{false, true} {
				name := fmt.Sprintf("%s/%dB/raw", c.name, size)
				conn := c.conn()
				if wrapped {
					name = fmt.Sprintf("%s/%dB/session", c.name, size)
					conn = New(time.Now(), time.Now).Attach(conn)
				}
				b.Run(name, func(b *testing.B) {
					b.SetBytes(int64(len(c.buf)))
					b.ReportAllocs()
					for b.Loop() {
						c.call(conn, c.buf)
					}
				})
			}
		}
	}
}
