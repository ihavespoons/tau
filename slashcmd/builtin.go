package slashcmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Builtin describes a built-in command. Names and descriptions are ported
// verbatim from Pi's BUILTIN_SLASH_COMMANDS (slash-commands.ts:19-42), so
// muscle memory and docs carry over.
type Builtin struct {
	Name         string
	Description  string
	ArgumentHint string
}

// Builtins is Pi's built-in command list.
var Builtins = []Builtin{
	{Name: "settings", Description: "Open settings menu"},
	{Name: "model", Description: "Select model (opens selector UI)", ArgumentHint: "<provider/model>"},
	{Name: "scoped-models", Description: "Enable/disable models for Ctrl+P cycling"},
	{Name: "export", Description: "Export session (HTML default, or specify path: .html/.jsonl)"},
	{Name: "import", Description: "Import and resume a session from a JSONL file"},
	{Name: "share", Description: "Share session as a secret GitHub gist"},
	{Name: "copy", Description: "Copy last agent message to clipboard"},
	{Name: "name", Description: "Set session display name"},
	{Name: "session", Description: "Show session info and stats"},
	{Name: "changelog", Description: "Show changelog entries"},
	{Name: "hotkeys", Description: "Show all keyboard shortcuts"},
	{Name: "fork", Description: "Create a new fork from a previous user message"},
	{Name: "clone", Description: "Duplicate the current session at the current position"},
	{Name: "tree", Description: "Navigate session tree (switch branches)"},
	{Name: "trust", Description: "Save project trust decision for future sessions"},
	{Name: "login", Description: "Configure provider authentication", ArgumentHint: "<provider>"},
	{Name: "logout", Description: "Remove provider authentication"},
	{Name: "new", Description: "Start a new session"},
	{Name: "compact", Description: "Manually compact the session context"},
	{Name: "resume", Description: "Resume a different session"},
	{Name: "reload", Description: "Reload keybindings, extensions, skills, prompts, themes, and context files"},
	{Name: "quit", Description: "Quit tau"},
}

// Host is what built-in commands act on. The coding layer implements it; a
// nil Host still yields a registry with correct metadata, which is all the
// autocomplete and help surfaces need.
type Host interface {
	// Models lists selectable model ids.
	Models() []string
	// CurrentModel returns the active model id.
	CurrentModel() string
	// SetModel switches models.
	SetModel(id string) error
	// SetSessionName sets the session's display name.
	SetSessionName(name string) error
	// SessionInfo renders a human-readable session summary.
	SessionInfo() string
}

// Interactive is the extra surface a host with a UI provides. Built-in
// commands that must ask the user something are wired only when the host
// implements it; a headless host leaves them advertised but unimplemented.
//
// Every method returns the text to show the user. Implementations block while
// a dialog is open, so they must never be called from a render goroutine.
type Interactive interface {
	// SelectModel opens the model picker.
	SelectModel(ctx context.Context) (string, error)
	// NewSession starts a fresh session in the same directory.
	NewSession(ctx context.Context) (string, error)
	// ResumeSession opens the session picker and switches to the choice.
	ResumeSession(ctx context.Context) (string, error)
	// Login runs provider authentication.
	Login(ctx context.Context, provider string) (string, error)
	// Logout removes stored credentials.
	Logout(ctx context.Context, provider string) (string, error)
	// SetTrust records a project-trust decision.
	SetTrust(ctx context.Context, args string) (string, error)
	// CopyLast copies the last assistant message to the clipboard.
	CopyLast(ctx context.Context) (string, error)
	// Hotkeys renders the host's key bindings.
	Hotkeys() string
}

// RegisterBuiltins adds every built-in command to the registry.
//
// Commands the host can actually serve are real; the rest are advertised with
// accurate metadata and return ErrNotImplemented when run, because they need
// subsystems from later phases. Registering them now keeps autocomplete and
// help honest and gives extensions a stable surface to override.
func RegisterBuiltins(r *Registry, host Host) {
	ui, _ := host.(Interactive)

	for _, b := range Builtins {
		info := Info{
			Name:         b.Name,
			Description:  b.Description,
			ArgumentHint: b.ArgumentHint,
			Source:       SourceBuiltin,
			SourceInfo:   "builtin",
		}

		switch b.Name {
		case "model":
			r.Register(NewWithCompleter(info, modelRun(host, ui), modelComplete(host)))
		case "name":
			r.Register(New(info, nameRun(host)))
		case "session":
			r.Register(New(info, sessionRun(host)))
		case "quit":
			r.Register(New(info, func(context.Context, string) (Result, error) {
				return Result{Quit: true}, nil
			}))
		case "hotkeys":
			r.Register(New(info, func(context.Context, string) (Result, error) {
				if ui != nil {
					return Result{Output: ui.Hotkeys()}, nil
				}
				return Result{Output: hotkeysText}, nil
			}))
		case "new":
			r.Register(New(info, sessionOp(ui, func(ctx context.Context, _ string) (string, error) {
				return ui.NewSession(ctx)
			})))
		case "resume":
			r.Register(New(info, sessionOp(ui, func(ctx context.Context, _ string) (string, error) {
				return ui.ResumeSession(ctx)
			})))
		case "login":
			r.Register(New(info, plainOp(ui, func(ctx context.Context, args string) (string, error) {
				return ui.Login(ctx, args)
			})))
		case "logout":
			r.Register(New(info, plainOp(ui, func(ctx context.Context, args string) (string, error) {
				return ui.Logout(ctx, args)
			})))
		case "trust":
			r.Register(New(info, plainOp(ui, func(ctx context.Context, args string) (string, error) {
				return ui.SetTrust(ctx, args)
			})))
		case "copy":
			r.Register(New(info, plainOp(ui, func(ctx context.Context, _ string) (string, error) {
				return ui.CopyLast(ctx)
			})))
		default:
			r.Register(New(info, nil)) // advertised; ErrNotImplemented when run
		}
	}

	// /help is tau's own addition — Pi surfaces this through its TUI rather
	// than a command, but a headless CLI needs a way to list commands.
	r.Register(New(Info{
		Name:        "help",
		Description: "List available commands",
		Source:      SourceBuiltin,
		SourceInfo:  "builtin",
	}, func(context.Context, string) (Result, error) {
		return Result{Output: renderHelp(r)}, nil
	}))
}

const hotkeysText = `Keyboard shortcuts are available in interactive mode.`

// plainOp adapts an Interactive method to a command, or yields
// ErrNotImplemented when the host has no UI.
func plainOp(ui Interactive, fn func(context.Context, string) (string, error)) func(context.Context, string) (Result, error) {
	if ui == nil {
		return nil
	}
	return func(ctx context.Context, args string) (Result, error) {
		out, err := fn(ctx, args)
		return Result{Output: out}, err
	}
}

// sessionOp is plainOp for commands that replace the session.
func sessionOp(ui Interactive, fn func(context.Context, string) (string, error)) func(context.Context, string) (Result, error) {
	if ui == nil {
		return nil
	}
	return func(ctx context.Context, args string) (Result, error) {
		out, err := fn(ctx, args)
		if err != nil {
			return Result{}, err
		}
		return Result{Output: out, SessionChanged: true}, nil
	}
}

func renderHelp(r *Registry) string {
	infos := r.List()
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	width := 0
	for _, i := range infos {
		if n := len(i.Name) + len(i.ArgumentHint); n > width {
			width = n
		}
	}
	var b strings.Builder
	for _, i := range infos {
		name := "/" + i.Name
		if i.ArgumentHint != "" {
			name += " " + i.ArgumentHint
		}
		fmt.Fprintf(&b, "%-*s  %s\n", width+2, name, i.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

func modelRun(host Host, ui Interactive) func(context.Context, string) (Result, error) {
	return func(ctx context.Context, args string) (Result, error) {
		if host == nil {
			return Result{}, fmt.Errorf("/model: %w", ErrNotImplemented)
		}
		arg := strings.TrimSpace(args)
		// With a UI and no argument, Pi opens the selector. Without one, the
		// same command lists the models so it stays useful headless.
		if arg == "" && ui != nil {
			out, err := ui.SelectModel(ctx)
			return Result{Output: out}, err
		}
		if arg == "" {
			var b strings.Builder
			current := host.CurrentModel()
			for _, m := range host.Models() {
				marker := "  "
				if m == current {
					marker = "* "
				}
				b.WriteString(marker + m + "\n")
			}
			return Result{Output: strings.TrimRight(b.String(), "\n")}, nil
		}
		if err := host.SetModel(arg); err != nil {
			return Result{}, err
		}
		return Result{Output: "Switched to " + arg}, nil
	}
}

func modelComplete(host Host) func(string) []Item {
	return func(prefix string) []Item {
		if host == nil {
			return nil
		}
		var out []Item
		for _, m := range host.Models() {
			if strings.HasPrefix(m, prefix) {
				out = append(out, Item{Value: m, Label: m})
			}
		}
		return out
	}
}

func nameRun(host Host) func(context.Context, string) (Result, error) {
	return func(_ context.Context, args string) (Result, error) {
		if host == nil {
			return Result{}, fmt.Errorf("/name: %w", ErrNotImplemented)
		}
		name := strings.TrimSpace(args)
		if name == "" {
			return Result{}, fmt.Errorf("/name requires a name")
		}
		if err := host.SetSessionName(name); err != nil {
			return Result{}, err
		}
		return Result{Output: "Session named " + name}, nil
	}
}

func sessionRun(host Host) func(context.Context, string) (Result, error) {
	return func(context.Context, string) (Result, error) {
		if host == nil {
			return Result{}, fmt.Errorf("/session: %w", ErrNotImplemented)
		}
		return Result{Output: host.SessionInfo()}, nil
	}
}
