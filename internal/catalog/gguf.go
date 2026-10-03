package catalog

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// ggufMeta is what a model's settings are derived from in a GGUF's metadata:
// its chat template and its trained context length. The reader stops at the
// end of the key-value section and never touches the tensors.
type ggufMeta struct {
	chatTemplate  string
	contextLength int
}

// GGUF value types, from ggml's gguf.h.
const (
	ggufUint8 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

func readGGUF(path string) (ggufMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return ggufMeta{}, err
	}
	defer f.Close()
	r := &ggufReader{r: bufio.NewReaderSize(f, 1<<16)}
	var magic [4]byte
	r.read(magic[:])
	version := r.u32()
	r.u64() // tensors
	count := r.u64()
	if r.err != nil {
		return ggufMeta{}, fmt.Errorf("%s: %w", path, r.err)
	}
	if string(magic[:]) != "GGUF" || version < 2 {
		return ggufMeta{}, fmt.Errorf("%s: not a GGUF of version 2 or later", path)
	}
	var meta ggufMeta
	arch := ""
	lengths := map[string]int{}
	for i := uint64(0); i < count && r.err == nil; i++ {
		key := r.str()
		typ := r.u32()
		switch {
		case key == "tokenizer.chat_template" && typ == ggufString:
			meta.chatTemplate = r.str()
		case key == "general.architecture" && typ == ggufString:
			arch = r.str()
		case typ == ggufUint32 || typ == ggufUint64:
			n := r.uint(typ)
			lengths[key] = int(n)
		default:
			r.skip(typ)
		}
	}
	if r.err != nil {
		return ggufMeta{}, fmt.Errorf("%s: %w", path, r.err)
	}
	meta.contextLength = lengths[arch+".context_length"]
	return meta, nil
}

// ggufReader keeps the first error, so a parse reads straight through and
// checks once.
type ggufReader struct {
	r   *bufio.Reader
	err error
}

func (g *ggufReader) read(b []byte) {
	if g.err == nil {
		_, g.err = io.ReadFull(g.r, b)
	}
}

func (g *ggufReader) u32() uint32 {
	var b [4]byte
	g.read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

func (g *ggufReader) u64() uint64 {
	var b [8]byte
	g.read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

func (g *ggufReader) uint(typ uint32) uint64 {
	if typ == ggufUint32 {
		return uint64(g.u32())
	}
	return g.u64()
}

// A string longer than this is a corrupt file, not a chat template.
const maxGGUFString = 16 << 20

func (g *ggufReader) str() string {
	n := g.u64()
	if g.err != nil {
		return ""
	}
	if n > maxGGUFString {
		g.err = errors.New("a metadata string longer than 16 MiB")
		return ""
	}
	b := make([]byte, n)
	g.read(b)
	return string(b)
}

func (g *ggufReader) discard(n uint64) {
	if g.err == nil {
		_, g.err = g.r.Discard(int(n))
	}
}

// skip reads past one value of type typ.
func (g *ggufReader) skip(typ uint32) {
	switch typ {
	case ggufUint8, ggufInt8, ggufBool:
		g.discard(1)
	case ggufUint16, ggufInt16:
		g.discard(2)
	case ggufUint32, ggufInt32, ggufFloat32:
		g.discard(4)
	case ggufUint64, ggufInt64, ggufFloat64:
		g.discard(8)
	case ggufString:
		n := g.u64()
		g.discard(n)
	case ggufArray:
		elem := g.u32()
		n := g.u64()
		if elem == ggufArray {
			g.err = errors.New("an array of arrays")
			return
		}
		for i := uint64(0); i < n && g.err == nil; i++ {
			g.skip(elem)
		}
	default:
		g.err = fmt.Errorf("unknown metadata type %d", typ)
	}
}
