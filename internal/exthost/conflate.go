package exthost

import (
	"sync"

	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
)

// hotEvents fire per streaming delta. Awaiting a subprocess round trip on each
// one would make every token cost a context switch through another process,
// and the handler's answer cannot change anything: these events are
// notification-only in every composition policy.
var hotEvents = map[extension.EventType]bool{
	extension.EventMessageUpdate:       true,
	extension.EventToolExecutionUpdate: true,
}

// conflator delivers hot events without blocking the agent.
//
// It holds exactly one pending payload per event type. A newer payload
// replaces an unsent older one, because for a streaming update the newest is
// strictly more informative than the one it replaces — a message_update
// carries the whole message so far, not a delta the receiver must accumulate.
//
// Dropping intermediate frames is the point, not a compromise: an extension
// watching the stream wants to know the current state, and a queue that grew
// without bound would deliver a history of it long after the turn ended.
type conflator struct {
	h *Host

	mu      sync.Mutex
	pending map[string]wire.Event
	order   []string
	wake    chan struct{}
	stopped bool
	// dropped counts payloads replaced before they were sent, for tests and
	// diagnostics.
	dropped int
}

func newConflator(h *Host) *conflator {
	c := &conflator{h: h, pending: map[string]wire.Event{}, wake: make(chan struct{}, 1)}
	go c.run()
	return c
}

// send queues an event, replacing any unsent one of the same type.
func (c *conflator) send(ev wire.Event) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	if _, exists := c.pending[ev.Event]; exists {
		c.dropped++
	} else {
		c.order = append(c.order, ev.Event)
	}
	c.pending[ev.Event] = ev
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *conflator) run() {
	for {
		select {
		case <-c.wake:
		case <-c.h.closed:
			return
		}
		for {
			ev, ok := c.take()
			if !ok {
				break
			}
			if c.h.suspend.Load() {
				continue
			}
			// No result is awaited, so a slow extension backs up here rather
			// than in the agent loop: the next payload simply replaces this
			// one once it lands.
			if err := c.h.w.Write(ev); err != nil {
				return
			}
		}
	}
}

func (c *conflator) take() (wire.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.order) > 0 {
		name := c.order[0]
		c.order = c.order[1:]
		ev, ok := c.pending[name]
		if !ok {
			continue
		}
		delete(c.pending, name)
		return ev, true
	}
	return wire.Event{}, false
}

func (c *conflator) stop() {
	c.mu.Lock()
	c.stopped = true
	c.pending = map[string]wire.Event{}
	c.order = nil
	c.mu.Unlock()
}

// Dropped reports how many hot payloads were superseded before being sent.
func (c *conflator) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}
