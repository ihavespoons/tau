package wire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
)

// Serve runs the extension side of the protocol.
//
// It is a reference implementation in two senses. Any program that can be
// executed is a valid tau extension if it behaves like this one, whatever
// language it is written in — so this is the specification, in a form that
// runs. And it is what tau's own conformance tests drive, so the behaviour
// described here is the behaviour that is checked.
//
// Serve returns when the host closes stdin, sends a shutdown frame, or ctx
// ends.
func Serve(ctx context.Context, in io.Reader, out io.Writer, h Handler) error {
	c := &Conn{
		w: NewWriter(out), r: NewReader(in), h: h,
		inflight: map[string]context.CancelFunc{},
		replies:  map[string]chan Reply{},
	}
	return c.run(ctx)
}

// ServeStdio runs Serve over the process's own stdin and stdout.
//
// It also redirects the standard log output to stderr, because anything
// written to stdout that is not a frame corrupts the stream. A stray
// fmt.Println in an extension is the single easiest way to break this
// protocol, and it fails in a way that looks like a host bug.
func ServeStdio(ctx context.Context, h Handler) error {
	return Serve(ctx, os.Stdin, os.Stdout, h)
}

// Handler is what an extension implements. Every field is optional: a nil
// hook means the extension does not offer that capability, and the host is
// told so at handshake.
type Handler struct {
	// Init declares the extension. It is called once, before anything else.
	Init func(Init) (InitResult, error)
	// Event handles one event. Returning a nil payload means "no opinion",
	// which most composition policies treat differently from an empty result.
	Event func(ctx context.Context, ev Event, c *Conn) (json.RawMessage, error)
	// Tool executes a registered tool. update streams partial results.
	Tool func(ctx context.Context, req ToolExecute, c *Conn, update func(ToolResultPayload)) (ToolResultPayload, error)
	// Command runs a registered slash command.
	Command func(ctx context.Context, req Command, c *Conn) error
	// Completions offers argument completions.
	Completions func(ctx context.Context, req Completions, c *Conn) ([]CompletionItem, error)
	// Shortcut fires a registered key binding.
	Shortcut func(ctx context.Context, req Shortcut, c *Conn) error
	// Render produces the lines for a registered renderer.
	Render func(ctx context.Context, req Render, c *Conn) ([]string, error)
	// Shutdown is called before Serve returns, so an extension can release
	// whatever it holds.
	Shutdown func(reason string)
}

// Conn is the extension's handle on the host: what it can ask for, and what it
// can ask the host to do.
type Conn struct {
	w *Writer
	r *Reader
	h Handler

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	replies  map[string]chan Reply

	nextID atomic.Uint64
	// init is the handshake the host sent, available to handlers.
	init Init
}

// Init returns the handshake the host opened with.
func (c *Conn) Init() Init { return c.init }

// Log writes a diagnostic line. It does not wait for the host.
func (c *Conn) Log(level, message string) {
	_ = c.w.Write(Log{Type: FrameLog, Level: level, Message: message})
}

func (c *Conn) newID() string {
	return "x" + strconv.FormatUint(c.nextID.Add(1), 10)
}

// ask sends an extension-originated request and waits for its reply.
func (c *Conn) ask(ctx context.Context, id string, frame any) (Reply, error) {
	ch := make(chan Reply, 1)
	c.mu.Lock()
	c.replies[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.replies, id)
		c.mu.Unlock()
	}()

	if err := c.w.Write(frame); err != nil {
		return Reply{}, err
	}
	select {
	case r := <-ch:
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
}

// UI sends a ui_request and waits for the user's answer. Cancelled reports a
// dialog the user dismissed, which is not an error.
func (c *Conn) UI(ctx context.Context, req UIRequest) (payload json.RawMessage, cancelled bool, err error) {
	req.Type, req.ID = FrameUIRequest, c.newID()
	r, err := c.ask(ctx, req.ID, req)
	if err != nil {
		return nil, false, err
	}
	return r.Payload, r.Cancelled, nil
}

// Action sends an action and waits for its result.
func (c *Conn) Action(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	act := Action{Type: FrameAction, ID: c.newID(), Method: method, Params: raw}
	r, err := c.ask(ctx, act.ID, act)
	if err != nil {
		return nil, err
	}
	return r.Payload, nil
}

func (c *Conn) run(ctx context.Context) error {
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	var wg sync.WaitGroup
	defer wg.Wait()

	reason := "eof"
	for {
		env, raw, err := c.r.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		switch env.Type {
		case FrameInit:
			var in Init
			if err := json.Unmarshal(raw, &in); err != nil {
				return err
			}
			c.init = in
			res := InitResult{Type: FrameInitResult, Protocol: Protocol, Name: in.Name}
			if c.h.Init != nil {
				got, err := c.h.Init(in)
				if err != nil {
					res.Error = err.Error()
				} else {
					res = got
				}
			}
			res.Type = FrameInitResult
			if res.Protocol == 0 {
				res.Protocol = Protocol
			}
			if err := c.w.Write(res); err != nil {
				return err
			}
			if res.Error != "" {
				return nil
			}

		case FrameShutdown:
			var sd Shutdown
			_ = json.Unmarshal(raw, &sd)
			reason = sd.Reason
			cancelAll()
			if c.h.Shutdown != nil {
				c.h.Shutdown(reason)
			}
			return nil

		case FrameCancel:
			var cn Cancel
			if err := json.Unmarshal(raw, &cn); err != nil {
				continue
			}
			c.mu.Lock()
			if stop := c.inflight[cn.ID]; stop != nil {
				stop()
			}
			c.mu.Unlock()

		case FrameReply:
			var rp Reply
			if err := json.Unmarshal(raw, &rp); err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.replies[rp.ID]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- rp:
				default:
				}
			}

		default:
			// Each request runs on its own goroutine. The host dispatches
			// events sequentially, but a tool execution and a dialog can be
			// outstanding at the same time, and a handler that blocked the
			// read loop would never see its own cancel frame.
			wg.Add(1)
			go func(env Envelope, raw []byte) {
				defer wg.Done()
				c.serve(ctx, env, raw)
			}(env, raw)
		}
	}

	if c.h.Shutdown != nil {
		c.h.Shutdown(reason)
	}
	return nil
}

// track scopes a cancellable context to one request id, so a cancel frame can
// reach the handler that is running it.
func (c *Conn) track(ctx context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.inflight[id] = cancel
	c.mu.Unlock()
	return ctx, func() {
		c.mu.Lock()
		delete(c.inflight, id)
		c.mu.Unlock()
		cancel()
	}
}

func (c *Conn) reply(id string, payload any, err error) {
	res := Result{Type: FrameResult, ID: id}
	if err != nil {
		res.Error = err.Error()
	} else if payload != nil {
		b, merr := json.Marshal(payload)
		if merr != nil {
			res.Error = merr.Error()
		} else {
			res.Payload = b
		}
	}
	_ = c.w.Write(res)
}

func (c *Conn) serve(ctx context.Context, env Envelope, raw []byte) {
	switch env.Type {
	case FrameEvent:
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			c.reply(env.ID, nil, err)
			return
		}
		// A hot event carries no reply. Answering one would put a frame on the
		// wire that no request is waiting for.
		if c.h.Event == nil {
			if !ev.NoReply {
				c.reply(ev.ID, nil, nil)
			}
			return
		}
		rctx, done := c.track(ctx, ev.ID)
		defer done()
		out, err := c.h.Event(rctx, ev, c)
		if ev.NoReply {
			return
		}
		if err != nil {
			c.reply(ev.ID, nil, err)
			return
		}
		res := Result{Type: FrameResult, ID: ev.ID, Payload: out}
		_ = c.w.Write(res)

	case FrameToolExecute:
		var req ToolExecute
		if err := json.Unmarshal(raw, &req); err != nil {
			c.reply(env.ID, nil, err)
			return
		}
		if c.h.Tool == nil {
			c.reply(req.ID, nil, fmt.Errorf("no tool handler for %q", req.Tool))
			return
		}
		rctx, done := c.track(ctx, req.ID)
		defer done()
		update := func(p ToolResultPayload) {
			b, err := json.Marshal(p)
			if err != nil {
				return
			}
			_ = c.w.Write(ToolUpdate{Type: FrameToolUpdate, ID: req.ID, Partial: b})
		}
		out, err := c.h.Tool(rctx, req, c, update)
		c.reply(req.ID, out, err)

	case FrameCommand:
		var req Command
		if err := json.Unmarshal(raw, &req); err != nil {
			c.reply(env.ID, nil, err)
			return
		}
		if c.h.Command == nil {
			c.reply(req.ID, nil, fmt.Errorf("no command handler for %q", req.Name))
			return
		}
		rctx, done := c.track(ctx, req.ID)
		defer done()
		c.reply(req.ID, nil, c.h.Command(rctx, req, c))

	case FrameCompletions:
		var req Completions
		if err := json.Unmarshal(raw, &req); err != nil {
			c.reply(env.ID, nil, err)
			return
		}
		if c.h.Completions == nil {
			c.reply(req.ID, CompletionsResult{}, nil)
			return
		}
		rctx, done := c.track(ctx, req.ID)
		defer done()
		items, err := c.h.Completions(rctx, req, c)
		c.reply(req.ID, CompletionsResult{Items: items}, err)

	case FrameShortcut:
		var req Shortcut
		if err := json.Unmarshal(raw, &req); err != nil {
			c.reply(env.ID, nil, err)
			return
		}
		if c.h.Shortcut == nil {
			c.reply(req.ID, nil, nil)
			return
		}
		rctx, done := c.track(ctx, req.ID)
		defer done()
		c.reply(req.ID, nil, c.h.Shortcut(rctx, req, c))

	case FrameRender:
		var req Render
		if err := json.Unmarshal(raw, &req); err != nil {
			c.reply(env.ID, nil, err)
			return
		}
		if c.h.Render == nil {
			c.reply(req.ID, RenderResult{}, nil)
			return
		}
		rctx, done := c.track(ctx, req.ID)
		defer done()
		lines, err := c.h.Render(rctx, req, c)
		c.reply(req.ID, RenderResult{Lines: lines}, err)

	default:
		c.reply(env.ID, nil, fmt.Errorf("unexpected frame %q from host", env.Type))
	}
}
