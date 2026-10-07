// Package wstest is a WebSocket client and backend for tests, written frame
// by frame (RFC 6455) so a test sees exactly the bytes that crossed.
package wstest

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
	"net/url"
	"testing"
	"time"
)

// The opcodes a test sends.
const (
	Text   = 1
	Binary = 2
	Close  = 8
	Ping   = 9
	Pong   = 10
)

// Frame is one final frame, masked as a client's must be.
func Frame(op byte, payload []byte, masked bool) []byte {
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

// ReadFrame is the next frame, unmasked.
func ReadFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var e [2]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(e[:])
	}
	var mask [4]byte
	if h[1]&0x80 != 0 {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
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

// Accept answers an upgrade with 101 and hands over the connection.
func Accept(rw http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	conn, brw, err := http.NewResponseController(rw).Hijack()
	if err != nil {
		return nil, nil, err
	}
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, brw.Reader, nil
}

// Echo is a model's server that speaks WebSocket on every path: it sends
// every data frame back, answers a ping, and returns a close.
func Echo() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, br, err := Accept(rw, r)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			op, p, err := ReadFrame(br)
			if err != nil {
				return
			}
			switch op {
			case Close:
				conn.Write(Frame(Close, p, false))
				return
			case Ping:
				conn.Write(Frame(Pong, p, false))
			default:
				conn.Write(Frame(op, p, false))
			}
		}
	})
}

// Client is one WebSocket from a test.
type Client struct {
	T    testing.TB
	Conn net.Conn
	R    *bufio.Reader
}

// Open sends an upgrade for target, a path and query, to the server at base,
// with header added. The client is nil unless the answer is 101.
func Open(t testing.TB, base, target string, header http.Header) (*Client, *http.Response) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var req bytes.Buffer
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n", target, u.Host)
	header.Write(&req)
	req.WriteString("\r\n")
	if _, err := conn.Write(req.Bytes()); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("upgrade %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil, resp
	}
	return &Client{T: t, Conn: conn, R: r}, resp
}

// Send writes one frame.
func (c *Client) Send(op byte, payload []byte) {
	c.T.Helper()
	if _, err := c.Conn.Write(Frame(op, payload, true)); err != nil {
		c.T.Fatal(err)
	}
}

// Read is the next frame, within five seconds.
func (c *Client) Read() (byte, []byte) {
	c.T.Helper()
	c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	op, p, err := ReadFrame(c.R)
	if err != nil {
		c.T.Fatalf("no frame: %v", err)
	}
	return op, p
}

// RoundTrip sends one frame and reads the answer.
func (c *Client) RoundTrip(op byte, payload []byte) (byte, []byte) {
	c.T.Helper()
	c.Send(op, payload)
	return c.Read()
}

// Closed is the close frame that came next, after which the connection has
// to end.
func (c *Client) Closed() (code int, reason string) {
	c.T.Helper()
	c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	op, p, err := ReadFrame(c.R)
	if err != nil || op != Close || len(p) < 2 {
		c.T.Fatalf("no close frame: op %d %x %v", op, p, err)
	}
	if _, _, err := ReadFrame(c.R); err == nil {
		c.T.Fatal("the connection went on after the close frame")
	}
	return int(binary.BigEndian.Uint16(p)), string(p[2:])
}
