package slashcmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
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

	// tau's own addition. Pi sets labels from inside its tree dialog, which is
	// a per-item action tau's picker does not have; a command reaches the same
	// feature and works headless besides.
	{Name: "label", Description: "Label the current point in the session (no argument clears it)", ArgumentHint: "<text>"},
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
	// Compact summarizes older history into a checkpoint. instructions is an
	// optional focus for the summary.
	Compact(ctx context.Context, instructions string) (string, error)
	// SessionTree renders the branch structure.
	SessionTree() string
	// SetLabel bookmarks the current position. An empty label clears it.
	SetLabel(ctx context.Context, label string) (string, error)
	// ForkPoints renders the places a fork or rewind can start from.
	ForkPoints() string
	// MoveTo repositions the conversation at an entry in the tree.
	MoveTo(ctx context.Context, entryID string) (string, error)
	// ForkSession copies the session up to an entry into a new one and
	// switches to it. An empty entryID copies the whole session.
	ForkSession(ctx context.Context, entryID string) (string, error)
	// Reload restarts the extensions that run in their own processes and
	// rebuilds the dispatch runner around them.
	Reload(ctx context.Context) (string, error)
}

// Exporter is the optional surface for taking a session out of tau. It is
// separate from Host because a host backed by something other than a file on
// disk has nothing to export, and should leave the commands advertised but
// unimplemented rather than fail at the end of the work.
type Exporter interface {
	// ExportSession writes the session to path and returns the path written.
	// An empty path picks a default name; the extension chooses the format.
	ExportSession(ctx context.Context, path string) (string, error)
	// ShareSession uploads the exported session and returns the links to it.
	ShareSession(ctx context.Context) (string, error)
}

// Changelogger is the optional surface for /changelog. It is separate from
// Host because release notes belong to the binary rather than to the session:
// a program embedding tau as a library ships its own, or none.
type Changelogger interface {
	// Changelog renders the release notes for display.
	Changelog() string
}

// ModelScoper is the optional surface for /scoped-models: the set of models
// the model-cycling shortcut moves between, and the saved patterns that choose
// them. A host with nowhere to save settings does not implement it.
type ModelScoper interface {
	// ScopedModels renders the cycle set and how it was configured.
	ScopedModels() string
	// SetScopedModels saves patterns as the cycle set. No patterns clears it,
	// putting every model back in the cycle.
	SetScopedModels(ctx context.Context, patterns []string) (string, error)
}

// Importer is the optional surface for /import: adopting a session file
// written elsewhere. It sits with Exporter rather than in Host for the same
// reason — a host not backed by session files has nothing to import into.
type Importer interface {
	// ImportSession adopts the session at path and continues it, returning
	// what to tell the user.
	ImportSession(ctx context.Context, path string) (string, error)
}

// SettingsStore is the optional surface for /settings: reading the merged
// configuration and writing to it.
type SettingsStore interface {
	// SettingsList renders every configured value with its scope.
	SettingsList() string
	// SettingsGet renders one key.
	SettingsGet(key string) (string, error)
	// SettingsSet writes a key, reading value as JSON when it parses as JSON
	// and as a string when it does not.
	SettingsSet(ctx context.Context, key, value string) (string, error)
	// SettingsUnset removes a key.
	SettingsUnset(ctx context.Context, key string) (string, error)
	// SettingsKeys lists the keys worth completing.
	SettingsKeys() []string
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
	// NavigateTree opens the branch picker and moves to the choice.
	NavigateTree(ctx context.Context) (string, error)
	// SelectForkPoint opens the fork picker and forks at the choice.
	SelectForkPoint(ctx context.Context) (string, error)
	// SelectScopedModels opens the checklist of models to cycle through.
	SelectScopedModels(ctx context.Context) (string, error)
	// SelectSettings opens the settings menu.
	SelectSettings(ctx context.Context) (string, error)
}

// RegisterBuiltins adds every built-in command to the registry.
//
// Commands the host can actually serve are real; the rest are advertised with
// accurate metadata and return ErrNotImplemented when run, because they need
// subsystems from later phases. Registering them now keeps autocomplete and
// help honest and gives extensions a stable surface to override.
func RegisterBuiltins(r *Registry, host Host) {
	ui, _ := host.(Interactive)
	exp, _ := host.(Exporter)
	log, _ := host.(Changelogger)
	scoper, _ := host.(ModelScoper)
	store, _ := host.(SettingsStore)
	imp, _ := host.(Importer)

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
		case "compact":
			r.Register(New(info, compactRun(host)))
		case "label":
			r.Register(New(info, plainHostOp(host, func(ctx context.Context, args string) (string, error) {
				return host.SetLabel(ctx, strings.TrimSpace(args))
			})))
		case "tree":
			r.Register(New(info, treeRun(host, ui)))
		case "fork":
			r.Register(New(info, forkRun(host, ui)))
		case "reload":
			r.Register(New(info, plainHostOp(host, func(ctx context.Context, _ string) (string, error) {
				return host.Reload(ctx)
			})))
		case "clone":
			r.Register(New(info, sessionOp2(host, func(ctx context.Context, _ string) (string, error) {
				return host.ForkSession(ctx, "")
			})))
		case "export":
			r.Register(New(info, exportOp(exp, func(ctx context.Context, args string) (string, error) {
				path, err := exp.ExportSession(ctx, strings.TrimSpace(args))
				if err != nil {
					return "", err
				}
				return "Exported to: " + path, nil
			})))
		case "share":
			r.Register(New(info, exportOp(exp, func(ctx context.Context, _ string) (string, error) {
				return exp.ShareSession(ctx)
			})))
		case "import":
			info.ArgumentHint = "<path.jsonl>"
			if imp == nil {
				r.Register(New(info, nil))
				break
			}
			r.Register(New(info, sessionOp2(host, func(ctx context.Context, args string) (string, error) {
				return imp.ImportSession(ctx, strings.TrimSpace(args))
			})))
		case "settings":
			// Pi opens a toggle menu here, so its entry carries no hint.
			info.ArgumentHint = "[<key> [value] | unset <key>]"
			if store == nil {
				r.Register(New(info, nil))
				break
			}
			r.Register(NewWithCompleter(info, settingsRun(store, ui), settingsComplete(store)))
		case "scoped-models":
			// The hint is set here rather than in the ported table: Pi's
			// command takes no arguments because it opens a picker, and this
			// one does.
			info.ArgumentHint = "<pattern>... | all"
			if scoper == nil {
				r.Register(New(info, nil))
				break
			}
			r.Register(New(info, scopedModelsRun(scoper, ui)))
		case "changelog":
			if log == nil {
				r.Register(New(info, nil))
				break
			}
			r.Register(New(info, func(context.Context, string) (Result, error) {
				return Result{Output: log.Changelog()}, nil
			}))
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

// sessionOp2 is sessionOp for commands served by the plain Host rather than a
// UI: they replace the session, so the host has to redraw.
func sessionOp2(host Host, fn func(context.Context, string) (string, error)) func(context.Context, string) (Result, error) {
	if host == nil {
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

// plainHostOp adapts a Host method that returns text to a command.
func plainHostOp(host Host, fn func(context.Context, string) (string, error)) func(context.Context, string) (Result, error) {
	if host == nil {
		return nil
	}
	return func(ctx context.Context, args string) (Result, error) {
		out, err := fn(ctx, args)
		return Result{Output: out}, err
	}
}

func exportOp(exp Exporter, fn func(context.Context, string) (string, error)) func(context.Context, string) (Result, error) {
	if exp == nil {
		return nil
	}
	return func(ctx context.Context, args string) (Result, error) {
		out, err := fn(ctx, args)
		return Result{Output: out}, err
	}
}

// settingsRun serves /settings: no argument lists the configuration, a key
// alone reads it, a key and a value writes it, and "unset <key>" removes it.
//
// The value keeps its spaces — a JSON array or an editor command line is one
// argument even though it looks like several.
func settingsRun(store SettingsStore, ui Interactive) func(context.Context, string) (Result, error) {
	return func(ctx context.Context, args string) (Result, error) {
		args = strings.TrimSpace(args)
		if args == "" {
			// With a UI this is a menu, the way Pi has it. Headless it stays a
			// report, which is the only thing a bare /settings can mean when
			// there is nobody to ask.
			if ui != nil {
				out, err := ui.SelectSettings(ctx)
				return Result{Output: out}, err
			}
			return Result{Output: store.SettingsList()}, nil
		}

		key, value, _ := strings.Cut(args, " ")
		value = strings.TrimSpace(value)

		if strings.EqualFold(key, "unset") {
			out, err := store.SettingsUnset(ctx, value)
			return Result{Output: out}, err
		}
		if value == "" {
			out, err := store.SettingsGet(key)
			return Result{Output: out}, err
		}
		out, err := store.SettingsSet(ctx, key, value)
		return Result{Output: out}, err
	}
}

// settingsComplete offers key names, and offers them again after "unset".
func settingsComplete(store SettingsStore) func(string) []Item {
	return func(prefix string) []Item {
		lead := ""
		if rest, ok := cutFold(prefix, "unset "); ok {
			lead, prefix = "unset ", rest
		}
		var out []Item
		for _, k := range store.SettingsKeys() {
			if strings.HasPrefix(k, prefix) {
				out = append(out, Item{Value: lead + k, Label: k})
			}
		}
		return out
	}
}

// cutFold is strings.CutPrefix with a case-insensitive prefix.
func cutFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// scopedModelsRun serves /scoped-models: patterns replace the cycle set, "all"
// or "reset" clears it, and no argument opens the checklist — or reports the
// set when there is no interface to open one in.
//
// Typing patterns stays supported alongside the picker: a glob says what you
// mean in one line, and it is the only form that works headless.
func scopedModelsRun(scoper ModelScoper, ui Interactive) func(context.Context, string) (Result, error) {
	return func(ctx context.Context, args string) (Result, error) {
		args = strings.TrimSpace(args)
		if args == "" {
			if ui != nil {
				out, err := ui.SelectScopedModels(ctx)
				return Result{Output: out}, err
			}
			return Result{Output: scoper.ScopedModels()}, nil
		}
		var patterns []string
		if !strings.EqualFold(args, "all") && !strings.EqualFold(args, "reset") {
			patterns = strings.FieldsFunc(args, func(r rune) bool {
				return unicode.IsSpace(r) || r == ','
			})
		}
		out, err := scoper.SetScopedModels(ctx, patterns)
		return Result{Output: out}, err
	}
}

func compactRun(host Host) func(context.Context, string) (Result, error) {
	if host == nil {
		return nil
	}
	return func(ctx context.Context, args string) (Result, error) {
		out, err := host.Compact(ctx, strings.TrimSpace(args))
		return Result{Output: out}, err
	}
}

// treeRun serves /tree three ways: an explicit id moves there, a UI opens the
// picker, and a headless run with no argument prints the tree. The last one
// matters — a scripted session still needs to see the shape of its history.
func treeRun(host Host, ui Interactive) func(context.Context, string) (Result, error) {
	if host == nil {
		return nil
	}
	return func(ctx context.Context, args string) (Result, error) {
		if id := strings.TrimSpace(args); id != "" {
			out, err := host.MoveTo(ctx, id)
			return Result{Output: out}, err
		}
		if ui != nil {
			out, err := ui.NavigateTree(ctx)
			return Result{Output: out}, err
		}
		return Result{Output: host.SessionTree()}, nil
	}
}

func forkRun(host Host, ui Interactive) func(context.Context, string) (Result, error) {
	if host == nil {
		return nil
	}
	return func(ctx context.Context, args string) (Result, error) {
		if id := strings.TrimSpace(args); id != "" {
			out, err := host.ForkSession(ctx, id)
			if err != nil {
				return Result{}, err
			}
			return Result{Output: out, SessionChanged: true}, nil
		}
		if ui != nil {
			out, err := ui.SelectForkPoint(ctx)
			if err != nil {
				return Result{}, err
			}
			return Result{Output: out, SessionChanged: true}, nil
		}
		return Result{Output: host.ForkPoints()}, nil
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
