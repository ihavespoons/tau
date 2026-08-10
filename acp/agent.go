package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/session"
)

// NewSessionFunc builds a tau session for a working directory.
//
// It is supplied rather than built here so a test can hand back a session on a
// scripted provider, and so an embedder can decide what tools and extensions
// the agent an editor drives is allowed to have.
type NewSessionFunc func(ctx context.Context, cwd string) (*coding.Session, error)

// Agent answers ACP on one connection.
//
// One process, many sessions: an editor opens a session per project and keeps
// them for as long as the window is open, so the map outlives any single turn.
type Agent struct {
	newSession NewSessionFunc
	version    string

	conn *Conn

	mu       sync.Mutex
	sessions map[string]*liveSession
}

// liveSession is one ACP session and the tau session behind it.
type liveSession struct {
	id string
	cs *coding.Session

	mu sync.Mutex
	// cancel stops the turn in flight. Nil between turns.
	cancel context.CancelFunc
	// cancelled records that the client asked, so the turn can answer with the
	// stop reason the protocol requires rather than an error.
	cancelled bool
}

// NewAgent builds an adapter.
func NewAgent(newSession NewSessionFunc, version string) *Agent {
	return &Agent{
		newSession: newSession,
		version:    version,
		sessions:   map[string]*liveSession{},
	}
}

// Serve runs the adapter until the stream ends.
//
// Nothing may be written to w that is not an ACP message — the transport says
// so — which is why logging goes to stderr and never through here.
func (a *Agent) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return a.Attach(r, w).Serve(ctx)
}

// Attach builds the connection without reading from it yet.
//
// Serving blocks, so a caller that wants to hold the connection — to send a
// notification the moment the agent exists, or to shut it down — needs it
// before the read loop starts. It is also what keeps conn from being assigned
// on one goroutine and read on another without a lock.
func (a *Agent) Attach(r io.Reader, w io.Writer) *Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = NewConn(r, w, a.handle)
	return a.conn
}

func (a *Agent) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case MethodInitialize:
		return a.initialize(params)
	case MethodSessionNew:
		return a.newSessionCall(ctx, params)
	case MethodPrompt:
		return a.prompt(ctx, params)
	case MethodCancel:
		return nil, a.cancel(params)
	case MethodAuthenticate:
		// tau authenticates against providers on its own, through `tau login`
		// and the credentials on disk. There is nothing for a client to do, so
		// the honest answer is a successful no-op rather than an error that
		// would stop a client following the documented flow.
		return struct{}{}, nil
	default:
		return nil, &RPCError{Code: CodeMethodNotFound, Message: "unsupported method " + method}
	}
}

func (a *Agent) initialize(params json.RawMessage) (any, error) {
	var req InitializeRequest
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
	}
	// The response version is the client's if it is supported, otherwise the
	// newest tau speaks — and the client decides whether it can live with that.
	version := ProtocolVersion
	if req.ProtocolVersion > 0 && req.ProtocolVersion < ProtocolVersion {
		version = req.ProtocolVersion
	}

	return InitializeResponse{
		ProtocolVersion: version,
		AgentCapabilities: AgentCapabilities{
			// Resuming an existing session is not wired yet: an ACP session id
			// is not a tau session file, and claiming the capability would
			// promise a load that cannot happen.
			LoadSession: false,
			PromptCapabilities: PromptCapabilities{
				Image: true,
			},
		},
		AgentInfo:   &Implementation{Name: "tau", Version: a.version},
		AuthMethods: []json.RawMessage{},
	}, nil
}

func (a *Agent) newSessionCall(ctx context.Context, params json.RawMessage) (any, error) {
	var req NewSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	if req.Cwd == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "cwd is required"}
	}

	cs, err := a.newSession(ctx, req.Cwd)
	if err != nil {
		return nil, err
	}

	live := &liveSession{id: session.NewID(), cs: cs}

	// One sink for the life of the session, forwarding to whichever turn is
	// running. Subscribing per turn would leak: sinks are registered for good.
	if cs.Agent != nil {
		cs.Agent.Subscribe(func(_ context.Context, ev agent.Event) error {
			a.forward(live, ev)
			return nil
		})
	}

	a.mu.Lock()
	a.sessions[live.id] = live
	a.mu.Unlock()

	return NewSessionResponse{SessionID: live.id}, nil
}

func (a *Agent) lookup(id string) (*liveSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live, ok := a.sessions[id]
	if !ok {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "unknown session " + id}
	}
	return live, nil
}

// prompt runs one turn and answers with why it stopped.
func (a *Agent) prompt(ctx context.Context, params json.RawMessage) (any, error) {
	var req PromptRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	live, err := a.lookup(req.SessionID)
	if err != nil {
		return nil, err
	}

	turnCtx, cancel := context.WithCancel(ctx)
	live.mu.Lock()
	live.cancel = cancel
	live.cancelled = false
	live.mu.Unlock()

	defer func() {
		cancel()
		live.mu.Lock()
		live.cancel = nil
		live.mu.Unlock()
	}()

	messages, err := live.cs.PromptContent(turnCtx, promptContent(req.Prompt))

	live.mu.Lock()
	cancelled := live.cancelled
	live.mu.Unlock()

	// Cancellation is not a failure. The protocol requires this stop reason
	// after session/cancel even when the cancel tore something underneath.
	if cancelled {
		return PromptResponse{StopReason: StopCancelled}, nil
	}
	if err != nil {
		return nil, err
	}
	return PromptResponse{StopReason: stopReason(messages)}, nil
}

// cancel asks the turn in flight to stop. It is a notification, so there is
// nothing to answer: the in-flight prompt reports the outcome.
func (a *Agent) cancel(params json.RawMessage) error {
	var req CancelNotification
	if err := json.Unmarshal(params, &req); err != nil {
		return err
	}
	live, err := a.lookup(req.SessionID)
	if err != nil {
		return err
	}

	live.mu.Lock()
	live.cancelled = true
	cancel := live.cancel
	live.mu.Unlock()

	// Both, and in this order: aborting stops the provider stream, cancelling
	// the context unblocks anything waiting on it.
	if live.cs.Agent != nil {
		live.cs.Agent.Abort()
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// promptContent turns ACP content blocks into a tau user message.
//
// Text is joined rather than kept as separate blocks: tau's user content is a
// string when it is only text, which is the shape a session file records for a
// text-only turn.
func promptContent(blocks []ContentBlock) ai.UserContent {
	var text []string
	var images []ai.ImageContent
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				text = append(text, b.Text)
			}
		case "image":
			if b.Data != "" {
				images = append(images, ai.ImageContent{Data: b.Data, MimeType: b.MimeType})
			}
		case "resource_link":
			// A link is a path the user is pointing at. Naming it in the prompt
			// is what lets the agent decide to read it.
			if b.URI != "" {
				text = append(text, b.URI)
			}
		}
	}

	joined := strings.Join(text, "\n")
	if len(images) == 0 {
		return ai.UserContent{Text: joined}
	}

	blocksOut := make(ai.ContentList, 0, len(images)+1)
	if joined != "" {
		blocksOut = append(blocksOut, ai.TextContent{Text: joined})
	}
	for _, img := range images {
		blocksOut = append(blocksOut, img)
	}
	return ai.UserContent{Blocks: blocksOut}
}

// stopReason maps how the turn ended onto the protocol's vocabulary.
func stopReason(messages []ai.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		am, ok := messages[i].(ai.AssistantMessage)
		if !ok {
			continue
		}
		switch am.StopReason {
		case ai.StopLength:
			return StopMaxTokens
		case ai.StopAborted:
			return StopCancelled
		default:
			return StopEndTurn
		}
	}
	return StopEndTurn
}

// forward turns one tau agent event into the session/update notifications a
// client renders.
func (a *Agent) forward(live *liveSession, ev agent.Event) {
	switch ev.Type {
	case agent.EventMessageUpdate:
		a.forwardDelta(live, ev)
	case agent.EventToolExecutionStart:
		a.notify(live, ToolCallUpdate{
			SessionUpdate: UpdateToolCall,
			ToolCallID:    ev.ToolCallID,
			Title:         ev.ToolName,
			Status:        ToolInProgress,
			RawInput:      ev.Args,
		})
	case agent.EventToolExecutionEnd:
		status := ToolCompleted
		if ev.IsError {
			status = ToolFailed
		}
		a.notify(live, ToolCallUpdate{
			SessionUpdate: UpdateToolCallUpdate,
			ToolCallID:    ev.ToolCallID,
			Status:        status,
			Content:       toolContent(ev.Result),
		})
	}
}

func (a *Agent) forwardDelta(live *liveSession, ev agent.Event) {
	if ev.StreamEvent == nil || ev.StreamEvent.Delta == "" {
		return
	}
	kind := ""
	switch ev.StreamEvent.Type {
	case ai.EventTextDelta:
		kind = UpdateAgentMessageChunk
	case ai.EventThinkingDelta:
		kind = UpdateAgentThoughtChunk
	default:
		// Tool-call argument deltas are not a message chunk. The call is
		// reported once it starts executing, with its arguments whole.
		return
	}
	a.notify(live, ContentChunkUpdate{
		SessionUpdate: kind,
		Content:       ContentBlock{Type: "text", Text: ev.StreamEvent.Delta},
	})
}

// toolContent renders a tool's output as ACP content blocks.
func toolContent(res *agent.ToolResult) []ToolCallContent {
	if res == nil {
		return nil
	}
	var out []ToolCallContent
	for _, c := range res.Content {
		switch v := c.(type) {
		case ai.TextContent:
			if strings.TrimSpace(v.Text) == "" {
				continue
			}
			out = append(out, ToolCallContent{
				Type:    "content",
				Content: ContentBlock{Type: "text", Text: v.Text},
			})
		case ai.ImageContent:
			// Already base64 on tau's side; validated here so a malformed
			// blob is dropped rather than sent as a broken image.
			if _, err := base64.StdEncoding.DecodeString(v.Data); err != nil {
				continue
			}
			out = append(out, ToolCallContent{
				Type:    "content",
				Content: ContentBlock{Type: "image", Data: v.Data, MimeType: v.MimeType},
			})
		}
	}
	return out
}

// notify sends a session/update. A failed write means the client is gone, and
// the read loop will notice; there is nothing useful to do about it here.
func (a *Agent) notify(live *liveSession, update any) {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.Notify(MethodSessionUpdate, SessionNotification{
		SessionID: live.id,
		Update:    update,
	})
}
