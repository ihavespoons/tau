package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestWriteThenReadRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	in := Event{Type: FrameEvent, ID: "1", Event: "tool_call", Generation: 3}
	if err := w.Write(in); err != nil {
		t.Fatalf("write: %v", err)
	}

	env, raw, err := NewReader(&buf).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != FrameEvent || env.ID != "1" {
		t.Fatalf("envelope = %+v", env)
	}
	var out Event
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Type != in.Type || out.ID != in.ID || out.Event != in.Event || out.Generation != in.Generation {
		t.Fatalf("round trip: got %+v want %+v", out, in)
	}
}

// A JSON string may contain U+2028 and U+2029. A JavaScript peer emits them
// raw, and a reader that splits on them tears the frame in two.
func TestReadDoesNotSplitOnUnicodeSeparators(t *testing.T) {
	payload := "line sep para"
	// Hand-built the way a JS peer would write it: JSON.stringify leaves both
	// separators unescaped, so the bytes really are on the wire.
	line := `{"type":"log","message":"` + payload + `"}` + "\n"
	if !strings.Contains(line, " ") {
		t.Fatal("fixture lost its separator")
	}

	env, raw, err := NewReader(strings.NewReader(line)).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != FrameLog {
		t.Fatalf("type = %q", env.Type)
	}
	var out Log
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Message != payload {
		t.Fatalf("message = %q want %q", out.Message, payload)
	}
}

// Go's encoder escapes both separators, so tau's own output survives a peer
// whose reader is less careful than this one.
func TestWriteEscapesUnicodeSeparators(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).Write(Log{Type: FrameLog, Message: "a\u2028b\u2029c"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if bytes.ContainsRune(buf.Bytes(), '\u2028') || bytes.ContainsRune(buf.Bytes(), '\u2029') {
		t.Fatalf("raw separator on the wire: %q", buf.String())
	}
	for _, want := range []string{`\u2028`, `\u2029`} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Fatalf("separator not escaped as %s: %q", want, buf.String())
		}
	}
}

func TestWriteDoesNotEscapeHTML(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).Write(Log{Type: FrameLog, Message: "<a> & </a>"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if bytes.Contains(buf.Bytes(), []byte(escaped)) {
			t.Fatalf("HTML escaped as %s, would differ from a JS peer: %q", escaped, buf.String())
		}
	}
	if !bytes.Contains(buf.Bytes(), []byte("<a> & </a>")) {
		t.Fatalf("payload did not survive verbatim: %q", buf.String())
	}
}

func TestWriteAlwaysEndsWithExactlyOneNewline(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, msg := range []string{"a", "embedded\nnewline", "tab\there"} {
		if err := w.Write(Log{Type: FrameLog, Message: msg}); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
	}
	// Three frames written; an embedded newline that reached the stream raw
	// would make four lines out of three.
	if got := bytes.Count(buf.Bytes(), []byte("\n")); got != 3 {
		t.Fatalf("newline count = %d, want 3: %q", got, buf.String())
	}
}

func TestReaderSkipsBlankLines(t *testing.T) {
	in := "\n\n" + `{"type":"log","message":"x"}` + "\n\n"
	r := NewReader(strings.NewReader(in))
	env, _, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != FrameLog {
		t.Fatalf("type = %q", env.Type)
	}
	if _, _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("second read = %v, want EOF", err)
	}
}

func TestReaderAcceptsCRLF(t *testing.T) {
	env, raw, err := NewReader(strings.NewReader(`{"type":"log","message":"x"}` + "\r\n")).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != FrameLog {
		t.Fatalf("type = %q", env.Type)
	}
	var out Log
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("a trailing CR reached the JSON decoder: %v", err)
	}
}

// A peer that exits the instant it finishes writing may never flush the
// terminator. Dropping that frame loses the answer the host is waiting for.
func TestReaderReturnsUnterminatedFinalLine(t *testing.T) {
	env, _, err := NewReader(strings.NewReader(`{"type":"log","message":"x"}`)).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Type != FrameLog {
		t.Fatalf("type = %q", env.Type)
	}
}

func TestReaderHandlesLinesLargerThanTheBuffer(t *testing.T) {
	big := strings.Repeat("x", 512<<10)
	var buf bytes.Buffer
	if err := NewWriter(&buf).Write(Log{Type: FrameLog, Message: big}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, raw, err := NewReader(&buf).Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out Log
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Message) != len(big) {
		t.Fatalf("message len = %d want %d", len(out.Message), len(big))
	}
}

func TestReaderRejectsOversizedFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"log","message":"`)
	buf.Write(bytes.Repeat([]byte("x"), MaxFrameBytes+16))
	buf.WriteString(`"}` + "\n")

	if _, _, err := NewReader(&buf).Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadRejectsFrameWithoutType(t *testing.T) {
	if _, _, err := NewReader(strings.NewReader(`{"id":"1"}` + "\n")).Read(); err == nil {
		t.Fatal("a frame with no type was accepted")
	}
}

func TestReadRejectsMalformedJSON(t *testing.T) {
	if _, _, err := NewReader(strings.NewReader("not json\n")).Read(); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}

// The host writes from whichever goroutine is dispatching. Two frames
// interleaved mid-line would corrupt both.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = w.Write(Log{Type: FrameLog, Message: strings.Repeat("abcdefgh", 512)})
		}(i)
	}
	wg.Wait()

	r := NewReader(&buf)
	for i := 0; i < 32; i++ {
		if _, _, err := r.Read(); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
}

// Held frames must survive the reader moving on, or a dispatch that outlives
// one read sees another frame's bytes.
func TestReadCopiesTheFrame(t *testing.T) {
	in := `{"type":"log","message":"first"}` + "\n" + `{"type":"log","message":"second"}` + "\n"
	r := NewReader(strings.NewReader(in))

	_, first, err := r.Read()
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	if _, _, err := r.Read(); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if !bytes.Contains(first, []byte("first")) {
		t.Fatalf("first frame was overwritten: %q", first)
	}
}
