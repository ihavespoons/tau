package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/faux"
)

func newTestAgent(script *faux.Script, tools ...Tool) *Agent {
	return NewAgent(Options{
		SystemPrompt: "sys",
		Model:        faux.Model(),
		Tools:        tools,
		Stream:       script.StreamSimple,
	})
}

func TestAgentPromptAppendsToTranscript(t *testing.T) {
	a := newTestAgent(faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "hello"}}}))

	produced, err := a.Prompt(context.Background(), userMsg("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(produced) != 2 {
		t.Fatalf("produced = %d", len(produced))
	}
	if got := a.Messages(); len(got) != 2 {
		t.Errorf("transcript = %d messages, want 2", len(got))
	}
	// A second prompt continues the same transcript.
	if _, err := a.Prompt(context.Background(), userMsg("again")); err != nil {
		t.Fatal(err)
	}
	if got := a.Messages(); len(got) != 4 {
		t.Errorf("transcript after 2nd prompt = %d, want 4", len(got))
	}
}

func TestAgentRejectsConcurrentPrompts(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{Text: "slow"}}, Delay: 150 * time.Millisecond},
		faux.Turn{Blocks: []faux.Block{{Text: "second"}}},
	)
	a := newTestAgent(script)

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = a.Prompt(context.Background(), userMsg("one"))
	}()
	<-started
	time.Sleep(40 * time.Millisecond)

	if _, err := a.Prompt(context.Background(), userMsg("two")); !errors.Is(err, ErrBusy) {
		t.Errorf("second prompt err = %v, want ErrBusy", err)
	}
	if err := a.WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.IsRunning() {
		t.Error("agent should be idle after WaitForIdle")
	}
}

func TestAgentSteerIsDeliveredAtNextTurn(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "acknowledged"}}},
	)
	a := newTestAgent(script, &echoTool{name: "echo", delay: 60 * time.Millisecond})

	go func() {
		time.Sleep(30 * time.Millisecond)
		a.Steer(userMsg("change of plan"))
	}()

	produced, err := a.Prompt(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	var steered bool
	for _, m := range produced {
		if u, ok := m.(ai.UserMessage); ok && u.Content.Text == "change of plan" {
			steered = true
		}
	}
	if !steered {
		t.Error("steering message was never delivered")
	}
}

func TestAgentFollowUpRunsAfterAgentWouldStop(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{Text: "first"}}},
		faux.Turn{Blocks: []faux.Block{{Text: "second"}}},
	)
	a := newTestAgent(script)
	a.FollowUp(userMsg("and another thing"))

	rec := &recorder{}
	a.Subscribe(rec.sink)

	if _, err := a.Prompt(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if rec.count(EventTurnStart) != 2 {
		t.Errorf("turns = %d, want 2 (follow-up should revive the agent)", rec.count(EventTurnStart))
	}
}

func TestAgentQueueModes(t *testing.T) {
	t.Run("one-at-a-time delivers a single message per poll", func(t *testing.T) {
		q := []ai.Message{userMsg("a"), userMsg("b"), userMsg("c")}
		got := drain(&q, QueueOneAtATime)
		if len(got) != 1 || len(q) != 2 {
			t.Errorf("drained %d, %d left", len(got), len(q))
		}
	})
	t.Run("all drains the queue", func(t *testing.T) {
		q := []ai.Message{userMsg("a"), userMsg("b")}
		got := drain(&q, QueueAll)
		if len(got) != 2 || len(q) != 0 {
			t.Errorf("drained %d, %d left", len(got), len(q))
		}
	})
}

func TestAgentAbortStopsRunPromptly(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "slow", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "unreached"}}},
	)
	a := newTestAgent(script, &echoTool{name: "slow", delay: 3 * time.Second})
	a.FollowUp(userMsg("follow"))

	results := make(chan AbortResult, 1)
	go func() {
		time.Sleep(60 * time.Millisecond)
		results <- a.Abort()
	}()

	start := time.Now()
	if _, err := a.Prompt(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("abort did not stop the run promptly")
	}
	// The follow-up queue is only polled once the agent would stop, so an
	// abort mid-tool-execution must discard it.
	if res := <-results; res.ClearedFollowUp != 1 {
		t.Errorf("abort result = %+v, want the pending follow-up cleared", res)
	}
	if a.IsRunning() {
		t.Error("agent still running after abort")
	}
}

// Abort clears both queues; steering is asserted here rather than mid-run
// because the loop legitimately drains it at each turn boundary.
func TestAgentAbortClearsBothQueues(t *testing.T) {
	a := newTestAgent(faux.NewScript())
	a.Steer(userMsg("s1"), userMsg("s2"))
	a.FollowUp(userMsg("f1"))

	res := a.Abort()
	if res.ClearedSteer != 2 || res.ClearedFollowUp != 1 {
		t.Errorf("abort result = %+v, want 2 steer / 1 follow-up cleared", res)
	}
	if res2 := a.Abort(); res2.ClearedSteer != 0 || res2.ClearedFollowUp != 0 {
		t.Errorf("second abort = %+v, want empty queues", res2)
	}
}

func TestAgentTracksPendingToolCallsAndErrors(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "partial"}}, ErrorMessage: "provider died"},
	)
	a := newTestAgent(script, &echoTool{name: "echo", delay: 50 * time.Millisecond})

	var sawPending bool
	var mu sync.Mutex
	a.Subscribe(func(_ context.Context, ev Event) error {
		if ev.Type == EventToolExecutionStart {
			mu.Lock()
			sawPending = len(a.PendingToolCalls()) > 0
			mu.Unlock()
		}
		return nil
	})

	if _, err := a.Prompt(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawPending {
		t.Error("pending tool calls were never observed mid-execution")
	}
	if len(a.PendingToolCalls()) != 0 {
		t.Error("pending tool calls should be empty once the run settles")
	}
	if a.ErrorMessage() != "provider died" {
		t.Errorf("ErrorMessage = %q", a.ErrorMessage())
	}
}

func TestAgentStateAccessorsAreCopies(t *testing.T) {
	a := newTestAgent(faux.NewScript())
	a.SetMessages([]ai.Message{userMsg("one")})

	got := a.Messages()
	got[0] = userMsg("mutated")

	if a.Messages()[0].(ai.UserMessage).Content.Text != "one" {
		t.Error("Messages() must return a copy, not the live slice")
	}

	tools := []Tool{&echoTool{name: "a"}}
	a.SetTools(tools)
	tools[0] = &echoTool{name: "swapped"}
	if a.Tools()[0].Def().Name != "a" {
		t.Error("SetTools must copy the slice")
	}
}

func TestAgentModelAndThinkingSwap(t *testing.T) {
	a := newTestAgent(faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "ok"}}}))
	m2 := faux.Model()
	m2.ID = "other"
	a.SetModel(m2)
	a.SetThinkingLevel(ai.ModelThinkingLevel(ai.ThinkingHigh))

	if a.Model().ID != "other" {
		t.Errorf("model = %s", a.Model().ID)
	}
	if a.ThinkingLevel() != ai.ModelThinkingLevel(ai.ThinkingHigh) {
		t.Errorf("thinking = %s", a.ThinkingLevel())
	}
	if _, err := a.Prompt(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}
}

func TestAgentWaitForIdleRespectsContext(t *testing.T) {
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "slow"}}, Delay: 2 * time.Second})
	a := newTestAgent(script)

	go func() { _, _ = a.Prompt(context.Background(), userMsg("go")) }()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := a.WaitForIdle(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitForIdle err = %v, want DeadlineExceeded", err)
	}
	a.Abort()
}
