package tui

import (
	"strings"
	"sync"
)

// lineWriter is the sink a printer drains into. *tea.Program satisfies it;
// tests substitute a recorder.
type lineWriter interface {
	Println(args ...any)
}

// printer serializes writes into the terminal's scrollback.
//
// It exists because ordering matters and tea.Println cannot guarantee it:
// Bubble Tea runs every command on its own goroutine, so two print commands
// returned from consecutive Update calls can reach the event loop in either
// order — which would scramble a transcript. Program.Println, by contrast,
// posts straight to the message channel, so a single goroutine draining an
// ordered queue writes lines in exactly the order they were produced.
//
// The queue is unbounded on purpose. push() is called from the render
// goroutine, and that goroutine is the one draining the message channel the
// printer writes into: if push could block, a full queue would deadlock the
// program against itself.
type printer struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []string
	stopped bool
	out     lineWriter
}

func newPrinter(out lineWriter) *printer {
	p := &printer{out: out}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// push enqueues a block of lines. It never blocks.
func (p *printer) push(lines []string) {
	if len(lines) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.queue = append(p.queue, strings.Join(lines, "\n"))
	p.cond.Signal()
}

// run drains the queue until stopped. Call it in its own goroutine.
func (p *printer) run() {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.stopped {
			p.cond.Wait()
		}
		if p.stopped && len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		block := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()

		p.out.Println(block)
	}
}

// stop ends the drain loop. Blocks already queued are dropped rather than
// written into a terminal the program has stopped managing.
func (p *printer) stop() {
	p.mu.Lock()
	p.stopped = true
	p.queue = nil
	p.mu.Unlock()
	p.cond.Broadcast()
}
