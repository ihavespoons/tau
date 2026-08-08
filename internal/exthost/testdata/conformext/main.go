// Command conformext is the conformance extension, written as a standalone
// program speaking the wire protocol.
//
// It must behave identically to the in-process version in
// conformance_test.go's conformanceExtension and to the TypeScript one in
// testdata/piexts/conformance.ts. Where the three differ, the in-process one
// is right: its composition policies were ported from Pi line by line.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ihavespoons/tau/extension/wire"
)

func main() {
	h := wire.Handler{
		Init: func(wire.Init) (wire.InitResult, error) {
			return wire.InitResult{
				Name:          "conformance",
				Subscriptions: []string{"tool_call", "input", "context"},
				Tools: []wire.ToolDecl{{
					Name:        "conform_echo",
					Description: "Echo the text back",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				}},
				Commands: []wire.CommandDecl{{Name: "conform", Description: "conformance command"}},
			}, nil
		},
		Event:   onEvent,
		Tool:    onTool,
		Command: onCommand,
	}
	if err := wire.ServeStdio(context.Background(), h); err != nil {
		fmt.Fprintln(os.Stderr, "conformext:", err)
		os.Exit(1)
	}
}

func onEvent(_ context.Context, ev wire.Event, _ *wire.Conn) (json.RawMessage, error) {
	switch ev.Event {
	case "tool_call":
		var e struct {
			Args map[string]any `json:"args"`
		}
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			return nil, err
		}
		switch text, _ := e.Args["text"].(string); text {
		case "blocked":
			return json.Marshal(map[string]any{
				"result": map[string]any{"block": true, "reason": "conformance says no"},
			})
		case "rewrite":
			return json.Marshal(map[string]any{
				"args": map[string]any{"text": "rewritten"},
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
		if e.Text == "transform-me" {
			return json.Marshal(map[string]any{"action": "transform", "text": "transformed"})
		}
		// No opinion. Not an empty transform, which would replace the input
		// with nothing.
		return nil, nil

	case "context":
		// Likewise, and here the cost of confusing the two is the whole
		// conversation rather than one line.
		return nil, nil
	}
	return nil, nil
}

func onTool(_ context.Context, req wire.ToolExecute, _ *wire.Conn, _ func(wire.ToolResultPayload)) (wire.ToolResultPayload, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return wire.ToolResultPayload{}, err
	}
	if args.Text == "fail" {
		return wire.ToolResultPayload{Output: "tool failure", IsError: true}, nil
	}
	return wire.ToolResultPayload{
		Output:  "echo:" + args.Text,
		Details: map[string]any{"callId": req.CallID},
	}, nil
}

func onCommand(_ context.Context, req wire.Command, _ *wire.Conn) error {
	if req.Args == "fail" {
		return fmt.Errorf("conformance failure")
	}
	return nil
}
