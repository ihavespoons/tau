package rpc

import (
	"encoding/json"
	"io"
	"sync"

	"github.com/ihavespoons/tau/extension/wire"
)

// Writer serializes responses and events onto a stream.
//
// It is safe for concurrent use, which is not optional here: the agent's event
// sink, an extension's dialog, and a command's response all write from
// different goroutines, and two interleaved records would corrupt both.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
	err error
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	enc := json.NewEncoder(w)
	// Go escapes <, >, and & by default, which no JSON reader requires and no
	// JavaScript client produces. Tool output full of HTML would otherwise
	// differ from what an equivalent Pi client emits, for no gain.
	enc.SetEscapeHTML(false)
	return &Writer{w: w, enc: enc}
}

// Err returns the first write failure.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *Writer) write(v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	if err := w.enc.Encode(v); err != nil {
		w.err = err
	}
}

// Emit writes a response.
func (w *Writer) Emit(res Response) { w.write(res) }

// EmitCommand writes a command, which is the direction a client sends. tau
// itself only ever writes the other three, but a supervisor driving tau needs
// the same framing and the same lock, and a second encoder would be a second
// place for the escaping rules to drift.
func (w *Writer) EmitCommand(cmd Command) { w.write(cmd) }

// EmitEvent writes an event.
func (w *Writer) EmitEvent(ev Event) { w.write(ev) }

// EmitUI writes an extension UI request.
func (w *Writer) EmitUI(req ExtensionUIRequest) { w.write(req) }

// Reader reads LF-framed JSON lines.
//
// It is the same framing the extension protocol uses, and for the same reason:
// U+2028 and U+2029 are legal inside a JSON string, a JavaScript client writes
// them raw, and a reader that splits on them corrupts the record. Sharing the
// implementation means there is one place that can get it wrong.
type Reader struct{ r *wire.Reader }

// NewReader wraps r.
func NewReader(r io.Reader) *Reader { return &Reader{r: wire.NewReader(r)} }

// ReadLine returns the next record's bytes, without the terminator.
func (r *Reader) ReadLine() ([]byte, error) {
	line, err := r.r.ReadLine()
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(line))
	copy(out, line)
	return out, nil
}
