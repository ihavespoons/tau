package exthost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
	"github.com/ihavespoons/tau/session"
)

// Extension presents the running subprocess as an ordinary extension.
//
// Everything the rest of tau touches — the composition policies, the error
// collection, the tool registry, the staleness guard — is the parent package's
// code, unchanged. The only thing this adds is where the answers come from.
func (h *Host) Extension() extension.Extension {
	return extension.Extension{
		Name:    h.Name(),
		Path:    h.spec.Path,
		Factory: h.factory,
	}
}

func (h *Host) factory(api *extension.API) error {
	h.bind(api)

	for _, d := range h.decl.Tools {
		api.RegisterTool(h.newWireTool(d))
	}
	for _, d := range h.decl.Commands {
		h.registerCommand(api, d)
	}
	for _, d := range h.decl.Shortcuts {
		h.registerShortcut(api, d)
	}
	for _, d := range h.decl.Renderers {
		h.registerRenderer(api, d)
	}
	for _, d := range h.decl.Flags {
		api.RegisterFlag(extension.Flag{
			Name: d.Name, Description: d.Description, Type: d.Type, Default: d.Default,
		})
	}

	for _, name := range h.decl.Subscriptions {
		if err := h.subscribe(api, extension.EventType(name)); err != nil {
			return err
		}
	}
	return nil
}

// emit sends one event and returns the extension's raw result.
//
// A hot event does not wait: it is handed to the conflator and the handler
// returns. Everything else is a synchronous round trip, matching the in-process
// contract that a handler runs to completion before the agent proceeds.
//
// A nil return is "no opinion", which is not the same as the zero value. Most
// composition policies distinguish them, and collapsing the two would turn
// silence into a decision.
func (h *Host) emit(ctx context.Context, ev extension.EventType, payload any) (json.RawMessage, error) {
	if h.suspend.Load() {
		return nil, ErrSuspended
	}

	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("exthost: %s: marshal %s: %w", h.Name(), ev, err)
		}
		raw = b
	}

	gen := h.generation.Load()

	if hotEvents[ev] {
		h.hot.send(wire.Event{
			Type: wire.FrameEvent, ID: h.nextRequestID(), Event: string(ev),
			Generation: gen, Payload: raw, NoReply: true,
		})
		return nil, nil
	}

	id := h.nextRequestID()
	res, err := h.request(ctx, id, wire.Event{
		Type: wire.FrameEvent, ID: id, Event: string(ev),
		Generation: gen, Payload: raw,
	})
	if err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("exthost: %s: %s", h.Name(), res.Error)
	}
	// A result that outlived its session is discarded. The decision it carries
	// was made about messages, a model, and a working directory that have all
	// been replaced since, and applying it to what is there now would be worse
	// than having no answer at all.
	if h.generation.Load() != gen {
		return nil, nil
	}
	if len(res.Payload) == 0 || string(res.Payload) == "null" {
		return nil, nil
	}
	return res.Payload, nil
}

// decoded emits an event and decodes the answer, preserving the difference
// between "no opinion" (nil) and an answer that happens to be empty.
func decoded[T any](h *Host, ctx context.Context, ev extension.EventType, payload any) (*T, error) {
	raw, err := h.emit(ctx, ev, payload)
	if err != nil || raw == nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("exthost: %s: decode %s result: %w", h.Name(), ev, err)
	}
	return &out, nil
}

// notify is the shape of a handler for an event whose result is ignored.
func notify(h *Host, ev extension.EventType) func(context.Context, any) error {
	return func(ctx context.Context, payload any) error {
		_, err := h.emit(ctx, ev, payload)
		return err
	}
}

// subscribe registers the forwarding handler for one event type.
//
// The long switch is the price of typed handlers, and it is worth paying: it
// is what makes the subprocess path go through the same Runner methods, with
// the same signatures, as a Go extension. A generic dispatcher would need its
// own composition rules.
func (h *Host) subscribe(api *extension.API, ev extension.EventType) error {
	switch ev {

	// --- notification-only ---

	case extension.EventSessionStart:
		fn := notify(h, ev)
		api.OnSessionStart(func(ctx context.Context, e *extension.SessionStartEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventSessionInfoChanged:
		fn := notify(h, ev)
		api.OnSessionInfoChanged(func(ctx context.Context, e *extension.SessionInfoChangedEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventSessionCompact:
		fn := notify(h, ev)
		api.OnSessionCompact(func(ctx context.Context, e *extension.SessionCompactEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventSessionTree:
		fn := notify(h, ev)
		api.OnSessionTree(func(ctx context.Context, e *extension.SessionTreeEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventSessionShutdown:
		fn := notify(h, ev)
		api.OnSessionShutdown(func(ctx context.Context, e *extension.SessionShutdownEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventAgentStart:
		fn := notify(h, ev)
		api.OnAgentStart(func(ctx context.Context, e *extension.AgentStartEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventAgentEnd:
		fn := notify(h, ev)
		api.OnAgentEnd(func(ctx context.Context, e *extension.AgentEndEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventAgentSettled:
		fn := notify(h, ev)
		api.OnAgentSettled(func(ctx context.Context, e *extension.AgentSettledEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventTurnStart:
		fn := notify(h, ev)
		api.OnTurnStart(func(ctx context.Context, e *extension.TurnStartEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventTurnEnd:
		fn := notify(h, ev)
		api.OnTurnEnd(func(ctx context.Context, e *extension.TurnEndEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventMessageStart:
		fn := notify(h, ev)
		api.OnMessageStart(func(ctx context.Context, e *extension.MessageStartEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventMessageUpdate:
		fn := notify(h, ev)
		api.OnMessageUpdate(func(ctx context.Context, e *extension.MessageUpdateEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventToolExecutionStart:
		fn := notify(h, ev)
		api.OnToolExecutionStart(func(ctx context.Context, e *extension.ToolExecutionStartEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventToolExecutionUpdate:
		fn := notify(h, ev)
		api.OnToolExecutionUpdate(func(ctx context.Context, e *extension.ToolExecutionUpdateEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventToolExecutionEnd:
		fn := notify(h, ev)
		api.OnToolExecutionEnd(func(ctx context.Context, e *extension.ToolExecutionEndEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventModelSelect:
		fn := notify(h, ev)
		api.OnModelSelect(func(ctx context.Context, e *extension.ModelSelectEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventThinkingLevelSelect:
		fn := notify(h, ev)
		api.OnThinkingLevelSelect(func(ctx context.Context, e *extension.ThinkingLevelSelectEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})
	case extension.EventAfterProviderResponse:
		fn := notify(h, ev)
		api.OnAfterProviderResponse(func(ctx context.Context, e *extension.AfterProviderResponseEvent, _ *extension.Context) error {
			return fn(ctx, e)
		})

	// --- result-bearing ---

	case extension.EventProjectTrust:
		api.OnProjectTrust(func(ctx context.Context, e *extension.ProjectTrustEvent, _ *extension.Context) (*extension.ProjectTrustResult, error) {
			return decoded[extension.ProjectTrustResult](h, ctx, ev, e)
		})
	case extension.EventResourcesDiscover:
		api.OnResourcesDiscover(func(ctx context.Context, e *extension.ResourcesDiscoverEvent, _ *extension.Context) (*extension.ResourcesDiscoverResult, error) {
			return decoded[extension.ResourcesDiscoverResult](h, ctx, ev, e)
		})
	case extension.EventSessionBeforeSwitch:
		api.OnSessionBeforeSwitch(func(ctx context.Context, e *extension.SessionBeforeSwitchEvent, _ *extension.Context) (*extension.SessionBeforeResult, error) {
			return decoded[extension.SessionBeforeResult](h, ctx, ev, e)
		})
	case extension.EventSessionBeforeFork:
		api.OnSessionBeforeFork(func(ctx context.Context, e *extension.SessionBeforeForkEvent, _ *extension.Context) (*extension.SessionBeforeResult, error) {
			return decoded[extension.SessionBeforeResult](h, ctx, ev, e)
		})
	case extension.EventSessionBeforeCompact:
		api.OnSessionBeforeCompact(func(ctx context.Context, e *extension.SessionBeforeCompactEvent, _ *extension.Context) (*extension.SessionBeforeResult, error) {
			return decoded[extension.SessionBeforeResult](h, ctx, ev, e)
		})
	case extension.EventSessionBeforeTree:
		api.OnSessionBeforeTree(func(ctx context.Context, e *extension.SessionBeforeTreeEvent, _ *extension.Context) (*extension.SessionBeforeResult, error) {
			return decoded[extension.SessionBeforeResult](h, ctx, ev, e)
		})
	case extension.EventContext:
		api.OnContext(func(ctx context.Context, e *extension.ContextEvent, _ *extension.Context) (*extension.ContextResult, error) {
			return decoded[extension.ContextResult](h, ctx, ev, e)
		})
	case extension.EventBeforeProviderRequest:
		api.OnBeforeProviderRequest(func(ctx context.Context, e *extension.BeforeProviderRequestEvent, _ *extension.Context) (*extension.BeforeProviderRequestResult, error) {
			return decoded[extension.BeforeProviderRequestResult](h, ctx, ev, e)
		})
	case extension.EventBeforeAgentStart:
		api.OnBeforeAgentStart(func(ctx context.Context, e *extension.BeforeAgentStartEvent, _ *extension.Context) (*extension.BeforeAgentStartResult, error) {
			return decoded[extension.BeforeAgentStartResult](h, ctx, ev, e)
		})
	case extension.EventMessageEnd:
		api.OnMessageEnd(func(ctx context.Context, e *extension.MessageEndEvent, _ *extension.Context) (*extension.MessageEndResult, error) {
			return decoded[extension.MessageEndResult](h, ctx, ev, e)
		})
	case extension.EventToolResult:
		api.OnToolResult(func(ctx context.Context, e *extension.ToolResultEvent, _ *extension.Context) (*extension.ToolResultResult, error) {
			return decoded[extension.ToolResultResult](h, ctx, ev, e)
		})
	case extension.EventUserBash:
		api.OnUserBash(func(ctx context.Context, e *extension.UserBashEvent, _ *extension.Context) (*extension.UserBashResult, error) {
			return decoded[extension.UserBashResult](h, ctx, ev, e)
		})
	case extension.EventInput:
		api.OnInput(func(ctx context.Context, e *extension.InputEvent, _ *extension.Context) (*extension.InputResult, error) {
			return decoded[extension.InputResult](h, ctx, ev, e)
		})

	// --- special cases ---

	case extension.EventBeforeProviderHeaders:
		// Headers are mutated in place in process. Over a wire there is no
		// shared map, so the extension returns the whole set and the host
		// applies the difference: a key it omitted is left alone, a key mapped
		// to null is deleted.
		api.OnBeforeProviderHeaders(func(ctx context.Context, e *extension.BeforeProviderHeadersEvent, _ *extension.Context) error {
			res, err := decoded[extension.BeforeProviderHeadersEvent](h, ctx, ev, e)
			if err != nil || res == nil {
				return err
			}
			for k, v := range res.Headers {
				e.Headers[k] = v
			}
			return nil
		})

	case extension.EventToolCall:
		// The one event where a failure is not merely reported. A gate that
		// cannot answer must not be read as consent, so a transport failure,
		// a timeout, a crash, and a suspended extension all block the call.
		api.OnToolCall(func(ctx context.Context, e *extension.ToolCallEvent, _ *extension.Context) (*extension.ToolCallResult, error) {
			raw, err := h.emit(ctx, ev, e)
			if err != nil {
				return &extension.ToolCallResult{
					Block:  true,
					Reason: fmt.Sprintf("extension %s could not be consulted: %v", h.Name(), err),
				}, nil
			}
			var out toolCallReply
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &out); err != nil {
					return &extension.ToolCallResult{
						Block:  true,
						Reason: fmt.Sprintf("extension %s returned an undecodable tool_call result: %v", h.Name(), err),
					}, nil
				}
			}
			// Argument edits travel back explicitly. The in-process handler
			// writes into the shared map; a subprocess cannot, so it names the
			// arguments it wants instead.
			if out.Args != nil {
				for k := range e.Args {
					delete(e.Args, k)
				}
				for k, v := range out.Args {
					e.Args[k] = v
				}
			}
			if out.Result == nil {
				return nil, nil
			}
			return out.Result, nil
		})

	default:
		return fmt.Errorf("exthost: %s subscribed to unknown event %q", h.Name(), ev)
	}
	return nil
}

// toolCallReply is tool_call's wire result: the block decision plus any
// argument rewrite.
type toolCallReply struct {
	Result *extension.ToolCallResult `json:"result,omitempty"`
	Args   map[string]any            `json:"args,omitempty"`
}

func (h *Host) registerCommand(api *extension.API, d wire.CommandDecl) {
	cmd := extension.Command{
		Name:        d.Name,
		Description: d.Description,
		Handler: func(ctx context.Context, args string, _ *extension.CommandContext) error {
			id := h.nextRequestID()
			res, err := h.request(ctx, id, wire.Command{
				Type: wire.FrameCommand, ID: id, Generation: h.generation.Load(),
				Name: d.Name, Args: args,
			})
			if err != nil {
				return err
			}
			if res.Error != "" {
				return fmt.Errorf("%s", res.Error)
			}
			return nil
		},
	}
	if d.Completions {
		cmd.ArgumentCompletions = func(prefix string) []extension.CompletionItem {
			// Completions run while the user is typing. A subprocess that is
			// slow to answer must not make the editor feel stuck, so this has
			// its own short deadline and gives up quietly.
			ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
			defer cancel()

			id := h.nextRequestID()
			res, err := h.request(ctx, id, wire.Completions{
				Type: wire.FrameCompletions, ID: id, Name: d.Name, Prefix: prefix,
			})
			if err != nil || res.Error != "" {
				return nil
			}
			var out wire.CompletionsResult
			if err := json.Unmarshal(res.Payload, &out); err != nil {
				return nil
			}
			items := make([]extension.CompletionItem, 0, len(out.Items))
			for _, i := range out.Items {
				items = append(items, extension.CompletionItem{
					Value: i.Value, Label: i.Label, Description: i.Description,
				})
			}
			return items
		}
	}
	api.RegisterCommand(cmd)
}

func (h *Host) registerShortcut(api *extension.API, d wire.ShortcutDecl) {
	api.RegisterShortcut(extension.Shortcut{
		Key: d.Key, Description: d.Description,
		Handler: func(ctx context.Context, _ *extension.Context) error {
			id := h.nextRequestID()
			res, err := h.request(ctx, id, wire.Shortcut{
				Type: wire.FrameShortcut, ID: id, Generation: h.generation.Load(), Key: d.Key,
			})
			if err != nil {
				return err
			}
			if res.Error != "" {
				return fmt.Errorf("%s", res.Error)
			}
			return nil
		},
	})
}

// registerRenderer forwards a declared renderer into the in-process registry,
// so the transcript consults subprocess and Go renderers through the same
// lookup and neither can be reached by a path the other cannot.
//
// The selector is re-derived from the value being drawn rather than taken from
// the declaration: a renderer that claims everything ("" selector) still has
// to tell the extension which role or entry type it was handed.
func (h *Host) registerRenderer(api *extension.API, d wire.RendererDecl) {
	switch d.Kind {
	case "message":
		api.RegisterMessageRenderer(extension.MessageRenderer{
			Role: d.Selector,
			Render: func(ctx context.Context, m ai.Message, width int) ([]string, error) {
				return h.Render(ctx, "message", m.Role(), width, m)
			},
		})
	case "entry":
		api.RegisterEntryRenderer(extension.EntryRenderer{
			EntryType: d.Selector,
			Render: func(ctx context.Context, e session.Entry, width int) ([]string, error) {
				return h.Render(ctx, "entry", e.EntryType(), width, e)
			},
		})
	}
}

// Render asks a declared renderer for its lines. An empty result means the
// extension declined and the host's own rendering applies.
func (h *Host) Render(ctx context.Context, kind, selector string, width int, payload any) ([]string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	// A renderer runs on the draw path. Waiting the full request timeout for
	// one would freeze the transcript, so it gets the short deadline and the
	// built-in rendering takes over when it is missed.
	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()

	id := h.nextRequestID()
	res, err := h.request(ctx, id, wire.Render{
		Type: wire.FrameRender, ID: id, Kind: kind, Selector: selector, Width: width, Payload: raw,
	})
	if err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	var out wire.RenderResult
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		return nil, err
	}
	return out.Lines, nil
}

// Renders reports whether the extension declared a renderer of this kind for
// this selector.
func (h *Host) Renders(kind, selector string) bool {
	for _, r := range h.decl.Renderers {
		if r.Kind != kind {
			continue
		}
		if r.Selector == "" || r.Selector == selector {
			return true
		}
	}
	return false
}
