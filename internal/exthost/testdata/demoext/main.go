// Command demoext is a wire-protocol extension used by exthost's tests.
//
// It is a real subprocess speaking the real protocol through the reference
// server in extension/wire, so what the tests exercise is the transport, not a
// mock of it. Behaviour is selected with TAU_DEMO_MODE so one binary can play
// the co-operative extension, the one that hangs, and the one that dies.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ihavespoons/tau/extension/wire"
)

func mode() string {
	m := os.Getenv("TAU_DEMO_MODE")
	if m == "" {
		return "normal"
	}
	return m
}

func main() {
	h := wire.Handler{
		Init:        onInit,
		Event:       onEvent,
		Tool:        onTool,
		Command:     onCommand,
		Completions: onCompletions,
		Shortcut:    onShortcut,
		Render:      onRender,
	}
	if err := wire.ServeStdio(context.Background(), h); err != nil {
		fmt.Fprintln(os.Stderr, "demoext:", err)
		os.Exit(1)
	}
}

func onInit(in wire.Init) (wire.InitResult, error) {
	switch mode() {
	case "refuse":
		return wire.InitResult{Error: "this extension declines to load"}, nil
	case "badversion":
		return wire.InitResult{Protocol: 99}, nil
	case "silent":
		// Never answers the handshake at all.
		select {}
	}

	res := wire.InitResult{
		Name: "demoext",
		Subscriptions: []string{
			"session_start", "tool_call", "message_update",
			"input", "before_provider_headers", "context",
		},
		Tools: []wire.ToolDecl{{
			Name:        "demo_echo",
			Description: "Echo the text back",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
			Streaming:   true,
		}},
		Commands: []wire.CommandDecl{{
			Name: "demo", Description: "A demo command", Completions: true,
		}},
		Shortcuts: []wire.ShortcutDecl{{Key: "ctrl+d", Description: "Demo shortcut"}},
		Flags:     []wire.FlagDecl{{Name: "demo-flag", Type: "bool", Description: "A demo flag"}},
		Renderers: []wire.RendererDecl{{Kind: "message", Selector: "assistant"}},
		Warnings:  []string{"demo warning"},
	}
	if in.Cwd == "" {
		res.Error = "host did not report a cwd"
	}
	return res, nil
}

// blockedText is the argument value the gate refuses.
const blockedText = "forbidden"

func onEvent(ctx context.Context, ev wire.Event, c *wire.Conn) (json.RawMessage, error) {
	switch ev.Event {
	case "tool_call":
		switch mode() {
		case "hang":
			// Wait for the host to give up, then answer anyway. A cancelled
			// request that still answers within the grace period is a real
			// answer; one that does not is what the fail-safe is for.
			<-ctx.Done()
			time.Sleep(30 * time.Second)
			return nil, nil
		case "crash":
			os.Exit(3)
		case "handlererror":
			return nil, fmt.Errorf("the gate is broken")
		}

		var e struct {
			ToolName string         `json:"toolName"`
			Args     map[string]any `json:"args"`
		}
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			return nil, err
		}
		if text, _ := e.Args["text"].(string); text == blockedText {
			return json.Marshal(map[string]any{
				"result": map[string]any{"block": true, "reason": "demoext says no"},
			})
		}
		if text, _ := e.Args["text"].(string); text == "rewrite" {
			return json.Marshal(map[string]any{
				"args": map[string]any{"text": "rewritten by demoext"},
			})
		}
		return nil, nil

	case "input":
		var e struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			return nil, err
		}
		if mode() == "slowinput" {
			// Long enough for the host to replace the session while the
			// answer is still in flight.
			time.Sleep(300 * time.Millisecond)
		}
		if e.Text == "shout" {
			return json.Marshal(map[string]any{"action": "transform", "text": "SHOUT"})
		}
		return json.Marshal(map[string]any{"action": "continue"})

	case "before_provider_headers":
		// Headers are mutated in place in process; over the wire the extension
		// names the ones it wants changed and the host merges them.
		return json.Marshal(map[string]any{
			"headers": map[string]any{"x-demo": "1", "x-drop-me": nil},
		})

	case "context":
		// Returning no payload must mean "no opinion", not "an empty message
		// list" — a policy that confused the two would erase the conversation.
		return nil, nil

	case "message_update":
		// A hot event. Recording it lets a test observe conflation by asking
		// the extension how many it actually received.
		hotSeen.mu.Lock()
		hotSeen.n++
		hotSeen.mu.Unlock()
		return nil, nil

	case "session_start":
		if mode() == "askui" {
			go askEverything(c)
		}
		return nil, nil
	}
	return nil, nil
}

func askEverything(c *wire.Conn) {
	ctx := context.Background()
	c.Log("info", "asking the host things")

	if raw, cancelled, err := c.UI(ctx, wire.UIRequest{
		Method: "confirm", Title: "Proceed?", Message: "well?",
	}); err == nil && !cancelled {
		c.Log("info", "confirm="+string(raw))
	}
	if raw, cancelled, err := c.UI(ctx, wire.UIRequest{
		Method: "select", Title: "Pick", Options: []string{"a", "b"},
	}); err == nil && !cancelled {
		c.Log("info", "select="+string(raw))
	}
	if raw, err := c.Action(ctx, "exec", wire.ExecParams{Command: "echo hello"}); err == nil {
		c.Log("info", "exec="+string(raw))
	}
	if raw, err := c.Action(ctx, "getModel", nil); err == nil {
		c.Log("info", "model="+string(raw))
	}
	if _, err := c.Action(ctx, "setSessionName", wire.NameParams{Name: "named by demoext"}); err != nil {
		c.Log("error", "setSessionName: "+err.Error())
	}
	c.Log("info", "asked everything")
}

func onTool(ctx context.Context, req wire.ToolExecute, _ *wire.Conn, update func(wire.ToolResultPayload)) (wire.ToolResultPayload, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return wire.ToolResultPayload{}, err
	}
	switch args.Text {
	case "fail":
		return wire.ToolResultPayload{Output: "the tool refused", IsError: true}, nil
	case "hang":
		<-ctx.Done()
		return wire.ToolResultPayload{Output: "cancelled"}, nil
	}
	update(wire.ToolResultPayload{Output: "working on " + args.Text})
	return wire.ToolResultPayload{
		Output:  "echo: " + args.Text,
		Details: map[string]any{"callId": req.CallID},
	}, nil
}

// hotSeen counts the hot events that actually arrived. Conflation is only
// observable from this side: the host drops payloads before they are written,
// so the count the extension reports is the evidence.
var hotSeen struct {
	mu sync.Mutex
	n  int
}

func onCommand(_ context.Context, req wire.Command, c *wire.Conn) error {
	switch req.Args {
	case "boom":
		return fmt.Errorf("the command failed")
	case "hotcount":
		hotSeen.mu.Lock()
		n := hotSeen.n
		hotSeen.mu.Unlock()
		c.Log("info", fmt.Sprintf("hot=%d", n))
		return nil
	}
	c.Log("info", "command ran with args "+req.Args)
	return nil
}

func onCompletions(_ context.Context, req wire.Completions, _ *wire.Conn) ([]wire.CompletionItem, error) {
	if mode() == "slowcompletions" {
		time.Sleep(30 * time.Second)
	}
	return []wire.CompletionItem{
		{Value: req.Prefix + "-one", Label: "one"},
		{Value: req.Prefix + "-two", Label: "two"},
	}, nil
}

func onShortcut(_ context.Context, _ wire.Shortcut, c *wire.Conn) error {
	c.Log("info", "shortcut fired")
	return nil
}

func onRender(_ context.Context, req wire.Render, _ *wire.Conn) ([]string, error) {
	if mode() == "slowrender" {
		time.Sleep(30 * time.Second)
	}
	return []string{fmt.Sprintf("rendered %s at width %d", req.Kind, req.Width)}, nil
}
