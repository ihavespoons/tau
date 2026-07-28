package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/ihavespoons/tau/ai"
)

// QueueMode controls how many queued messages are delivered at once.
type QueueMode string

const (
	// QueueOneAtATime delivers a single queued message per poll (Pi's default).
	QueueOneAtATime QueueMode = "one-at-a-time"
	// QueueAll delivers the whole queue per poll.
	QueueAll QueueMode = "all"
)

// ErrBusy is returned when a prompt arrives while a run is in flight.
var ErrBusy = errors.New("agent: busy")

// Agent is a stateful wrapper around the loop: it owns the transcript, the
// steering and follow-up queues, subscriptions, and cancellation.
//
// Concurrency: exported methods are safe to call from any goroutine. Sinks are
// invoked on the goroutine running the turn, in order.
type Agent struct {
	mu sync.Mutex

	systemPrompt  string
	model         *ai.Model
	thinkingLevel ai.ModelThinkingLevel
	tools         []Tool
	messages      []ai.Message

	stream ai.SimpleStreamFunc
	config LoopConfig

	steerQueue    []ai.Message
	followUpQueue []ai.Message
	SteeringMode  QueueMode
	FollowUpMode  QueueMode

	sinks []Sink

	running   bool
	cancel    context.CancelFunc
	streaming *ai.AssistantMessage
	pending   map[string]struct{}
	errMsg    string
	idle      chan struct{} // closed when a run settles; nil while idle
}

// Options configures a new Agent.
type Options struct {
	SystemPrompt  string
	Model         *ai.Model
	ThinkingLevel ai.ModelThinkingLevel
	Tools         []Tool
	Messages      []ai.Message
	Stream        ai.SimpleStreamFunc
	// Config supplies loop hooks. Model, Stream, Reasoning, and the queue
	// pollers are managed by the Agent and overwritten.
	Config LoopConfig
}

// NewAgent builds an Agent.
func NewAgent(opts Options) *Agent {
	return &Agent{
		systemPrompt:  opts.SystemPrompt,
		model:         opts.Model,
		thinkingLevel: opts.ThinkingLevel,
		tools:         append([]Tool{}, opts.Tools...),
		messages:      append([]ai.Message{}, opts.Messages...),
		stream:        opts.Stream,
		config:        opts.Config,
		SteeringMode:  QueueOneAtATime,
		FollowUpMode:  QueueOneAtATime,
		pending:       map[string]struct{}{},
	}
}

// Subscribe registers an event sink. Sinks are invoked in registration order.
func (a *Agent) Subscribe(s Sink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sinks = append(a.sinks, s)
}

// Messages returns a copy of the transcript.
func (a *Agent) Messages() []ai.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ai.Message{}, a.messages...)
}

// SetMessages replaces the transcript.
func (a *Agent) SetMessages(msgs []ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append([]ai.Message{}, msgs...)
}

// Tools returns a copy of the active tool set.
func (a *Agent) Tools() []Tool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Tool{}, a.tools...)
}

// SetTools replaces the active tool set.
func (a *Agent) SetTools(tools []Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools = append([]Tool{}, tools...)
}

// Model returns the active model.
func (a *Agent) Model() *ai.Model {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// SetModel swaps the model used for subsequent turns.
func (a *Agent) SetModel(m *ai.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = m
}

// ThinkingLevel returns the requested reasoning level.
func (a *Agent) ThinkingLevel() ai.ModelThinkingLevel {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.thinkingLevel
}

// SetThinkingLevel sets the reasoning level for subsequent turns.
func (a *Agent) SetThinkingLevel(l ai.ModelThinkingLevel) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinkingLevel = l
}

// SystemPrompt returns the active system prompt.
func (a *Agent) SystemPrompt() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.systemPrompt
}

// SetSystemPrompt replaces the system prompt.
func (a *Agent) SetSystemPrompt(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.systemPrompt = s
}

// IsRunning reports whether a run is in flight.
func (a *Agent) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// ErrorMessage returns the error from the most recent failed or aborted turn.
func (a *Agent) ErrorMessage() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.errMsg
}

// PendingToolCalls returns the ids of tool calls currently executing.
func (a *Agent) PendingToolCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.pending))
	for id := range a.pending {
		out = append(out, id)
	}
	return out
}

// Steer queues a message for injection at the next turn boundary. Tool calls
// already in flight still complete.
func (a *Agent) Steer(msgs ...ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steerQueue = append(a.steerQueue, msgs...)
}

// FollowUp queues a message delivered only when the agent would otherwise stop.
func (a *Agent) FollowUp(msgs ...ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUpQueue = append(a.followUpQueue, msgs...)
}

// AbortResult reports what a cancellation discarded.
type AbortResult struct {
	ClearedSteer    int
	ClearedFollowUp int
}

// Abort cancels the in-flight run and clears both queues.
func (a *Agent) Abort() AbortResult {
	a.mu.Lock()
	res := AbortResult{ClearedSteer: len(a.steerQueue), ClearedFollowUp: len(a.followUpQueue)}
	a.steerQueue = nil
	a.followUpQueue = nil
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return res
}

// WaitForIdle blocks until any in-flight run settles.
func (a *Agent) WaitForIdle(ctx context.Context) error {
	a.mu.Lock()
	idle := a.idle
	a.mu.Unlock()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drain pops from a queue according to its mode.
func drain(q *[]ai.Message, mode QueueMode) []ai.Message {
	if len(*q) == 0 {
		return nil
	}
	if mode == QueueAll {
		out := *q
		*q = nil
		return out
	}
	out := []ai.Message{(*q)[0]}
	*q = (*q)[1:]
	return out
}

// Prompt runs one agent loop with the given messages appended to the
// transcript. It returns ErrBusy if a run is already in flight.
func (a *Agent) Prompt(ctx context.Context, prompts ...ai.Message) ([]ai.Message, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return nil, ErrBusy
	}
	a.running = true
	a.errMsg = ""
	a.idle = make(chan struct{})
	idle := a.idle

	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel

	rc := &RunContext{
		SystemPrompt: a.systemPrompt,
		Messages:     append([]ai.Message{}, a.messages...),
		Tools:        append([]Tool{}, a.tools...),
	}
	cfg := a.loopConfigLocked()
	sinks := append([]Sink{a.trackSink}, a.sinks...)
	a.mu.Unlock()

	defer func() {
		cancel()
		a.mu.Lock()
		a.running = false
		a.cancel = nil
		a.streaming = nil
		a.idle = nil
		a.mu.Unlock()
		close(idle)
	}()

	produced, err := RunLoop(runCtx, prompts, rc, cfg, sinks...)

	a.mu.Lock()
	a.messages = append(a.messages, produced...)
	a.mu.Unlock()

	return produced, err
}

// loopConfigLocked builds the per-run config. Caller must hold a.mu.
func (a *Agent) loopConfigLocked() LoopConfig {
	cfg := a.config
	cfg.Model = a.model
	cfg.Stream = a.stream
	if a.thinkingLevel != "" && a.thinkingLevel != ai.ThinkingOff {
		cfg.Reasoning = ai.ThinkingLevel(a.thinkingLevel)
	} else {
		cfg.Reasoning = ""
	}

	userSteering := cfg.GetSteeringMessages
	cfg.GetSteeringMessages = func(ctx context.Context) ([]ai.Message, error) {
		a.mu.Lock()
		out := drain(&a.steerQueue, a.SteeringMode)
		a.mu.Unlock()
		if userSteering != nil {
			extra, err := userSteering(ctx)
			if err != nil {
				return out, err
			}
			out = append(out, extra...)
		}
		return out, nil
	}

	userFollowUp := cfg.GetFollowUpMessages
	cfg.GetFollowUpMessages = func(ctx context.Context) ([]ai.Message, error) {
		a.mu.Lock()
		out := drain(&a.followUpQueue, a.FollowUpMode)
		a.mu.Unlock()
		if userFollowUp != nil {
			extra, err := userFollowUp(ctx)
			if err != nil {
				return out, err
			}
			out = append(out, extra...)
		}
		return out, nil
	}

	return cfg
}

// trackSink maintains streaming/pending/error state from the event stream.
func (a *Agent) trackSink(_ context.Context, ev Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch ev.Type {
	case EventMessageUpdate, EventMessageStart:
		if m, ok := ev.Message.(ai.AssistantMessage); ok {
			cp := m
			a.streaming = &cp
		}
	case EventMessageEnd:
		if m, ok := ev.Message.(ai.AssistantMessage); ok {
			a.streaming = nil
			if m.StopReason == ai.StopError || m.StopReason == ai.StopAborted {
				a.errMsg = m.ErrorMessage
			}
		}
	case EventToolExecutionStart:
		a.pending[ev.ToolCallID] = struct{}{}
	case EventToolExecutionEnd:
		delete(a.pending, ev.ToolCallID)
	}
	return nil
}

// StreamingMessage returns the in-flight assistant message, if any.
func (a *Agent) StreamingMessage() *ai.AssistantMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streaming
}
