package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihavespoons/tau/rpc"
)

// TestHelperProcess is the stand-in tau. Re-executing the test binary is how a
// test gets a real subprocess speaking the real protocol without needing a
// built tau, a model, or an API key.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("TAU_SERVER_TEST_HELPER")
	if mode == "" {
		return
	}
	out := rpc.NewWriter(os.Stdout)

	switch mode {
	case "crash":
		os.Exit(3)

	case "events":
		for i := range 3 {
			out.EmitEvent(rpc.Event{Type: "agent_event", ToolCallID: string(rune('a' + i))})
		}
		os.Exit(0)

	case "flood":
		// More than a subscriber's buffer, to prove a slow reader is dropped
		// rather than allowed to stall the process.
		for range 2000 {
			out.EmitEvent(rpc.Event{Type: "agent_event"})
		}
		os.Exit(0)
	}

	// "echo" and "silent" both read commands; only echo answers.
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for in.Scan() {
		var cmd rpc.Command
		if err := json.Unmarshal(in.Bytes(), &cmd); err != nil {
			continue
		}
		if mode == "silent" {
			continue
		}
		// An event first, so a test can prove the two are told apart by shape
		// rather than by arrival order.
		out.EmitEvent(rpc.Event{Type: "agent_event", ToolCallID: cmd.ID})
		out.Emit(rpc.Response{ID: cmd.ID, Type: "response", Success: true})
	}
	os.Exit(0)
}

func helper(t *testing.T, mode string) *Instance {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "TAU_SERVER_TEST_HELPER="+mode)
	cmd.Stderr = nil

	in, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = in.Close(ctx)
	})
	return in
}

func TestACommandGetsItsOwnResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := helper(t, "echo")

	res, err := in.Do(ctx, rpc.Command{ID: "c1", Type: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "c1" || !res.Success {
		t.Errorf("response = %+v, want the reply to c1", res)
	}
}

// Responses are matched by id, not by order, so commands in flight together
// must not receive each other's replies.
func TestConcurrentCommandsDoNotCrossTalk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in := helper(t, "echo")

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "cmd-" + string(rune('a'+i))
			res, err := in.Do(ctx, rpc.Command{ID: id, Type: "prompt"})
			if err != nil {
				errs <- err
				return
			}
			if res.ID != id {
				errs <- errors.New("got response for " + res.ID + ", wanted " + id)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A caller waiting on a process that dies must be released with a reason, not
// left blocked forever.
func TestAWaiterIsReleasedWhenTheProcessDies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := helper(t, "silent")

	done := make(chan error, 1)
	go func() {
		_, err := in.Do(ctx, rpc.Command{ID: "never-answered", Type: "prompt"})
		done <- err
	}()

	// Closing stdin ends the helper's read loop, so it exits without replying.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := in.Close(closeCtx); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrInstanceGone) {
			t.Errorf("err = %v, want it to say the instance is gone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was never released")
	}
}

func TestACancelledCommandStopsWaiting(t *testing.T) {
	in := helper(t, "silent")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := in.Do(ctx, rpc.Command{ID: "c1", Type: "prompt"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}

	// The abandoned command must not be left in the pending table, or its id
	// could never be used again.
	in.mu.Lock()
	pending := len(in.pending)
	in.mu.Unlock()
	if pending != 0 {
		t.Errorf("%d commands still pending after cancellation", pending)
	}
}

// An event is not a response and must reach subscribers rather than a waiter.
func TestSubscribersSeeEventsButNotResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := helper(t, "echo")

	lines, stop := in.Subscribe()
	defer stop()

	if _, err := in.Do(ctx, rpc.Command{ID: "c1", Type: "prompt"}); err != nil {
		t.Fatal(err)
	}

	select {
	case line := <-lines:
		if !strings.Contains(string(line), "agent_event") {
			t.Errorf("first line was %q, want the event", line)
		}
		if strings.Contains(string(line), `"type":"response"`) {
			t.Error("a correlated response was broadcast to subscribers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
	}
}

func TestUnsubscribingClosesTheChannel(t *testing.T) {
	in := helper(t, "echo")

	lines, stop := in.Subscribe()
	stop()
	select {
	case _, ok := <-lines:
		if ok {
			t.Error("a line arrived after unsubscribing")
		}
	case <-time.After(2 * time.Second):
		t.Error("the channel was not closed on unsubscribe")
	}
	// Stopping twice must not panic on a double close.
	stop()
}

// A slow subscriber must lose lines rather than back up into the agent: a
// stalled HTTP client cannot be allowed to freeze a running turn.
func TestASlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	in := helper(t, "flood")

	lines, stop := in.Subscribe()
	defer stop()

	select {
	case <-in.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the process never finished, so a slow subscriber blocked it")
	}

	drained := 0
	for range lines {
		drained++
	}
	if drained == 0 {
		t.Error("the subscriber received nothing at all")
	}
	if drained >= 2000 {
		t.Errorf("received %d lines, so nothing was dropped and the buffer is unbounded", drained)
	}
}

func TestSubscribingAfterExitEndsImmediately(t *testing.T) {
	in := helper(t, "crash")

	select {
	case <-in.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the process never exited")
	}

	lines, stop := in.Subscribe()
	defer stop()
	select {
	case _, ok := <-lines:
		if ok {
			t.Error("a line arrived from a process that had already exited")
		}
	case <-time.After(2 * time.Second):
		t.Error("subscribing after exit did not end")
	}
}

func TestAFailedProcessReportsWhy(t *testing.T) {
	in := helper(t, "crash")

	select {
	case <-in.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the process never exited")
	}
	if in.Err() == nil {
		t.Error("a process that exited non-zero reported no error")
	}

	// And a command sent afterwards fails rather than hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := in.Do(ctx, rpc.Command{ID: "c1", Type: "prompt"}); !errors.Is(err, ErrInstanceGone) {
		t.Errorf("err = %v, want it to say the instance is gone", err)
	}
}
