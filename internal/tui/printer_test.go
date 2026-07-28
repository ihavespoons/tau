package tui

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder stands in for the Bubble Tea program, capturing what would be
// written to the scrollback.
type recorder struct {
	mu    sync.Mutex
	lines []string
	// delay simulates a slow terminal, so ordering is tested under the
	// interleaving that would actually expose a race.
	delay time.Duration
}

func (r *recorder) Println(args ...any) {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprint(args...))
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.lines...)
}

// The whole reason this type exists: transcript blocks must come out in the
// order they were produced. Bubble Tea's own print commands cannot promise
// that, so if this ever regresses a conversation renders scrambled.
func TestPrinterPreservesOrder(t *testing.T) {
	rec := &recorder{delay: time.Millisecond}
	p := newPrinter(rec)
	go p.run()
	defer p.stop()

	const n = 200
	for i := range n {
		p.push([]string{strconv.Itoa(i)})
	}

	deadline := time.After(5 * time.Second)
	for len(rec.snapshot()) < n {
		select {
		case <-deadline:
			t.Fatalf("only %d of %d blocks were written", len(rec.snapshot()), n)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	for i, got := range rec.snapshot() {
		if got != strconv.Itoa(i) {
			t.Fatalf("block %d came out as %q — the transcript is out of order", i, got)
		}
	}
}

// push runs on the render goroutine, which is also the goroutine draining the
// channel the printer writes into. If push could ever block, that would be a
// deadlock; it has to accept work faster than the sink can take it.
func TestPushNeverBlocks(t *testing.T) {
	rec := &recorder{delay: 20 * time.Millisecond}
	p := newPrinter(rec)
	go p.run()
	defer p.stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 500 {
			p.push([]string{strconv.Itoa(i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked behind a slow sink")
	}
}

func TestPrinterJoinsLinesIntoOneBlock(t *testing.T) {
	rec := &recorder{}
	p := newPrinter(rec)
	go p.run()
	defer p.stop()

	p.push([]string{"a", "b", "c"})

	deadline := time.After(2 * time.Second)
	for len(rec.snapshot()) == 0 {
		select {
		case <-deadline:
			t.Fatal("nothing was written")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := rec.snapshot()[0]; got != "a\nb\nc" {
		t.Errorf("lines should be written as one block, got %q", got)
	}
}

// After stop, writes are dropped rather than sent into a terminal the program
// has already stopped managing.
func TestPrinterDropsWritesAfterStop(t *testing.T) {
	rec := &recorder{}
	p := newPrinter(rec)
	go p.run()

	p.stop()
	p.push([]string{"late"})

	time.Sleep(50 * time.Millisecond)
	for _, line := range rec.snapshot() {
		if strings.Contains(line, "late") {
			t.Error("a write after stop reached the terminal")
		}
	}
}

func TestPrinterIgnoresEmptyPush(t *testing.T) {
	rec := &recorder{}
	p := newPrinter(rec)
	go p.run()
	defer p.stop()

	p.push(nil)
	p.push([]string{})

	time.Sleep(30 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("empty pushes should write nothing, got %v", got)
	}
}
