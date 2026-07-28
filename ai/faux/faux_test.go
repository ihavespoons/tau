package faux

import (
	"context"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai"
)

func TestScriptedTextTurn(t *testing.T) {
	s := NewScript(Turn{Blocks: []Block{{Text: "hello world", DeltaSize: 4}}})
	stream := s.Stream(context.Background(), Model(), ai.Context{}, nil)
	var types []ai.EventType
	for ev := range stream.Events() {
		types = append(types, ev.Type)
	}
	want := []ai.EventType{ai.EventStart, ai.EventTextStart, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextEnd, ai.EventDone}
	if len(types) != len(want) {
		t.Fatalf("types = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %v want %v", types, want)
		}
	}
	final := stream.Result()
	if final.StopReason != ai.StopStop {
		t.Errorf("stop = %v", final.StopReason)
	}
	if txt := final.Content[0].(ai.TextContent).Text; txt != "hello world" {
		t.Errorf("text = %q", txt)
	}
	if final.Usage.Cost.Total == 0 {
		t.Error("expected non-zero synthesized cost")
	}
}

func TestScriptedToolTurnStopsWithToolUse(t *testing.T) {
	s := NewScript(Turn{Blocks: []Block{
		{Text: "let me look"},
		{ToolCall: &ai.ToolCall{ID: "t1", Name: "bash", Arguments: map[string]any{"command": "ls"}}},
	}})
	final := s.Stream(context.Background(), Model(), ai.Context{}, nil).Result()
	if final.StopReason != ai.StopToolUse {
		t.Errorf("stop = %v", final.StopReason)
	}
	tc, ok := final.Content[1].(ai.ToolCall)
	if !ok || tc.Name != "bash" {
		t.Errorf("content[1] = %#v", final.Content[1])
	}
}

func TestScriptedErrorTurn(t *testing.T) {
	s := NewScript(Turn{Blocks: []Block{{Text: "partial answer"}}, ErrorMessage: "overloaded"})
	final := s.Stream(context.Background(), Model(), ai.Context{}, nil).Result()
	if final.StopReason != ai.StopError || final.ErrorMessage != "overloaded" {
		t.Errorf("final = %+v", final)
	}
	if len(final.Content) != 1 {
		t.Error("partial content should be preserved on error")
	}
}

func TestAbortMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewScript(Turn{Blocks: []Block{{Text: "aaaa", DeltaSize: 1}, {Text: "bbbb"}}, Delay: 30 * time.Millisecond})
	stream := s.Stream(ctx, Model(), ai.Context{}, nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	final := stream.Result()
	if final.StopReason != ai.StopAborted {
		t.Errorf("stop = %v", final.StopReason)
	}
}

func TestScriptExhaustion(t *testing.T) {
	s := NewScript()
	final := s.Stream(context.Background(), Model(), ai.Context{}, nil).Result()
	if final.StopReason != ai.StopStop {
		t.Errorf("stop = %v", final.StopReason)
	}
	if s.Calls() != 1 {
		t.Errorf("calls = %d", s.Calls())
	}
}
