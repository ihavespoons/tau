// Command askext is a wire-protocol extension that asks the user a question as
// soon as the session starts, and gates a tool call on the answer.
//
// It exists to prove the whole path end to end: tau's rpc client asks a
// question it never authored, an extension in a third process receives an
// answer it never asked a terminal for, and the tool call that depended on it
// is allowed or blocked accordingly.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/ihavespoons/tau/extension/wire"
)

// allowed records the user's answer. Until it arrives the gate has no opinion,
// which is the honest state: the extension has not been told anything yet.
var allowed atomic.Bool

func main() {
	h := wire.Handler{
		Init: func(wire.Init) (wire.InitResult, error) {
			return wire.InitResult{
				Name:          "askext",
				Subscriptions: []string{"session_start", "tool_call"},
				Commands:      []wire.CommandDecl{{Name: "askstate", Description: "Report the answer"}},
			}, nil
		},
		Event: onEvent,
		Command: func(_ context.Context, req wire.Command, c *wire.Conn) error {
			c.Log("info", fmt.Sprintf("allowed=%t", allowed.Load()))
			return nil
		},
	}
	if err := wire.ServeStdio(context.Background(), h); err != nil {
		fmt.Fprintln(os.Stderr, "askext:", err)
		os.Exit(1)
	}
}

func onEvent(ctx context.Context, ev wire.Event, c *wire.Conn) (json.RawMessage, error) {
	switch ev.Event {
	case "session_start":
		// Asking on its own goroutine: the host dispatches events one at a
		// time, and blocking this one on a human would stall every other
		// extension behind it.
		go func() {
			raw, cancelled, err := c.UI(context.Background(), wire.UIRequest{
				Method: "confirm", Title: "Allow tools?", Message: "askext would like to know",
			})
			if err != nil {
				c.Log("error", "confirm failed: "+err.Error())
				return
			}
			if cancelled {
				c.Log("info", "the user declined to answer")
				return
			}
			var res wire.UIConfirmed
			if err := json.Unmarshal(raw, &res); err != nil {
				c.Log("error", "undecodable confirm reply")
				return
			}
			allowed.Store(res.Confirmed)
			c.Log("info", fmt.Sprintf("answered=%t", res.Confirmed))
		}()
		return nil, nil

	case "tool_call":
		if allowed.Load() {
			return nil, nil
		}
		return json.Marshal(map[string]any{
			"result": map[string]any{"block": true, "reason": "askext has no permission yet"},
		})
	}
	return nil, nil
}
