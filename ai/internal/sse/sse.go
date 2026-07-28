// Package sse decodes Server-Sent Event streams. Port of the hand-rolled
// decoder in Pi's anthropic-messages.ts (iterateSseMessages): handles \r, \n,
// and \r\n line breaks, comment lines, and multi-line data fields.
package sse

import (
	"bufio"
	"io"
	"strings"
)

// Event is one decoded server-sent event.
type Event struct {
	// Name is the value of the "event:" field, empty if none was present.
	Name string
	// Data is the joined "data:" field values (newline-separated).
	Data string
	// Raw preserves the original field lines for diagnostics.
	Raw []string
}

// Reader incrementally decodes SSE events from a byte stream.
type Reader struct {
	scanner *bufio.Scanner
	// pending decoder state
	event string
	data  []string
	raw   []string
	// trailing flush emitted?
	done bool
}

// NewReader wraps r. Lines may be arbitrarily long (up to 10 MiB).
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	sc.Split(scanLines)
	return &Reader{scanner: sc}
}

// scanLines splits on \n, \r, or \r\n (SSE spec), unlike bufio.ScanLines
// which only handles \n and \r\n.
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			// Might be \r\n split across reads; ask for more data.
			return 0, nil, nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// Next returns the next event, or (nil, io.EOF) at end of stream. Any read
// error from the underlying stream is returned as-is.
func (r *Reader) Next() (*Event, error) {
	if r.done {
		return nil, io.EOF
	}
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if ev := r.decodeLine(line); ev != nil {
			return ev, nil
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	// Trailing flush of an unterminated final event.
	r.done = true
	if ev := r.flush(); ev != nil {
		return ev, nil
	}
	return nil, io.EOF
}

func (r *Reader) decodeLine(line string) *Event {
	if line == "" {
		return r.flush()
	}
	r.raw = append(r.raw, line)
	if strings.HasPrefix(line, ":") {
		return nil
	}
	field, value := line, ""
	if idx := strings.Index(line, ":"); idx != -1 {
		field = line[:idx]
		value = line[idx+1:]
	}
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		r.event = value
	case "data":
		r.data = append(r.data, value)
	}
	return nil
}

func (r *Reader) flush() *Event {
	if r.event == "" && len(r.data) == 0 {
		return nil
	}
	ev := &Event{Name: r.event, Data: strings.Join(r.data, "\n"), Raw: r.raw}
	r.event = ""
	r.data = nil
	r.raw = nil
	return ev
}
