package stream

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
)

// side follows one direction of the stream frame by frame (RFC 6455 5.2).
// It reads headers, skips payloads by their length, and copies a payload
// only for a close frame's code and for the backend's text messages until
// the first output.
type side struct {
	fromClient bool

	head [14]byte
	have int    // header bytes held
	left uint64 // payload bytes still to come in the current frame
	at   uint64 // payload bytes of the current frame seen so far
	op   byte
	fin  bool
	mask [4]byte
	// masked is set for a frame whose payload is masked: every client
	// frame, and none of the backend's.
	masked bool
	watch  bool // the current frame's payload is copied below

	closeHead []byte // a close frame's first two payload bytes
	// peeking is a text message of the backend's being read for the first
	// output. It spans continuation frames, and a control frame between them.
	peeking bool
	peek    []byte
}

// between is true when the next byte would start a frame.
func (d *side) between() bool { return d.have == 0 && d.left == 0 }

// headerLen is the length of the header whose first bytes are h: 2, then
// 2 or 8 for an extended length, then 4 for a mask.
func headerLen(h []byte) int {
	if len(h) < 2 {
		return 2
	}
	n := 2
	switch h[1] & 0x7f {
	case 126:
		n += 2
	case 127:
		n += 8
	}
	if h[1]&0x80 != 0 {
		n += 4
	}
	return n
}

// scan follows b, the next bytes in d's direction, and says whether any of
// them belong to a data frame: text, binary or continuation.
func (s *Session) scan(d *side, b []byte) (data bool) {
	for len(b) > 0 {
		if d.left > 0 {
			n := d.left
			if uint64(len(b)) < n {
				n = uint64(len(b))
			}
			if d.op < 8 {
				data = true
			}
			if d.watch {
				s.payload(d, b[:n])
			}
			d.left -= n
			d.at += n
			b = b[n:]
			if d.left == 0 {
				s.frameEnd(d)
			}
			continue
		}
		d.head[d.have] = b[0]
		d.have++
		b = b[1:]
		need := headerLen(d.head[:d.have])
		if d.have < need {
			continue
		}
		h := d.head[:need]
		d.have, d.at = 0, 0
		d.fin, d.op = h[0]&0x80 != 0, h[0]&0x0f
		d.masked = h[1]&0x80 != 0
		rest := h[2:]
		switch l := h[1] & 0x7f; l {
		case 126:
			d.left = uint64(binary.BigEndian.Uint16(rest))
			rest = rest[2:]
		case 127:
			d.left = binary.BigEndian.Uint64(rest)
			rest = rest[8:]
		default:
			d.left = uint64(l)
		}
		if d.masked {
			copy(d.mask[:], rest)
		}
		if d.op < 8 {
			data = true
		}
		s.frameStart(d)
		if d.left == 0 {
			s.frameEnd(d)
		}
	}
	return data
}

func (s *Session) frameStart(d *side) {
	switch {
	case d.op == 8:
		d.watch, d.closeHead = true, d.closeHead[:0]
	case d.op > 8:
		d.watch = false
	case d.fromClient:
		d.watch = false
		if s.firstIn.Load() == 0 {
			s.firstIn.CompareAndSwap(0, s.offset())
		}
	case s.firstOut.Load() != 0:
		d.watch, d.peeking = false, false
	case d.op == 2:
		// The backend's binary frames are audio, or a format this does not
		// read: the first is the first output.
		s.output()
		d.watch = false
	case d.op == 1:
		d.peeking, d.peek = true, d.peek[:0]
		d.watch = true
	default: // a continuation
		d.watch = d.peeking
	}
}

func (s *Session) payload(d *side, p []byte) {
	if d.op == 8 {
		for i := 0; i < len(p) && len(d.closeHead) < 2; i++ {
			d.closeHead = append(d.closeHead, d.unmask(p[i], d.at+uint64(i)))
		}
		return
	}
	if len(d.peek)+len(p) > peekLimit {
		d.peeking, d.watch, d.peek = false, false, nil
		return
	}
	start := len(d.peek)
	d.peek = append(d.peek, p...)
	if d.masked {
		for i := range p {
			d.peek[start+i] = d.unmask(p[i], d.at+uint64(i))
		}
	}
	// A message whose start has no mark is passed on unread from there.
	if start < markWindow && len(d.peek) >= markWindow && !marked(d.peek) {
		d.peeking, d.watch, d.peek = false, false, d.peek[:0]
	}
}

func (d *side) unmask(b byte, at uint64) byte {
	if !d.masked {
		return b
	}
	return b ^ d.mask[at%4]
}

func (s *Session) frameEnd(d *side) {
	if d.op == 8 {
		code := 1005 // no status code
		if len(d.closeHead) == 2 {
			code = int(binary.BigEndian.Uint16(d.closeHead))
		}
		by := "backend"
		if d.fromClient {
			by = "client"
		}
		s.closing.CompareAndSwap(nil, &closeSeen{code: code, by: by})
		return
	}
	if d.op < 8 && d.fin && d.peeking {
		d.peeking = false
		if carriesOutput(d.peek) {
			s.output()
			d.peek = nil
		} else {
			d.peek = d.peek[:0]
		}
	}
}

func (s *Session) output() {
	if s.firstOut.Load() == 0 {
		s.firstOut.CompareAndSwap(0, s.offset())
	}
}

// outputMarks are the bytes a message that carries output has, one of them
// at least, within its first markWindow bytes: OpenAI's events start with
// their type, Deepgram's transcript comes before its words, WhisperLiveKit's
// lines near the start. A message with none is not parsed, so a backend's
// large text frame costs a search of 1 KB, not of the frame.
var outputMarks = [][]byte{[]byte(`.delta"`), []byte(`input_audio_transcription.completed"`), []byte(`"transcript"`), []byte(`"lines"`), []byte(`"buffer_transcription"`)}

const markWindow = 1 << 10

func marked(msg []byte) bool {
	head := msg[:min(len(msg), markWindow)]
	for _, m := range outputMarks {
		if bytes.Contains(head, m) {
			return true
		}
	}
	return false
}

// carriesOutput is whether one of the backend's JSON messages carries
// output (0018, 8): an OpenAI realtime delta, of a transcript, text or
// audio, or a finished input transcription; a Deepgram Results with a
// transcript; a WhisperLiveKit update with text.
func carriesOutput(msg []byte) bool {
	if !marked(msg) {
		return false
	}
	var e struct {
		Type    string `json:"type"`
		Channel *struct {
			Alternatives []struct {
				Transcript string `json:"transcript"`
			} `json:"alternatives"`
		} `json:"channel"`
		Lines []struct {
			Text string `json:"text"`
		} `json:"lines"`
		Buffer string `json:"buffer_transcription"`
	}
	if json.Unmarshal(msg, &e) != nil {
		return false
	}
	if strings.HasSuffix(e.Type, ".delta") || strings.HasSuffix(e.Type, "input_audio_transcription.completed") {
		return true
	}
	if e.Channel != nil {
		for _, a := range e.Channel.Alternatives {
			if a.Transcript != "" {
				return true
			}
		}
	}
	for _, l := range e.Lines {
		if l.Text != "" {
			return true
		}
	}
	return e.Buffer != ""
}
