// Package acp speaks the Agent Client Protocol, so an editor that drives ACP
// agents can drive tau.
//
// It is a satellite: the binary does not import it unless asked. The protocol
// is JSON-RPC 2.0 over stdio, newline-delimited, with the agent forbidden from
// writing anything to stdout that is not an ACP message — stderr is where logs
// go. Schema version 1 (schema-v1.20.0).
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ihavespoons/tau/extension/wire"
)

// JSON-RPC 2.0 error codes, plus the range reserved for application errors.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Message is one JSON-RPC frame. Request, response and notification share a
// shape on the wire, and which one it is depends on which fields are present:
// a method with an id is a request, a method without one is a notification,
// and an id with a result or an error is a response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("acp error %d: %s", e.Code, e.Message) }

// Handler answers an inbound request. Returning an error becomes a JSON-RPC
// error response; returning a nil result for a notification is normal.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Conn is a JSON-RPC connection over a pair of streams.
//
// Both directions are live at once: the client sends requests while the agent
// sends session/update notifications back, and an outbound request from the
// agent — a permission prompt, a file read — is waiting for its own response
// while inbound traffic keeps arriving.
type Conn struct {
	reader  io.Reader
	handler Handler

	writeMu sync.Mutex
	enc     *json.Encoder

	// inflight counts handlers still running, so the read loop ending does not
	// abandon a response that was about to be written.
	inflight sync.WaitGroup

	mu      sync.Mutex
	pending map[string]chan *Message
	nextID  int
	closed  bool
	done    chan struct{}
}

// NewConn wires a connection. Nothing is read until Serve runs.
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	enc := json.NewEncoder(w)
	// The transport forbids embedded newlines in a message, and Go's encoder
	// already emits exactly one line per value. Escaping is off for the same
	// reason it is off elsewhere in tau: no JSON reader requires it, and it
	// would make tool output full of HTML differ for no gain.
	enc.SetEscapeHTML(false)

	c := &Conn{
		reader:  r,
		handler: h,
		enc:     enc,
		pending: map[string]chan *Message{},
		done:    make(chan struct{}),
	}
	return c
}

// Serve reads until the stream ends, dispatching as it goes.
//
// Inbound requests are handled on their own goroutine so a long prompt turn
// does not stop the connection from noticing a session/cancel that arrives
// while it runs — which is the one notification that has to get through.
func (c *Conn) Serve(ctx context.Context) error {
	rd := wire.NewReader(c.reader)
	for {
		line, err := rd.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			// Shut down first so a handler parked on an outbound Call is
			// released — its answer can never arrive now — then wait for the
			// handlers themselves. Returning without waiting would drop the
			// response to a request the client sent just before closing.
			c.shutdown()
			c.inflight.Wait()
			return err
		}
		if len(line) == 0 {
			continue
		}

		var msg Message
		if uerr := json.Unmarshal(line, &msg); uerr != nil {
			c.writeError(nil, CodeParseError, uerr.Error())
			continue
		}

		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			c.inflight.Add(1)
			go func() {
				defer c.inflight.Done()
				c.serveRequest(ctx, msg)
			}()
		case msg.Method != "":
			c.inflight.Add(1)
			go func() {
				defer c.inflight.Done()
				c.serveNotification(ctx, msg)
			}()
		case len(msg.ID) > 0:
			c.deliver(&msg)
		}
	}
}

func (c *Conn) serveRequest(ctx context.Context, msg Message) {
	result, err := c.handler(ctx, msg.Method, msg.Params)
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			c.writeError(msg.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		c.writeError(msg.ID, CodeInternalError, err.Error())
		return
	}
	if result == nil {
		// A request must be answered even when there is nothing to say, or the
		// caller waits forever.
		result = struct{}{}
	}
	encoded, merr := json.Marshal(result)
	if merr != nil {
		c.writeError(msg.ID, CodeInternalError, merr.Error())
		return
	}
	_ = c.write(Message{JSONRPC: "2.0", ID: msg.ID, Result: encoded})
}

func (c *Conn) serveNotification(ctx context.Context, msg Message) {
	// A notification has nobody to report to, so a failure is dropped rather
	// than turned into a response the sender is not waiting for.
	_, _ = c.handler(ctx, msg.Method, msg.Params)
}

// Notify sends a one-way message.
func (c *Conn) Notify(method string, params any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(Message{JSONRPC: "2.0", Method: method, Params: encoded})
}

// Call sends a request and waits for its response.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return io.ErrClosedPipe
	}
	c.nextID++
	id := fmt.Sprintf("tau-%d", c.nextID)
	ch := make(chan *Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	rawID, _ := json.Marshal(id)
	if err := c.write(Message{JSONRPC: "2.0", ID: rawID, Method: method, Params: encoded}); err != nil {
		return err
	}

	select {
	case reply := <-ch:
		if reply.Error != nil {
			return reply.Error
		}
		if result != nil && len(reply.Result) > 0 {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return io.ErrClosedPipe
	}
}

// deliver routes a response to whoever is waiting for it.
func (c *Conn) deliver(msg *Message) {
	var id string
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		// An id that is a number rather than a string is legal JSON-RPC; tau
		// only ever sends string ids, so a numeric one answers nothing here.
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ok {
		ch <- msg
	}
}

func (c *Conn) write(msg Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.enc.Encode(msg)
}

func (c *Conn) writeError(id json.RawMessage, code int, message string) {
	_ = c.write(Message{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}})
}

// shutdown releases everyone waiting on a response that can no longer come.
func (c *Conn) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[string]chan *Message{}
	c.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	close(c.done)
}
