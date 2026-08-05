package exthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
)

// bind attaches the API the extension's inbound requests act on. It is set
// once the factory has run, which is also the earliest moment the extension
// could have sent anything: the handshake completes first, and the extension
// has no reason to ask for the model before it has been told the session
// exists.
func (h *Host) bind(api *extension.API) {
	h.apiMu.Lock()
	h.api = api
	h.apiMu.Unlock()
}

func (h *Host) boundAPI() *extension.API {
	h.apiMu.Lock()
	defer h.apiMu.Unlock()
	return h.api
}

func (h *Host) replyErr(id string, err error) {
	_ = h.w.Write(wire.Reply{Type: wire.FrameReply, ID: id, Error: err.Error()})
}

func (h *Host) replyValue(id string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		h.replyErr(id, err)
		return
	}
	_ = h.w.Write(wire.Reply{Type: wire.FrameReply, ID: id, Payload: raw})
}

func (h *Host) replyCancelled(id string) {
	_ = h.w.Write(wire.Reply{Type: wire.FrameReply, ID: id, Cancelled: true})
}

// serveUI answers a ui_request. It runs on its own goroutine because the
// dialog methods block until the user answers, and the read pump must keep
// delivering results while one is open.
func (h *Host) serveUI(req wire.UIRequest) {
	api := h.boundAPI()
	if api == nil {
		h.replyErr(req.ID, errors.New("exthost: extension is not bound to a session yet"))
		return
	}
	ui := api.UI()

	ctx := context.Background()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Millisecond)
		defer cancel()
	}

	switch req.Method {
	case "select":
		opts := make([]extension.SelectOption, 0, len(req.Options))
		for _, o := range req.Options {
			opts = append(opts, extension.SelectOption{Label: o, Value: o})
		}
		idx, err := ui.Select(ctx, extension.SelectRequest{
			Title: req.Title, Message: req.Message, Options: opts,
		})
		switch {
		case err != nil:
			h.replyErr(req.ID, err)
		case idx < 0 || idx >= len(req.Options):
			// Out of range is how a picker reports dismissal, and dismissal is
			// not a failure — the extension asked and got no answer.
			h.replyCancelled(req.ID)
		default:
			h.replyValue(req.ID, wire.UIValue{Value: req.Options[idx]})
		}

	case "confirm":
		ok, err := ui.Confirm(ctx, extension.ConfirmRequest{Title: req.Title, Message: req.Message})
		if err != nil {
			h.replyErr(req.ID, err)
			return
		}
		h.replyValue(req.ID, wire.UIConfirmed{Confirmed: ok})

	case "input", "editor":
		// An editor request is a multi-line input. tau has no external-editor
		// surface yet, so it degrades to the input dialog rather than failing:
		// an extension asking for text gets text.
		text, err := ui.Input(ctx, extension.InputRequest{
			Title: req.Title, Message: req.Message,
			Placeholder: req.Placeholder, Initial: req.Prefill,
		})
		if err != nil {
			h.replyErr(req.ID, err)
			return
		}
		h.replyValue(req.ID, wire.UIValue{Value: text})

	case "notify":
		level := extension.NotifyInfo
		switch req.NotifyType {
		case "warning":
			level = extension.NotifyWarning
		case "error":
			level = extension.NotifyError
		}
		ui.Notify(extension.Notification{Level: level, Title: req.Title, Message: req.Message})
		h.replyValue(req.ID, struct{}{})

	case "setStatus":
		text := ""
		if req.StatusText != nil {
			text = *req.StatusText
		}
		ui.SetStatus(text)
		h.replyValue(req.ID, struct{}{})

	case "setTitle":
		ui.SetTitle(req.Title)
		h.replyValue(req.ID, struct{}{})

	case "setWidget":
		pos := extension.WidgetAboveEditor
		if req.WidgetPlacement == "belowEditor" {
			pos = extension.WidgetBelowEditor
		}
		if req.WidgetLines == nil {
			ui.SetWidget(req.WidgetKey, pos, nil)
		} else {
			// The lines were produced by the extension at some width it chose.
			// Re-wrapping them here would break any box drawing they contain,
			// so they are drawn as given — the same contract an in-process
			// widget has.
			lines := append([]string(nil), req.WidgetLines...)
			ui.SetWidget(req.WidgetKey, pos, extension.WidgetFunc(func(int) []string { return lines }))
		}
		h.replyValue(req.ID, struct{}{})

	case "set_editor_text":
		// Replacing the editor's contents needs an editor. Without one this is
		// a no-op rather than an error: Pi extensions call it opportunistically.
		h.replyValue(req.ID, struct{}{})

	default:
		h.replyErr(req.ID, fmt.Errorf("exthost: unknown ui method %q", req.Method))
	}
}

// serveAction answers an action frame.
func (h *Host) serveAction(act wire.Action) {
	api := h.boundAPI()
	if api == nil {
		h.replyErr(act.ID, errors.New("exthost: extension is not bound to a session yet"))
		return
	}

	switch act.Method {
	case "sendMessage":
		var p wire.SendMessageParams
		if err := json.Unmarshal(act.Params, &p); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		msg, err := ai.UnmarshalMessage(p.Message)
		if err != nil {
			h.replyErr(act.ID, fmt.Errorf("exthost: sendMessage: %w", err))
			return
		}
		if err := api.SendMessage(msg, p.DeliverAs); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		h.replyValue(act.ID, struct{}{})

	case "exec":
		var p wire.ExecParams
		if err := json.Unmarshal(act.Params, &p); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		out, code, err := api.Exec(context.Background(), p.Command)
		if err != nil {
			h.replyErr(act.ID, err)
			return
		}
		h.replyValue(act.ID, wire.ExecResult{Output: out, ExitCode: code})

	case "setSessionName":
		var p wire.NameParams
		if err := json.Unmarshal(act.Params, &p); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		if err := api.SetSessionName(p.Name); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		h.replyValue(act.ID, struct{}{})

	case "getSessionName":
		h.replyValue(act.ID, wire.StringResult{Value: api.SessionName()})

	case "getActiveTools":
		h.replyValue(act.ID, wire.StringsResult{Values: api.ActiveToolNames()})

	case "setActiveTools":
		var p wire.ToolNamesParams
		if err := json.Unmarshal(act.Params, &p); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		if err := api.SetActiveTools(p.Names); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		h.replyValue(act.ID, struct{}{})

	case "getModel":
		m := api.Model()
		if m == nil {
			h.replyValue(act.ID, nil)
			return
		}
		h.replyValue(act.ID, wire.ModelInfo{
			Provider: string(m.Provider), ID: m.ID, Name: m.Name,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens,
			Reasoning: m.Reasoning,
		})

	case "getThinkingLevel":
		h.replyValue(act.ID, wire.StringResult{Value: string(api.ThinkingLevel())})

	case "setThinkingLevel":
		var p wire.ThinkingParams
		if err := json.Unmarshal(act.Params, &p); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		if err := api.SetThinkingLevel(ai.ModelThinkingLevel(p.Level)); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		h.replyValue(act.ID, struct{}{})

	case "registerTool":
		var p wire.RegisterToolParams
		if err := json.Unmarshal(act.Params, &p); err != nil {
			h.replyErr(act.ID, err)
			return
		}
		api.RegisterTool(h.newWireTool(p.Tool))
		h.replyValue(act.ID, struct{}{})

	default:
		h.replyErr(act.ID, fmt.Errorf("exthost: unknown action %q", act.Method))
	}
}

// deliverToolUpdate routes a streamed partial result to the call awaiting it.
func (h *Host) deliverToolUpdate(up wire.ToolUpdate) {
	h.mu.Lock()
	fn := h.toolUpdates[up.ID]
	h.mu.Unlock()
	if fn != nil {
		fn(up)
	}
}

// watchToolUpdates registers an update sink for the duration of one tool call.
func (h *Host) watchToolUpdates(id string, update agent.UpdateFunc) func() {
	if update == nil {
		return func() {}
	}
	h.mu.Lock()
	h.toolUpdates[id] = func(up wire.ToolUpdate) {
		var p wire.ToolResultPayload
		if err := json.Unmarshal(up.Partial, &p); err != nil {
			return
		}
		update(toolResultFrom(p))
	}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.toolUpdates, id)
		h.mu.Unlock()
	}
}
