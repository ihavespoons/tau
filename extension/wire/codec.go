package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxFrameBytes caps one frame. A tool result carrying a large file is
// legitimate; an extension streaming without newlines until the host runs out
// of memory is not, and the two are indistinguishable without a limit.
const MaxFrameBytes = 32 << 20

// ErrFrameTooLarge is returned when a frame exceeds MaxFrameBytes.
var ErrFrameTooLarge = fmt.Errorf("wire: frame exceeds %d bytes", MaxFrameBytes)

// Writer serializes frames onto a stream. It is safe for concurrent use: the
// host writes from whichever goroutine is dispatching, and a partial line
// interleaved with another would corrupt both.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
	enc *json.Encoder
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	fw := &Writer{w: w}
	fw.enc = json.NewEncoder(&fw.buf)
	// Go escapes <, >, and & by default, which no JSON reader requires and
	// which a JavaScript peer using JSON.stringify never produces. A tool
	// argument full of HTML would then differ byte-for-byte depending on which
	// side wrote it, for no gain.
	fw.enc.SetEscapeHTML(false)
	return fw
}

// Write serializes v as one LF-terminated line.
//
// json.Encoder already appends the newline and escapes every control character
// inside strings, so the only way a raw LF reaches the stream is through a
// value that is not valid JSON — which the encoder rejects instead.
func (w *Writer) Write(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Reset()
	if err := w.enc.Encode(v); err != nil {
		return fmt.Errorf("wire: encode: %w", err)
	}
	if w.buf.Len() > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if _, err := w.w.Write(w.buf.Bytes()); err != nil {
		return fmt.Errorf("wire: write: %w", err)
	}
	return nil
}

// Reader reads LF-framed JSON lines.
//
// Framing is LF-only and deliberately hand-rolled. U+2028 and U+2029 are legal
// inside a JSON string and a JavaScript peer emits them unescaped; a reader
// that treats them as terminators — as Node's readline and several line
// scanners do — splits a frame in half and reports two parse errors instead of
// one message.
type Reader struct {
	br *bufio.Reader
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10)}
}

// ReadLine returns the next frame's bytes, without the terminator. Blank lines
// are skipped: a peer that flushes an extra newline should not look like a
// protocol violation. The returned slice is only valid until the next call.
func (r *Reader) ReadLine() ([]byte, error) {
	for {
		line, err := r.readOne()
		if err != nil {
			return nil, err
		}
		// A CRLF-terminated line comes from a peer on a platform that
		// translated the newline; the CR is framing, not payload.
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return line, nil
	}
}

func (r *Reader) readOne() ([]byte, error) {
	var acc []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			acc = append(acc, chunk...)
			if len(acc) > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			continue
		}
		if err != nil {
			// A final line without a terminator is still a frame: a peer that
			// exits immediately after writing may not get the newline out.
			if errors.Is(err, io.EOF) && (len(acc) > 0 || len(chunk) > 0) {
				return append(acc, chunk...), nil
			}
			return nil, err
		}
		acc = append(acc, chunk[:len(chunk)-1]...)
		if len(acc) > MaxFrameBytes {
			return nil, ErrFrameTooLarge
		}
		return acc, nil
	}
}

// Read decodes the next frame's envelope and returns the raw line alongside
// it, so the caller can decode again into the concrete type once it knows
// which one applies.
func (r *Reader) Read() (Envelope, []byte, error) {
	line, err := r.ReadLine()
	if err != nil {
		return Envelope{}, nil, err
	}
	// The line buffer is reused; the caller may hold the frame across a
	// dispatch, so it gets its own copy.
	raw := make([]byte, len(line))
	copy(raw, line)

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Envelope{}, raw, fmt.Errorf("wire: decode envelope: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, raw, errors.New("wire: frame has no type")
	}
	return env, raw, nil
}
