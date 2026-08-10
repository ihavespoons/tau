package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/auth/oauth"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/keybindings"
)

// host implements slashcmd.Interactive: the built-in commands that need to
// ask the user something.
//
// Every method here runs on a command goroutine, never on the render loop, so
// blocking on a dialog is safe. The session pointer is filled in after
// coding.New returns — nothing calls into the host during construction.
type host struct {
	cs     *coding.Session
	bridge *uiBridge
	keys   *keybindings.Manager
}

// Hotkeys renders tau's key bindings.
//
// The keys are read from the live table rather than written out here, so a
// rebound key is listed under the name it now answers to and an action someone
// unbound is not listed at all — a help screen that lied about either would be
// worse than no help screen.
func (h *host) Hotkeys() string {
	km := h.keys
	if km == nil {
		km = fallbackKeys
	}

	// all lists every key an action answers to. pair takes one key from each of
	// two actions, for the rows that describe both halves of a motion and would
	// otherwise read as a pile of alternatives.
	all := func(id keybindings.Binding) string {
		var out []string
		for _, k := range km.Keys(id) {
			out = append(out, prettyKey(k))
		}
		return strings.Join(out, " / ")
	}
	first := func(id keybindings.Binding) string {
		if keys := km.Keys(id); len(keys) > 0 {
			return prettyKey(keys[0])
		}
		return ""
	}
	pair := func(a, b keybindings.Binding) string {
		l, r := first(a), first(b)
		if l == "" || r == "" {
			return l + r
		}
		return l + " / " + r
	}

	rows := [][2]string{
		{all(keybindings.InputSubmit), "send the message"},
		{all(keybindings.InputNewLine), "insert a newline"},
		{all(keybindings.AppMessageFollowUp), "queue a follow-up, or send when nothing is running"},
		{all(keybindings.AppInterrupt), "stop the agent (in-flight tools still finish)"},
		{all(keybindings.AppClear), "clear the input, then quit on a second press"},
		{all(keybindings.AppExit), "quit when the input is empty"},
		{pair(keybindings.AppModelCycleForward, keybindings.AppModelCycleBackward), "next / previous model"},
		{all(keybindings.AppThinkingCycle), "cycle the thinking level"},
		{all(keybindings.AppThinkingToggle), "show or hide thinking blocks"},
		{all(keybindings.AppSuspend), "suspend tau"},
		{all(keybindings.InputTab), "accept the highlighted command completion"},
		{pair(keybindings.EditorCursorUp, keybindings.EditorCursorDown), "move a line, or step through prompt history"},
		{pair(keybindings.EditorCursorLineStart, keybindings.EditorCursorLineEnd), "start / end of line"},
		{all(keybindings.EditorDeleteWordBackward), "delete the previous word"},
		{pair(keybindings.EditorDeleteToLineStart, keybindings.EditorDeleteToLineEnd), "kill to start / end of line"},
	}

	width := 0
	for _, r := range rows {
		if r[0] != "" {
			width = max(width, displayWidth(r[0]))
		}
	}
	var b strings.Builder
	for _, r := range rows {
		if r[0] == "" {
			continue
		}
		// Padded by display width rather than %-*s: an arrow key is one column
		// on screen and three bytes in the string.
		fmt.Fprintf(&b, "%s%s  %s\n", r[0], strings.Repeat(" ", width-displayWidth(r[0])), r[1])
	}
	return strings.TrimRight(b.String(), "\n")
}

// SelectModel opens the model picker.
func (h *host) SelectModel(ctx context.Context) (string, error) {
	all := h.cs.AvailableModels()
	if len(all) == 0 {
		return "", errors.New("no models are configured")
	}

	opts := make([]extension.SelectOption, 0, len(all))
	initial := 0
	for i := range all {
		m := all[i]
		id := m.Provider + "/" + m.ID
		if ai.ModelsEqual(&m, h.cs.Model) {
			initial = i
		}
		opts = append(opts, extension.SelectOption{
			Label:       id,
			Description: fmt.Sprintf("%dk ctx · $%.2f/$%.2f per Mtok", m.ContextWindow/1000, m.Cost.Input, m.Cost.Output),
			Value:       id,
		})
	}

	idx, err := h.bridge.Select(ctx, extension.SelectRequest{
		Title: "Select model", Options: opts, Initial: initial, Filterable: true,
	})
	if err != nil || idx < 0 {
		return "", err
	}
	m, err := h.cs.SetModel(ctx, opts[idx].Value)
	if err != nil {
		return "", err
	}
	return "Switched to " + m.Provider + "/" + m.ID, nil
}

// NewSession starts a fresh session in the same directory.
func (h *host) NewSession(ctx context.Context) (string, error) {
	if err := h.cs.StartSession(ctx); err != nil {
		return "", err
	}
	return "Started a new session: " + h.cs.Path, nil
}

// ResumeSession opens the session picker.
func (h *host) ResumeSession(ctx context.Context) (string, error) {
	metas, err := h.cs.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	if len(metas) == 0 {
		return "", errors.New("no previous sessions in this directory")
	}

	opts := make([]extension.SelectOption, 0, len(metas))
	for _, m := range metas {
		label := m.CreatedAt
		if label == "" {
			label = filepath.Base(m.Path)
		}
		desc := filepath.Base(m.Path)
		if m.Path == h.cs.Path {
			desc += "  (current)"
		}
		opts = append(opts, extension.SelectOption{Label: label, Description: desc, Value: m.Path})
	}

	idx, err := h.bridge.Select(ctx, extension.SelectRequest{
		Title: "Resume session", Options: opts, Filterable: true,
	})
	if err != nil || idx < 0 {
		return "", err
	}
	if err := h.cs.SwitchSession(ctx, metas[idx]); err != nil {
		return "", err
	}
	return "Resumed " + h.cs.Path, nil
}

// NavigateTree opens the branch picker and moves to the choice.
//
// The offer is user messages, not every entry: a branch point is a request the
// user made, and offering the assistant's replies as destinations would ask
// them to pick a place in the middle of an answer.
func (h *host) NavigateTree(ctx context.Context) (string, error) {
	points, err := h.cs.UserPrompts(ctx)
	if err != nil {
		return "", err
	}
	if len(points) == 0 {
		return "", errors.New("this session has no history to navigate")
	}

	opts := make([]extension.SelectOption, 0, len(points))
	for _, p := range points {
		opts = append(opts, extension.SelectOption{
			Label: p.Text, Description: p.EntryID, Value: p.EntryID,
		})
	}
	idx, err := h.bridge.Select(ctx, extension.SelectRequest{
		Title:      "Go back to",
		Message:    "The conversation continues from here. Later branches stay in the file.",
		Options:    opts,
		Initial:    len(opts) - 1,
		Filterable: true,
	})
	if err != nil || idx < 0 {
		return "", err
	}

	// Summarizing costs a request, so it is asked rather than assumed — unless
	// settings already answered.
	summarize := false
	if h.cs.Settings == nil || !h.cs.Settings.BranchSummarySkipPrompt() {
		choice, err := h.bridge.Select(ctx, extension.SelectRequest{
			Title:   "Summarize the branch you are leaving?",
			Message: "Costs one request. Without it, the abandoned work drops out of context.",
			Options: []extension.SelectOption{
				{Label: "Summarize it", Value: "yes"},
				{Label: "Just move", Value: "no"},
			},
		})
		if err != nil || choice < 0 {
			return "", err
		}
		summarize = choice == 0
	}

	result, err := h.cs.MoveTo(ctx, opts[idx].Value, summarize)
	if err != nil {
		return "", err
	}
	if result != nil {
		return "Moved back, and summarized the branch left behind.", nil
	}
	return "Moved back.", nil
}

// SelectForkPoint opens the fork picker and forks at the choice.
func (h *host) SelectForkPoint(ctx context.Context) (string, error) {
	points, err := h.cs.UserPrompts(ctx)
	if err != nil {
		return "", err
	}
	if len(points) == 0 {
		return "", errors.New("this session has no user messages to fork from")
	}

	opts := make([]extension.SelectOption, 0, len(points))
	for _, p := range points {
		opts = append(opts, extension.SelectOption{
			Label: p.Text, Description: p.EntryID, Value: p.EntryID,
		})
	}
	idx, err := h.bridge.Select(ctx, extension.SelectRequest{
		Title:      "Fork from",
		Message:    "Copies this session up to just before the chosen message. The original is untouched.",
		Options:    opts,
		Initial:    len(opts) - 1,
		Filterable: true,
	})
	if err != nil || idx < 0 {
		return "", err
	}
	if err := h.cs.Fork(ctx, opts[idx].Value); err != nil {
		return "", err
	}
	return "Forked to " + h.cs.Path, nil
}

// SetTrust records a project-trust decision, asking when none was given.
func (h *host) SetTrust(ctx context.Context, args string) (string, error) {
	decision := strings.TrimSpace(strings.ToLower(args))
	if decision == "" {
		idx, err := h.bridge.Select(ctx, extension.SelectRequest{
			Title:   "Trust " + h.cs.Cwd + "?",
			Message: "Trusted directories may load .tau settings, skills, and extensions.",
			Options: []extension.SelectOption{
				{Label: "Always trust this directory", Value: "always"},
				{Label: "Never trust this directory", Value: "never"},
				{Label: "Ask every time", Value: "ask"},
			},
			Initial: 0,
		})
		if err != nil || idx < 0 {
			return "", err
		}
		decision = []string{"always", "never", "ask"}[idx]
	}
	return h.cs.SaveTrust(ctx, decision)
}

// CopyLast copies the last assistant message to the clipboard.
func (h *host) CopyLast(ctx context.Context) (string, error) {
	text := h.cs.LastAssistantText()
	if text == "" {
		return "", errors.New("no assistant message to copy")
	}
	if err := h.bridge.Copy(text); err != nil {
		return "", err
	}
	return fmt.Sprintf("Copied %d characters to the clipboard", len(text)), nil
}

// Login authenticates a provider.
func (h *host) Login(ctx context.Context, providerName string) (string, error) {
	id := strings.TrimSpace(providerName)
	if id == "" {
		id = "anthropic"
	}
	if id != "anthropic" {
		return "", fmt.Errorf("tau can only log in to anthropic so far (asked for %q)", id)
	}

	store := auth.NewFileStore(config.AuthPath())
	in := &uiInteraction{bridge: h.bridge}

	idx, err := h.bridge.Select(ctx, extension.SelectRequest{
		Title: "Anthropic login",
		Options: []extension.SelectOption{
			{Label: "Claude Pro/Max (OAuth)", Description: "opens your browser", Value: "oauth"},
			{Label: "API key", Description: "paste a key from console.anthropic.com", Value: "key"},
		},
	})
	if err != nil || idx < 0 {
		return "", err
	}

	if idx == 1 {
		key, err := h.bridge.Input(ctx, extension.InputRequest{
			Title: "Anthropic API key", Placeholder: "sk-ant-…", Secret: true,
		})
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(key) == "" {
			return "", errors.New("no key entered")
		}
		if _, err := store.Modify(ctx, "anthropic", func(*auth.Credential) (*auth.Credential, error) {
			return &auth.Credential{Type: auth.CredentialAPIKey, Key: strings.TrimSpace(key)}, nil
		}); err != nil {
			return "", err
		}
		return "Saved API key to " + config.AuthPath(), nil
	}

	if err := oauth.Login(ctx, oauth.NewAnthropic(), store, "anthropic", in); err != nil {
		return "", err
	}
	return "Logged in. Credentials saved to " + config.AuthPath(), nil
}

// Logout removes stored credentials.
func (h *host) Logout(ctx context.Context, providerName string) (string, error) {
	id := strings.TrimSpace(providerName)
	if id == "" {
		id = "anthropic"
	}
	if err := auth.NewFileStore(config.AuthPath()).Delete(ctx, id); err != nil {
		return "", err
	}
	return "Removed stored " + id + " credentials", nil
}

// uiInteraction adapts the login flows' prompt/notify contract to dialogs.
type uiInteraction struct{ bridge *uiBridge }

var _ auth.Interaction = (*uiInteraction)(nil)

func (u *uiInteraction) Prompt(ctx context.Context, p auth.Prompt) (string, error) {
	// A flow may cancel an individual prompt when an out-of-band event
	// resolves the step — a pasted code racing the callback server, say — so
	// the prompt's own context has to be honored alongside the caller's.
	if p.Ctx != nil {
		merged, cancel := mergeContexts(ctx, p.Ctx)
		defer cancel()
		ctx = merged
	}

	if p.Type == auth.PromptSelect {
		opts := make([]extension.SelectOption, 0, len(p.Options))
		for _, o := range p.Options {
			opts = append(opts, extension.SelectOption{
				Label: o.Label, Description: o.Description, Value: o.ID,
			})
		}
		idx, err := u.bridge.Select(ctx, extension.SelectRequest{Title: p.Message, Options: opts})
		if err != nil {
			return "", err
		}
		if idx < 0 {
			return "", errors.New("cancelled")
		}
		return p.Options[idx].ID, nil
	}

	return u.bridge.Input(ctx, extension.InputRequest{
		Title:       "Sign in",
		Message:     p.Message,
		Placeholder: p.Placeholder,
		Secret:      p.Type == auth.PromptSecret,
	})
}

func (u *uiInteraction) Notify(ev auth.Event) {
	switch ev.Type {
	case auth.EventAuthURL:
		u.bridge.print([]string{ev.Message, "", "  " + ev.URL, "", ev.Instructions})
		openBrowser(ev.URL)
	case auth.EventDeviceCode:
		u.bridge.print([]string{
			ev.Message,
			fmt.Sprintf("  enter code %s at %s", ev.UserCode, ev.VerificationURI),
		})
		openBrowser(ev.VerificationURI)
	default:
		lines := []string{ev.Message}
		for _, l := range ev.Links {
			lines = append(lines, "  "+l.Label+" "+l.URL)
		}
		u.bridge.print(lines)
	}
}

// mergeContexts returns a context cancelled when either parent is.
func mergeContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}
