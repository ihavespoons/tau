package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/rpc"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/slashcmd"
)

// RPCServer drives a coding session from JSONL commands on a stream.
//
// It is the third headless surface, after print and json, and the only one
// that is bidirectional. That is what it exists for: an editor or a supervisor
// can steer a running agent, answer an extension's dialog, and navigate the
// session tree, none of which a one-shot invocation can do.
type RPCServer struct {
	s   *Session
	out *rpc.Writer

	// running guards the agent: a second prompt while one is in flight is
	// steering, not a concurrent run.
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc

	// pending correlates an extension's dialog with the client's answer.
	pendingMu sync.Mutex
	pending   map[string]chan rpc.ExtensionUIResponse
	nextUIID  atomic.Uint64
}

// NewRPCServer builds a server around an existing session.
func NewRPCServer(s *Session, out io.Writer) *RPCServer {
	srv := &RPCServer{}
	srv.Attach(s, out)
	return srv
}

// Attach binds the session and output stream.
//
// It exists separately from construction because of an ordering problem: the
// extension UI has to be handed to coding.New, and coding.New produces the
// session this server serves. So the server is created first, its UI is passed
// in, and the session is attached once it exists. An extension that opens a
// dialog from its factory then reaches a real client rather than a nil one.
func (r *RPCServer) Attach(s *Session, out io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s = s
	r.out = rpc.NewWriter(out)
	if r.pending == nil {
		r.pending = map[string]chan rpc.ExtensionUIResponse{}
	}
}

// Dispatch runs one command, as though it had arrived on the input stream.
// A prompt given on the command line uses it, so the client sees exactly the
// events it would have seen had it sent the prompt itself.
func (r *RPCServer) Dispatch(ctx context.Context, cmd rpc.Command) { r.dispatch(ctx, cmd) }

// UI returns the extension surface that proxies dialogs to the client.
//
// It is exposed so a caller can attach it before building the session: an
// extension may open a dialog from its factory, and a UI attached afterwards
// would miss it.
func (r *RPCServer) UI() extension.UI { return rpcUI{r} }

// Serve reads commands until the stream ends or ctx is cancelled.
func (r *RPCServer) Serve(ctx context.Context, in io.Reader) error {
	r.s.Agent.Subscribe(r.sink())
	r.emitReady()

	reader := rpc.NewReader(in)
	for {
		line, err := reader.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// An extension_ui_response is not a command: it is the answer to a
		// question tau asked, and it must be routed before the command
		// dispatcher sees a type it does not know.
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			r.out.Emit(rpc.Response{Type: "response", Command: "", Success: false,
				Error: "malformed command: " + err.Error()})
			continue
		}
		if probe.Type == "extension_ui_response" {
			var res rpc.ExtensionUIResponse
			if err := json.Unmarshal(line, &res); err == nil {
				r.deliverUI(res)
			}
			continue
		}

		var cmd rpc.Command
		if err := json.Unmarshal(line, &cmd); err != nil {
			r.out.Emit(rpc.Response{Type: "response", Command: probe.Type, Success: false,
				Error: "malformed command: " + err.Error()})
			continue
		}
		r.dispatch(ctx, cmd)
	}
}

func (r *RPCServer) ok(cmd rpc.Command, data any) {
	res := rpc.Response{ID: cmd.ID, Type: "response", Command: cmd.Type, Success: true}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		res.Data = raw
	}
	r.out.Emit(res)
}

func (r *RPCServer) fail(cmd rpc.Command, err error) {
	r.out.Emit(rpc.Response{
		ID: cmd.ID, Type: "response", Command: cmd.Type, Success: false, Error: err.Error(),
	})
}

func (r *RPCServer) emitReady() {
	model := ""
	if r.s.Model != nil {
		model = string(r.s.Model.Provider) + "/" + r.s.Model.ID
	}
	r.out.EmitEvent(rpc.Event{Type: "ready", SessionPath: r.s.Path, Model: model})
}

func (r *RPCServer) dispatch(ctx context.Context, cmd rpc.Command) {
	switch cmd.Type {

	// --- prompting ---

	case "prompt", "steer", "follow_up":
		r.handlePrompt(ctx, cmd)

	case "abort":
		r.mu.Lock()
		cancel := r.cancel
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		r.ok(cmd, nil)

	// --- state ---

	case "get_state":
		r.ok(cmd, r.state())

	case "get_messages":
		msgs := r.s.Agent.Messages()
		raw, err := json.Marshal(msgs)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, map[string]json.RawMessage{"messages": raw})

	case "get_commands":
		r.ok(cmd, map[string]any{"commands": r.commands()})

	case "get_last_assistant_text":
		r.ok(cmd, map[string]any{"text": r.s.LastAssistantText()})

	// --- model and thinking ---

	case "set_model":
		id := cmd.ModelID
		if cmd.Provider != "" {
			id = cmd.Provider + "/" + cmd.ModelID
		}
		m, err := r.s.SetModel(ctx, id)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, modelInfo(m))

	case "get_available_models":
		ms := r.s.AvailableModels()
		infos := make([]rpc.ModelInfo, 0, len(ms))
		for i := range ms {
			infos = append(infos, *modelInfo(&ms[i]))
		}
		r.ok(cmd, map[string]any{"models": infos})

	case "set_thinking_level":
		r.s.Agent.SetThinkingLevel(ai.ModelThinkingLevel(cmd.Level))
		r.ok(cmd, nil)

	case "get_available_thinking_levels":
		r.ok(cmd, map[string]any{"levels": ai.SupportedThinkingLevels(r.s.Model)})

	// --- session ---

	case "set_session_name":
		if err := (runtimeAdapter{r.s}).SetSessionName(cmd.Name); err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, nil)

	case "compact":
		res, err := r.s.Compact(ctx, cmd.CustomInstructions)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		if res == nil {
			r.ok(cmd, rpc.CompactionResult{Cancelled: true})
			return
		}
		r.ok(cmd, rpc.CompactionResult{
			Summary: res.Summary, TokensBefore: res.TokensBefore,
		})

	case "get_tree":
		r.ok(cmd, map[string]any{"tree": r.tree(ctx), "leafId": r.leafID(ctx)})

	case "get_fork_messages":
		prompts, err := r.s.UserPrompts(ctx)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		msgs := make([]rpc.ForkMessage, 0, len(prompts))
		for _, p := range prompts {
			msgs = append(msgs, rpc.ForkMessage{EntryID: p.EntryID, Text: p.Text})
		}
		r.ok(cmd, map[string]any{"messages": msgs})

	case "fork":
		out, err := (codingHost{r.s}).ForkSession(ctx, cmd.EntryID)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, map[string]any{"text": out, "cancelled": false})

	case "clone":
		if _, err := (codingHost{r.s}).ForkSession(ctx, ""); err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, rpc.CancelledResult{})

	case "switch_session":
		if err := r.s.SwitchSession(ctx, session.Metadata{Path: cmd.SessionPath, Cwd: r.s.Cwd}); err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, rpc.CancelledResult{})

	case "reload":
		out, err := (codingHost{r.s}).Reload(ctx)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, map[string]any{"text": out})

	// --- shell ---

	case "bash":
		out, code, err := (runtimeAdapter{r.s}).Exec(ctx, cmd.Command)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, rpc.BashResult{Output: out, ExitCode: code})

	default:
		r.fail(cmd, fmt.Errorf("unknown command %q", cmd.Type))
	}
}

// handlePrompt starts a run, or delivers into a running one.
//
// The response is emitted before the run finishes: a prompt is asynchronous by
// design, and the client learns the outcome from the event stream. Waiting for
// the run would make the connection unusable for the whole turn, which is
// exactly when a client most wants to steer or abort.
func (r *RPCServer) handlePrompt(ctx context.Context, cmd rpc.Command) {
	// A slash command is invoked through a prompt, which is what get_commands
	// means by "available for invocation". Pi does the same, and it is the
	// only way a client can reach a command an extension registered without
	// the protocol growing a case per command.
	if parsed, ok := slashcmd.Parse(cmd.Message); ok && cmd.Type == "prompt" {
		res, err := r.s.RunCommand(ctx, cmd.Message)
		if err != nil {
			r.fail(cmd, err)
			return
		}
		r.ok(cmd, map[string]any{"output": res.Output, "command": parsed.Name})
		if res.Output != "" {
			r.out.EmitEvent(rpc.Event{Type: "command_output", Delta: res.Output})
		}
		// A command that produces a prompt — a skill, a template — feeds it
		// straight back in, so the client sees a turn rather than a string it
		// would have to resend.
		if res.Prompt != "" {
			r.startRun(ctx, res.Prompt)
		}
		return
	}

	msg := ai.UserMessage{Content: ai.UserContent{Text: cmd.Message}}

	r.mu.Lock()
	running := r.running
	r.mu.Unlock()

	if running || cmd.Type == "steer" || cmd.Type == "follow_up" {
		if !running {
			r.fail(cmd, errors.New("nothing is running to deliver into"))
			return
		}
		switch {
		case cmd.Type == "steer" || cmd.StreamingBehavior == "steer":
			r.s.Agent.Steer(msg)
		default:
			r.s.Agent.FollowUp(msg)
		}
		r.ok(cmd, nil)
		return
	}

	r.ok(cmd, nil)
	r.startRun(ctx, cmd.Message)
}

// startRun begins a turn in the background. The client learns the outcome from
// the event stream, which is what keeps the connection usable for steering and
// aborting while the turn is in flight.
func (r *RPCServer) startRun(ctx context.Context, prompt string) {
	runCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.running, r.cancel = true, cancel
	r.mu.Unlock()

	go func() {
		defer cancel()
		_, err := r.s.Prompt(runCtx, prompt)
		r.mu.Lock()
		r.running, r.cancel = false, nil
		r.mu.Unlock()
		if err != nil {
			r.out.EmitEvent(rpc.Event{Type: "error", Error: err.Error()})
		}
	}()
}

// EmitExtensionLog surfaces a subprocess extension's diagnostic on the client's
// stream. Without it those lines land on tau's stderr, where a program driving
// tau over a pipe never sees them.
func (r *RPCServer) EmitExtensionLog(name, level, message string) {
	w := r.writer()
	if w == nil {
		return
	}
	w.EmitEvent(rpc.Event{
		Type: "extension_log", ExtensionPath: name, DeltaKind: level, Delta: message,
	})
}

func (r *RPCServer) state() rpc.SessionState {
	r.mu.Lock()
	running := r.running
	r.mu.Unlock()

	st := rpc.SessionState{
		ThinkingLevel: string(r.s.Agent.ThinkingLevel()),
		IsStreaming:   running,
		SteeringMode:  string(r.s.Agent.SteeringMode),
		FollowUpMode:  string(r.s.Agent.FollowUpMode),
		SessionFile:   r.s.Path,
		SessionID:     r.s.sessionID,
		SessionName:   (runtimeAdapter{r.s}).SessionName(),
		MessageCount:  len(r.s.Agent.Messages()),
	}
	if r.s.Settings != nil {
		st.AutoCompactionEnabled = true
	}
	st.Model = modelInfo(r.s.Model)
	return st
}

func modelInfo(m *ai.Model) *rpc.ModelInfo {
	if m == nil {
		return nil
	}
	return &rpc.ModelInfo{
		ID: m.ID, Name: m.Name, Provider: string(m.Provider), API: string(m.Api),
		ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Reasoning: m.Reasoning,
	}
}

func (r *RPCServer) commands() []rpc.SlashCommand {
	if r.s.Commands == nil {
		return nil
	}
	var out []rpc.SlashCommand
	for _, info := range r.s.Commands.List() {
		out = append(out, rpc.SlashCommand{
			Name: info.Name, Description: info.Description,
			Source: string(info.Source), SourceInfo: info.SourceInfo,
		})
	}
	return out
}

func (r *RPCServer) tree(ctx context.Context) []rpc.TreeNode {
	nodes, err := r.s.TreeNodes(ctx)
	if err != nil {
		return nil
	}
	return convertTree(nodes)
}

func convertTree(nodes []*session.TreeNode) []rpc.TreeNode {
	out := make([]rpc.TreeNode, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.Entry == nil {
			continue
		}
		base := n.Entry.Base()
		node := rpc.TreeNode{
			ID: base.ID, Kind: n.Entry.EntryType(), Label: n.Label,
			Children: convertTree(n.Children),
		}
		if base.ParentID != nil {
			node.ParentID = *base.ParentID
		}
		out = append(out, node)
	}
	return out
}

func (r *RPCServer) leafID(ctx context.Context) string {
	if r.s.Session == nil {
		return ""
	}
	id, _ := r.s.Session.LeafID(ctx)
	if id == nil {
		return ""
	}
	return *id
}

// sink renders agent events as rpc events.
func (r *RPCServer) sink() agent.Sink {
	return func(_ context.Context, ev agent.Event) error {
		switch ev.Type {
		case agent.EventAgentStart:
			r.out.EmitEvent(rpc.Event{Type: "agent_start"})
		case agent.EventAgentEnd:
			r.out.EmitEvent(rpc.Event{Type: "agent_end"})
			r.out.EmitEvent(rpc.Event{Type: "agent_settled"})
		case agent.EventTurnStart:
			r.out.EmitEvent(rpc.Event{Type: "turn_start"})
		case agent.EventTurnEnd:
			r.out.EmitEvent(rpc.Event{Type: "turn_end", Message: marshalMessage(ev.Message)})
		case agent.EventMessageStart:
			r.out.EmitEvent(rpc.Event{Type: "message_start", Message: marshalMessage(ev.Message)})
		case agent.EventMessageUpdate:
			out := rpc.Event{Type: "message_update"}
			if ev.StreamEvent != nil {
				switch ev.StreamEvent.Type {
				case ai.EventTextDelta:
					out.Delta, out.DeltaKind = ev.StreamEvent.Delta, "text"
				case ai.EventThinkingDelta:
					out.Delta, out.DeltaKind = ev.StreamEvent.Delta, "thinking"
				}
			}
			r.out.EmitEvent(out)
		case agent.EventMessageEnd:
			r.out.EmitEvent(rpc.Event{Type: "message_end", Message: marshalMessage(ev.Message)})
		case agent.EventToolExecutionStart:
			r.out.EmitEvent(rpc.Event{
				Type: "tool_execution_start", ToolCallID: ev.ToolCallID,
				ToolName: ev.ToolName, Args: ev.Args,
			})
		case agent.EventToolExecutionEnd:
			out := rpc.Event{
				Type: "tool_execution_end", ToolCallID: ev.ToolCallID,
				ToolName: ev.ToolName, IsError: ev.IsError,
			}
			if ev.Result != nil {
				if raw, err := json.Marshal(ev.Result); err == nil {
					out.Result = raw
				}
			}
			r.out.EmitEvent(out)
		}
		return nil
	}
}

// --- extension UI proxying ---

// rpcUI turns an extension's dialog into a request on the client's stream.
//
// The extension blocks on a channel while the client decides, exactly as it
// would while a TUI dialog was open. That is the property worth preserving: an
// extension asking a question should not have to know whether the answer comes
// from a terminal or from a program.
type rpcUI struct{ r *RPCServer }

func (u rpcUI) request(ctx context.Context, req rpc.ExtensionUIRequest) (rpc.ExtensionUIResponse, error) {
	if u.r.writer() == nil {
		// Asked before a client is listening. Failing is right: the extension
		// gets ErrNoUI, which is what it would get in any headless mode, and
		// it can degrade instead of blocking on an answer that cannot come.
		return rpc.ExtensionUIResponse{}, extension.ErrNoUI
	}
	id := strconv.FormatUint(u.r.nextUIID.Add(1), 10)
	req.Type, req.ID = "extension_ui_request", id

	ch := make(chan rpc.ExtensionUIResponse, 1)
	u.r.pendingMu.Lock()
	u.r.pending[id] = ch
	u.r.pendingMu.Unlock()
	defer func() {
		u.r.pendingMu.Lock()
		delete(u.r.pending, id)
		u.r.pendingMu.Unlock()
	}()

	u.r.writer().EmitUI(req)

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return rpc.ExtensionUIResponse{}, ctx.Err()
	}
}

func (r *RPCServer) deliverUI(res rpc.ExtensionUIResponse) {
	r.pendingMu.Lock()
	ch := r.pending[res.ID]
	r.pendingMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- res:
	default:
	}
}

// notify and the setters do not wait for the client. They are statements, not
// questions, and blocking the extension on an acknowledgement it has no use
// for would turn a status update into a round trip.
func (u rpcUI) emit(req rpc.ExtensionUIRequest) {
	w := u.r.writer()
	if w == nil {
		return
	}
	req.Type = "extension_ui_request"
	req.ID = strconv.FormatUint(u.r.nextUIID.Add(1), 10)
	w.EmitUI(req)
}

// writer returns the output stream, nil before Attach.
func (r *RPCServer) writer() *rpc.Writer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out
}

func (u rpcUI) Confirm(ctx context.Context, req extension.ConfirmRequest) (bool, error) {
	res, err := u.request(ctx, rpc.ExtensionUIRequest{
		Method: "confirm", Title: req.Title, Message: req.Message,
	})
	if err != nil {
		return false, err
	}
	// A dismissed confirm is "no", not a distinct outcome. The dialog asked a
	// yes/no question, the user did not say yes, and treating silence as
	// anything else would let a permission prompt be answered by ignoring it.
	if res.Cancelled {
		return false, nil
	}
	if res.Confirmed != nil {
		return *res.Confirmed, nil
	}
	return res.Value == "true" || res.Value == "yes", nil
}

func (u rpcUI) Select(ctx context.Context, req extension.SelectRequest) (int, error) {
	opts := make([]string, 0, len(req.Options))
	for _, o := range req.Options {
		opts = append(opts, o.Label)
	}
	res, err := u.request(ctx, rpc.ExtensionUIRequest{
		Method: "select", Title: req.Title, Message: req.Message, Options: opts,
	})
	if err != nil {
		return -1, err
	}
	if res.Cancelled {
		return -1, nil
	}
	for i, o := range opts {
		if o == res.Value {
			return i, nil
		}
	}
	// A client that answered with an index rather than a label is accommodated
	// rather than rejected: both readings are unambiguous, and refusing one
	// would fail a dialog the user did answer.
	if n, err := strconv.Atoi(res.Value); err == nil && n >= 0 && n < len(opts) {
		return n, nil
	}
	return -1, nil
}

func (u rpcUI) Input(ctx context.Context, req extension.InputRequest) (string, error) {
	res, err := u.request(ctx, rpc.ExtensionUIRequest{
		Method: "input", Title: req.Title, Message: req.Message,
		Placeholder: req.Placeholder, Prefill: req.Initial,
	})
	if err != nil {
		return "", err
	}
	if res.Cancelled {
		return "", extension.ErrNoUI
	}
	return res.Value, nil
}

func (u rpcUI) Notify(n extension.Notification) {
	u.emit(rpc.ExtensionUIRequest{
		Method: "notify", Title: n.Title, Message: n.Message, NotifyType: string(n.Level),
	})
}

func (u rpcUI) SetStatus(text string) {
	u.emit(rpc.ExtensionUIRequest{Method: "setStatus", StatusKey: "extension", StatusText: &text})
}

func (u rpcUI) SetTitle(title string) {
	u.emit(rpc.ExtensionUIRequest{Method: "setTitle", Title: title})
}

func (u rpcUI) SetWidget(id string, pos extension.WidgetPosition, w extension.Widget) {
	req := rpc.ExtensionUIRequest{Method: "setWidget", WidgetKey: id}
	if pos == extension.WidgetBelowEditor {
		req.WidgetPlacement = "belowEditor"
	} else {
		req.WidgetPlacement = "aboveEditor"
	}
	if w != nil {
		// A client has no width to offer, so the widget is rendered at a
		// conventional one. Asking the client first would make mounting a
		// widget a blocking round trip.
		req.WidgetLines = w.Render(rpcWidgetWidth)
	}
	u.emit(req)
}

// rpcWidgetWidth is the column count a widget is rendered at when there is no
// terminal to measure.
const rpcWidgetWidth = 80
