// Package server supervises tau processes running in RPC mode.
//
// It is a satellite: the binary does not import it, and nothing here is linked
// into tau unless a program asks for it. What it buys is a long-lived process
// that owns several agents at once — one per working directory — which is the
// shape an editor plugin or a hosted deployment wants and which `tau --mode
// rpc` alone does not provide, because that is one agent on one pair of pipes.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/ihavespoons/tau/rpc"
)

// ErrInstanceGone is returned when a command is sent to an instance whose
// process has exited.
var ErrInstanceGone = errors.New("instance is no longer running")

// Instance is one supervised tau process and the conversation on its pipes.
//
// The process speaks LF-framed JSON in both directions. Three things come back
// on stdout and they are told apart by shape rather than by order: a response
// carries the id of the command it answers, an extension UI request carries its
// own id and wants an answer, and everything else is an event nobody asked for.
type Instance struct {
	cmd    *exec.Cmd
	stdin  *rpc.Writer
	closer io.Closer

	mu sync.Mutex
	// pending maps a command id to the channel waiting for its response.
	pending map[string]chan rpc.Response
	// subs receive every line the process writes, for event streaming.
	subs map[int]chan []byte
	next int
	// done is closed once the reader has stopped, which is the one signal that
	// says no further response will ever arrive.
	done chan struct{}
	// exitErr is why the process stopped, readable after done is closed.
	exitErr error
}

// Start launches a process and begins reading from it.
//
// The command is supplied rather than built here so a test can run a stand-in
// that speaks the protocol without a model, an API key, or a built binary.
func Start(cmd *exec.Cmd) (*Instance, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	in := &Instance{
		cmd:     cmd,
		stdin:   rpc.NewWriter(stdin),
		closer:  stdin,
		pending: map[string]chan rpc.Response{},
		subs:    map[int]chan []byte{},
		done:    make(chan struct{}),
	}
	go in.read(stdout)
	return in, nil
}

// read consumes stdout until it ends, then releases everyone waiting.
//
// It goes through rpc.Reader rather than a bufio.Scanner so the framing is the
// one tau writes with, including the line-length behaviour a large tool result
// depends on.
func (in *Instance) read(stdout io.Reader) {
	reader := rpc.NewReader(stdout)
	var err error
	for {
		line, rerr := reader.ReadLine()
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				err = rerr
			}
			break
		}
		if len(line) == 0 {
			continue
		}
		in.dispatch(line)
	}

	waitErr := in.cmd.Wait()
	if err == nil {
		err = waitErr
	}
	in.shutdown(err)
}

// dispatch routes one line: a response wakes its caller, everything else is
// broadcast.
func (in *Instance) dispatch(line []byte) {
	var probe struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	// A line that is not JSON at all is still forwarded: it is almost always a
	// crash message, and swallowing it would leave a caller waiting on a
	// process that has already said what went wrong.
	if err := json.Unmarshal(line, &probe); err == nil && probe.Type == "response" && probe.ID != "" {
		var res rpc.Response
		if err := json.Unmarshal(line, &res); err == nil {
			in.mu.Lock()
			ch, ok := in.pending[probe.ID]
			delete(in.pending, probe.ID)
			in.mu.Unlock()
			if ok {
				ch <- res
				close(ch)
				return
			}
		}
	}
	in.broadcast(line)
}

func (in *Instance) broadcast(line []byte) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, ch := range in.subs {
		// A subscriber that has stopped reading must not stall the process it
		// is watching, so a full buffer drops the line rather than blocking.
		select {
		case ch <- line:
		default:
		}
	}
}

// shutdown releases every waiter once the process is gone.
func (in *Instance) shutdown(err error) {
	in.mu.Lock()
	in.exitErr = err
	pending := in.pending
	in.pending = map[string]chan rpc.Response{}
	subs := in.subs
	in.subs = map[int]chan []byte{}
	in.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	for _, ch := range subs {
		close(ch)
	}
	close(in.done)
}

// Do sends a command and waits for the response that carries its id.
//
// A command with no id is fire-and-forget: there is nothing to correlate, so
// waiting for a reply that will never come would hang.
func (in *Instance) Do(ctx context.Context, cmd rpc.Command) (rpc.Response, error) {
	select {
	case <-in.done:
		return rpc.Response{}, ErrInstanceGone
	default:
	}

	if cmd.ID == "" {
		return rpc.Response{}, in.send(cmd)
	}

	ch := make(chan rpc.Response, 1)
	in.mu.Lock()
	if _, taken := in.pending[cmd.ID]; taken {
		in.mu.Unlock()
		return rpc.Response{}, fmt.Errorf("command id %q is already in flight", cmd.ID)
	}
	in.pending[cmd.ID] = ch
	in.mu.Unlock()

	if err := in.send(cmd); err != nil {
		in.mu.Lock()
		delete(in.pending, cmd.ID)
		in.mu.Unlock()
		return rpc.Response{}, err
	}

	select {
	case res, ok := <-ch:
		if !ok {
			return rpc.Response{}, in.exitReason()
		}
		return res, nil
	case <-ctx.Done():
		in.mu.Lock()
		delete(in.pending, cmd.ID)
		in.mu.Unlock()
		return rpc.Response{}, ctx.Err()
	case <-in.done:
		return rpc.Response{}, in.exitReason()
	}
}

// Send writes a command without waiting for anything.
func (in *Instance) Send(cmd rpc.Command) error { return in.send(cmd) }

// send writes to the process without taking the instance lock.
//
// That is deliberate and load-bearing. rpc.Writer serializes its own writes, so
// the lock would buy nothing — and holding it across a pipe write deadlocks the
// moment the process stops draining its stdin: the write blocks holding the
// lock, and the reader goroutine needs that same lock to deliver the response
// that would let the process move on.
func (in *Instance) send(cmd rpc.Command) error {
	in.stdin.EmitCommand(cmd)
	if err := in.stdin.Err(); err != nil {
		// A failed write to the pipe has exactly one meaning: the process is
		// unreachable, because it exited or because Close already took its
		// stdin away. Reporting the raw "file already closed" would make a
		// caller handle a plumbing detail to answer a question Done answers.
		return fmt.Errorf("%w: %v", ErrInstanceGone, err)
	}
	return nil
}

func (in *Instance) exitReason() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.exitErr != nil {
		return fmt.Errorf("%w: %v", ErrInstanceGone, in.exitErr)
	}
	return ErrInstanceGone
}

// Subscribe returns a channel of every line the process writes that was not a
// correlated response, and a function to stop listening.
//
// The buffer is deep enough for a burst of streaming deltas. A subscriber that
// falls further behind than that loses lines rather than backing up into the
// agent, because a slow HTTP client must not be able to stall a running turn.
func (in *Instance) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 256)

	in.mu.Lock()
	select {
	case <-in.done:
		// Already finished: hand back a closed channel so a caller's range
		// ends immediately instead of waiting for an event that cannot come.
		in.mu.Unlock()
		close(ch)
		return ch, func() {}
	default:
	}
	id := in.next
	in.next++
	in.subs[id] = ch
	in.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			in.mu.Lock()
			if existing, ok := in.subs[id]; ok {
				delete(in.subs, id)
				close(existing)
			}
			in.mu.Unlock()
		})
	}
}

// Done is closed once the process has exited and every waiter is released.
func (in *Instance) Done() <-chan struct{} { return in.done }

// Err reports why the process stopped. It is only meaningful after Done.
func (in *Instance) Err() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.exitErr
}

// Close asks the process to stop by closing its stdin, which is how a tau in
// RPC mode is told the conversation is over, and waits for it to go.
func (in *Instance) Close(ctx context.Context) error {
	_ = in.closer.Close()
	select {
	case <-in.done:
		return nil
	case <-ctx.Done():
		// It did not take the hint. Killing is the only remaining move, and
		// leaving it running would leak a process holding a session file.
		if in.cmd.Process != nil {
			_ = in.cmd.Process.Kill()
		}
		<-in.done
		return ctx.Err()
	}
}
