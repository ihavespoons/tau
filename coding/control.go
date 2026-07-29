package coding

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/models"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/slashcmd"
	"github.com/ihavespoons/tau/trust"
)

// ErrRunning is returned by operations that cannot run while the agent is
// mid-turn.
var ErrRunning = errors.New("the agent is still working — press Esc to stop it first")

// --- model selection ---

// AvailableModels lists every model the registry knows, provider-qualified.
func (s *Session) AvailableModels() []ai.Model { return s.Models.Models() }

// CycleModels is the ordered set Ctrl+P cycles through: the models named by
// the enabledModels setting, or everything when the setting is empty.
//
// Pi treats an unmatched pattern as a warning rather than an error, so a
// setting that names a model from an unconfigured provider still leaves a
// usable cycle set.
func (s *Session) CycleModels() []ai.Model {
	patterns := s.Settings.EnabledModels()
	if len(patterns) == 0 {
		return s.Models.Models()
	}
	matches, _ := s.Models.Scoped(patterns)
	out := make([]ai.Model, 0, len(matches))
	for _, m := range matches {
		if m.Model != nil {
			out = append(out, *m.Model)
		}
	}
	if len(out) == 0 {
		return s.Models.Models()
	}
	return out
}

// SetModel switches the model for subsequent turns, records the change in the
// session, and notifies extensions.
func (s *Session) SetModel(ctx context.Context, spec string) (*ai.Model, error) {
	match, err := s.Models.Resolve(spec)
	if err != nil {
		return nil, err
	}
	s.applyModel(ctx, match.Model)
	if match.ThinkingLevel != "" {
		s.SetThinkingLevel(ctx, ai.ModelThinkingLevel(match.ThinkingLevel))
	}
	return match.Model, nil
}

// applyModel installs a model everywhere it is observed.
func (s *Session) applyModel(ctx context.Context, m *ai.Model) {
	s.Model = m
	s.Agent.SetModel(m)

	// The new model may not support the level in force; clamping here means
	// the UI shows what will actually be requested.
	if clamped := ai.ClampThinkingLevel(m, s.Agent.ThinkingLevel()); clamped != s.Agent.ThinkingLevel() {
		s.Agent.SetThinkingLevel(clamped)
	}

	if s.Session != nil {
		_, _ = s.Session.AppendModelChange(ctx, m.Provider, m.ID)
	}
	if s.Extensions != nil {
		s.Extensions.EmitModelSelect(ctx, &extension.ModelSelectEvent{Model: m})
	}
}

// CycleModel steps through the cycle set by delta, wrapping at both ends.
func (s *Session) CycleModel(ctx context.Context, delta int) *ai.Model {
	set := s.CycleModels()
	if len(set) == 0 {
		return s.Model
	}
	idx := 0
	for i := range set {
		if ai.ModelsEqual(&set[i], s.Model) {
			idx = i
			break
		}
	}
	next := set[((idx+delta)%len(set)+len(set))%len(set)]
	s.applyModel(ctx, &next)
	return s.Model
}

// SetThinkingLevel changes the reasoning level, clamped to what the model
// supports, and records it in the session.
func (s *Session) SetThinkingLevel(ctx context.Context, level ai.ModelThinkingLevel) ai.ModelThinkingLevel {
	level = ai.ClampThinkingLevel(s.Model, level)
	s.Agent.SetThinkingLevel(level)
	if s.Session != nil {
		_, _ = s.Session.AppendThinkingLevelChange(ctx, string(level))
	}
	if s.Extensions != nil {
		s.Extensions.EmitThinkingLevelSelect(ctx, &extension.ThinkingLevelSelectEvent{Level: level})
	}
	return level
}

// CycleThinkingLevel steps to the next level this model supports.
func (s *Session) CycleThinkingLevel(ctx context.Context, delta int) ai.ModelThinkingLevel {
	levels := ai.SupportedThinkingLevels(s.Model)
	if len(levels) < 2 {
		return s.Agent.ThinkingLevel()
	}
	cur := s.Agent.ThinkingLevel()
	idx := 0
	for i, l := range levels {
		if l == cur {
			idx = i
			break
		}
	}
	return s.SetThinkingLevel(ctx, levels[((idx+delta)%len(levels)+len(levels))%len(levels)])
}

// ThinkingLevel reports the reasoning level in force.
func (s *Session) ThinkingLevel() ai.ModelThinkingLevel { return s.Agent.ThinkingLevel() }

// --- session lifecycle ---

// ListSessions returns this directory's sessions, most recent first.
func (s *Session) ListSessions(ctx context.Context) ([]session.Metadata, error) {
	if s.repo == nil {
		return nil, nil
	}
	return s.repo.List(ctx, s.Cwd)
}

// StartSession replaces the current session with a new empty one.
func (s *Session) StartSession(ctx context.Context) error {
	return s.replaceSession(ctx, func() (*session.Session, session.Metadata, error) {
		sess, err := s.repo.Create(ctx, session.CreateSessionOptions{Cwd: s.Cwd})
		if err != nil {
			return nil, session.Metadata{}, err
		}
		meta, _ := sess.Metadata(ctx)
		return sess, meta, nil
	}, "")
}

// SwitchSession opens an existing session file and adopts its transcript.
func (s *Session) SwitchSession(ctx context.Context, meta session.Metadata) error {
	return s.replaceSession(ctx, func() (*session.Session, session.Metadata, error) {
		sess, err := s.repo.Open(ctx, meta)
		return sess, meta, err
	}, meta.Path)
}

// replaceSession is the shared body of /new and /resume.
//
// Order matters: extensions get a veto before anything is torn down, the old
// session is shut down, every previously issued extension context is
// invalidated, and only then does the new session announce itself.
func (s *Session) replaceSession(ctx context.Context, open func() (*session.Session, session.Metadata, error), targetPath string) error {
	if s.repo == nil {
		return errors.New("this session does not persist, so it cannot be replaced")
	}
	if s.Agent.IsRunning() {
		return ErrRunning
	}

	if s.Extensions != nil {
		if res := s.Extensions.EmitSessionBeforeSwitch(ctx, &extension.SessionBeforeSwitchEvent{
			TargetPath: targetPath,
		}); res != nil && res.Cancel {
			reason := res.Reason
			if reason == "" {
				reason = "cancelled by an extension"
			}
			return errors.New(reason)
		}
		s.Extensions.EmitSessionShutdown(ctx, &extension.SessionShutdownEvent{Reason: "switch"})
	}

	sess, meta, err := open()
	if err != nil {
		return err
	}

	var restored []ai.Message
	if sctx, berr := sess.BuildContext(ctx); berr == nil {
		restored = session.ConvertToLLM(sctx.Messages)
	} else {
		return berr
	}

	s.Session = sess
	s.Path = meta.Path
	s.sessionID = meta.ID
	s.Agent.SetMessages(restored)

	if s.Extensions != nil {
		// Handlers holding a context from the previous session must fail
		// loudly rather than mutate the new one.
		s.Extensions.Invalidate()
		s.Extensions.EmitSessionStart(ctx, &extension.SessionStartEvent{
			SessionPath: s.Path, Cwd: s.Cwd, Resumed: len(restored) > 0,
		})
	}
	return nil
}

// RestoredMessages returns the transcript the session was opened with, so a
// host can replay it into its display.
func (s *Session) RestoredMessages() []ai.Message { return s.Agent.Messages() }

// LastAssistantText returns the text of the most recent assistant message.
func (s *Session) LastAssistantText() string {
	msgs := s.Agent.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		am, ok := msgs[i].(ai.AssistantMessage)
		if !ok {
			continue
		}
		var b strings.Builder
		for _, c := range am.Content {
			if t, ok := c.(ai.TextContent); ok {
				b.WriteString(t.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

// SaveTrust records a project-trust decision for future sessions.
func (s *Session) SaveTrust(ctx context.Context, decision string) (string, error) {
	store := trust.NewStore(agentDir())
	var value *bool
	switch decision {
	case "always", "yes", "trust":
		yes := true
		value = &yes
	case "never", "no":
		no := false
		value = &no
	case "ask", "forget", "":
		value = nil
	default:
		return "", fmt.Errorf("unknown trust decision %q (use always, never, or ask)", decision)
	}
	if err := store.Set(ctx, s.Cwd, value); err != nil {
		return "", err
	}
	switch {
	case value == nil:
		return "Cleared the saved trust decision for " + s.Cwd, nil
	case *value:
		return "Trusting " + s.Cwd + " from now on (restart tau to load its resources)", nil
	default:
		return "Never trusting " + s.Cwd, nil
	}
}

// --- slash commands ---

// buildCommands assembles the registry: built-ins first so an extension
// command with the same name is suffixed rather than shadowing it.
func (s *Session) buildCommands() *slashcmd.Registry {
	reg := slashcmd.NewRegistry()

	var host slashcmd.Host = codingHost{s}
	if s.opts.Interactive != nil {
		host = hostWithUI{codingHost{s}, s.opts.Interactive}
	}
	slashcmd.RegisterBuiltins(reg, host)

	if s.Extensions != nil {
		for _, c := range s.Extensions.Commands() {
			reg.Register(extensionCommand{s: s, cmd: c})
		}
	}
	return reg
}

// RunCommand parses and executes a slash-command line.
//
// It must not be called from a host's render goroutine: a command may open a
// dialog and block until the user answers.
func (s *Session) RunCommand(ctx context.Context, line string) (slashcmd.Result, error) {
	parsed, ok := slashcmd.Parse(line)
	if !ok {
		return slashcmd.Result{}, fmt.Errorf("not a command: %q", line)
	}
	cmd, found := s.Commands.Lookup(parsed.Name)
	if !found {
		return slashcmd.Result{}, fmt.Errorf("unknown command /%s — try /help", parsed.Name)
	}
	return cmd.Run(ctx, parsed.Args)
}

// extensionCommand adapts an extension-registered command to the registry.
type extensionCommand struct {
	s   *Session
	cmd extension.Command
}

func (c extensionCommand) Info() slashcmd.Info {
	return slashcmd.Info{
		Name:        c.cmd.Name,
		Description: c.cmd.Description,
		Source:      slashcmd.SourceExtension,
		SourceInfo:  "extension",
	}
}

func (c extensionCommand) Run(ctx context.Context, args string) (slashcmd.Result, error) {
	if c.cmd.Handler == nil {
		return slashcmd.Result{}, fmt.Errorf("/%s: %w", c.cmd.Name, slashcmd.ErrNotImplemented)
	}
	cc := c.s.Extensions.NewCommandContext()
	if err := c.cmd.Handler(ctx, args, cc); err != nil {
		return slashcmd.Result{}, err
	}
	return slashcmd.Result{}, nil
}

func (c extensionCommand) Complete(prefix string) []slashcmd.Item {
	if c.cmd.ArgumentCompletions == nil {
		return nil
	}
	var out []slashcmd.Item
	for _, i := range c.cmd.ArgumentCompletions(prefix) {
		label := i.Label
		if label == "" {
			label = i.Value
		}
		out = append(out, slashcmd.Item{Value: i.Value, Label: label})
	}
	return out
}

// codingHost serves the built-in commands that need no UI.
type codingHost struct{ s *Session }

func (h codingHost) Models() []string {
	ms := h.s.AvailableModels()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Provider+"/"+m.ID)
	}
	sort.Strings(out)
	return out
}

func (h codingHost) CurrentModel() string {
	if h.s.Model == nil {
		return ""
	}
	return h.s.Model.Provider + "/" + h.s.Model.ID
}

func (h codingHost) SetModel(id string) error {
	_, err := h.s.SetModel(context.Background(), id)
	return err
}

func (h codingHost) SetSessionName(name string) error {
	if h.s.Session == nil {
		return errors.New("this session is not persisted, so it cannot be named")
	}
	if _, err := h.s.Session.AppendName(context.Background(), name); err != nil {
		return err
	}
	if h.s.Extensions != nil {
		h.s.Extensions.EmitSessionInfoChanged(context.Background(),
			&extension.SessionInfoChangedEvent{Name: name})
	}
	return nil
}

func (h codingHost) SessionInfo() string { return h.s.SessionSummary() }

// hostWithUI adds the interactive command surface supplied by the host.
type hostWithUI struct {
	codingHost
	slashcmd.Interactive
}

// SessionSummary renders the /session report.
func (s *Session) SessionSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model    %s/%s\n", s.Model.Provider, s.Model.ID)
	if lvl := s.Agent.ThinkingLevel(); lvl != "" {
		fmt.Fprintf(&b, "thinking %s\n", lvl)
	}
	fmt.Fprintf(&b, "cwd      %s\n", s.Cwd)
	if s.Path != "" {
		fmt.Fprintf(&b, "session  %s\n", s.Path)
	}
	if s.Session != nil {
		if name, ok := s.Session.Name(context.Background()); ok && name != "" {
			fmt.Fprintf(&b, "name     %s\n", name)
		}
	}
	if !s.Trust.Trusted {
		fmt.Fprintf(&b, "trust    project resources not loaded (%s)\n", s.Trust.Reason)
	}
	fmt.Fprintf(&b, "usage    %s\n", FormatUsage(s.Usage()))
	fmt.Fprintf(&b, "tools    %s", strings.Join(s.ToolNames(), ", "))
	return b.String()
}

// ToolsByName exposes the full registered tool set for activation UIs.
func (s *Session) ToolsByName() map[string]agent.Tool {
	out := map[string]agent.Tool{}
	for _, t := range s.allTools {
		out[t.Def().Name] = t
	}
	return out
}

// ResolveModelSpec is a lookup that does not switch models, for previewing a
// selection.
func (s *Session) ResolveModelSpec(spec string) (models.Match, error) { return s.Models.Resolve(spec) }
