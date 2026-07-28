package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/slashcmd"
)

// Messages produced inside the TUI.
type (
	agentEventMsg struct{ ev agent.Event }
	agentDoneMsg  struct{ err error }
	tickMsg       struct{}
	commandMsg    struct {
		res slashcmd.Result
		err error
	}
	printMsg struct{ lines []string }
)

// liveTool is a tool call currently executing.
type liveTool struct {
	name    string
	args    map[string]any
	started time.Time
	partial *agent.ToolResult
}

// widgetEntry is one extension-mounted widget.
type widgetEntry struct {
	pos extension.WidgetPosition
	w   extension.Widget
}

// spinnerFrames is a braille spinner: one cell wide in every terminal that
// can draw it, and it degrades to a visible glyph in those that cannot.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// app is the root Bubble Tea model.
//
// tau runs inline rather than in the alternate screen: finished messages are
// printed into the terminal's own scrollback with tea.Println and never
// rendered again, so scrolling, selection, and copy all work the way they do
// for any other command — and a ten-thousand-message session costs the same
// to render as an empty one.
type app struct {
	cs      *coding.Session
	bridge  *uiBridge
	theme   Theme
	rend    *renderer
	ed      *editor
	printer *printer

	width, height int

	running    bool
	streamText strings.Builder
	streamThnk strings.Builder
	liveTools  map[string]*liveTool
	liveOrder  []string
	spinner    int

	dialogs     dialogStack
	extStatus   string
	widgets     map[string]*widgetEntry
	widgetOrder []string
	notice      string

	// completions holds slash-command suggestions for the current input.
	completions []slashcmd.Info
	completeIdx int

	// ctrlCArmed is set by a first Ctrl+C on an empty prompt; a second one
	// quits. Requiring two makes an accidental interrupt survivable.
	ctrlCArmed bool
	busy       bool
	quitting   bool
}

func newApp(cs *coding.Session, bridge *uiBridge, theme Theme) *app {
	a := &app{
		cs: cs, bridge: bridge, theme: theme,
		ed:        newEditor(theme),
		rend:      newRenderer(theme, 80, cs.Settings.HideThinkingBlock()),
		liveTools: map[string]*liveTool{},
		widgets:   map[string]*widgetEntry{},
		width:     80, height: 24,
	}
	return a
}

// Init implements tea.Model.
func (a *app) Init() tea.Cmd {
	a.emit(a.banner())
	return tea.EnableBracketedPaste
}

// banner is the greeting: what you are talking to, and where.
func (a *app) banner() []string {
	t := a.theme
	lines := []string{
		t.Bold.Render("tau") + t.Dim.Render("  "+a.cs.Model.Provider+"/"+a.cs.Model.ID),
		t.Dim.Render("  " + a.cs.Cwd),
	}
	if !a.cs.Trust.Trusted {
		lines = append(lines, t.Warning.Render("  project resources not loaded: "+a.cs.Trust.Reason))
	}
	if a.cs.Extensions != nil {
		if errs := a.cs.Extensions.Errors(); len(errs) > 0 {
			for _, e := range errs {
				lines = append(lines, t.Error.Render("  "+e.Error()))
			}
		}
	}
	lines = append(lines, t.Dim.Render("  /help for commands · Esc stops the agent · Ctrl+C twice to quit"), "")
	return lines
}

// Update implements tea.Model. Everything here runs on the render goroutine,
// so it must never block: work that can wait goes out as a tea.Cmd.
func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.ed.SetWidth(msg.Width)
		a.rend.setWidth(msg.Width)
		a.bridge.setListRows(clampRows(msg.Height))
		return a, nil

	case tea.KeyMsg:
		return a, a.onKey(msg)

	case tickMsg:
		a.spinner = (a.spinner + 1) % len(spinnerFrames)
		if a.running || a.busy {
			return a, tickCmd()
		}
		return a, nil

	case agentEventMsg:
		return a, a.onAgentEvent(msg.ev)

	case agentDoneMsg:
		a.running = false
		if msg.err != nil && !isCancel(msg.err) {
			a.emit(a.rend.notice(msg.err.Error(), a.theme.Error.Render))
		}
		return a, nil

	case commandMsg:
		return a, a.onCommandDone(msg)

	case printMsg:
		a.emit(msg.lines)
		return a, nil

	case openDialogMsg:
		a.dialogs.push(msg.d)
		return a, nil

	case cancelDialogMsg:
		a.dialogs.cancel(msg.d)
		return a, nil

	case notifyMsg:
		a.emit(a.renderNotification(msg.n))
		return a, nil

	case statusMsg:
		a.extStatus = msg.text
		return a, nil

	case titleMsg:
		return a, tea.SetWindowTitle(msg.title)

	case widgetMsg:
		a.setWidget(msg)
		return a, nil

	case clipboardMsg:
		return a, writeClipboard(msg.text)
	}
	return a, nil
}

func clampRows(height int) int { return max(3, min(12, height-8)) }

func tickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// emit flushes lines into scrollback, in order.
func (a *app) emit(lines []string) {
	if a.printer != nil {
		a.printer.push(lines)
	}
}

func (a *app) renderNotification(n extension.Notification) []string {
	style := a.theme.Dim.Render
	switch n.Level {
	case extension.NotifyError:
		style = a.theme.Error.Render
	case extension.NotifyWarning:
		style = a.theme.Warning.Render
	}
	text := n.Message
	if n.Title != "" {
		text = n.Title + ": " + text
	}
	return a.rend.notice(text, style)
}

func (a *app) setWidget(msg widgetMsg) {
	if msg.w == nil {
		delete(a.widgets, msg.id)
		for i, id := range a.widgetOrder {
			if id == msg.id {
				a.widgetOrder = append(a.widgetOrder[:i], a.widgetOrder[i+1:]...)
				break
			}
		}
		return
	}
	if _, exists := a.widgets[msg.id]; !exists {
		a.widgetOrder = append(a.widgetOrder, msg.id)
	}
	a.widgets[msg.id] = &widgetEntry{pos: msg.pos, w: msg.w}
}

// --- keys ---

func (a *app) onKey(msg tea.KeyMsg) tea.Cmd {
	// A dialog owns the keyboard while it is open.
	if a.dialogs.key(msg) {
		return nil
	}

	a.notice = ""
	if msg.Type != tea.KeyCtrlC {
		a.ctrlCArmed = false
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		return a.onInterrupt()

	case tea.KeyCtrlD:
		if a.ed.Empty() && !a.running {
			a.quitting = true
			return tea.Quit
		}
		return nil

	case tea.KeyEsc:
		if a.running {
			res := a.cs.Agent.Abort()
			if res.ClearedSteer+res.ClearedFollowUp > 0 {
				a.notice = fmt.Sprintf("stopping — discarded %d queued message(s)",
					res.ClearedSteer+res.ClearedFollowUp)
			} else {
				a.notice = "stopping…"
			}
		}
		return nil

	case tea.KeyCtrlP:
		return a.cycleModel(1)

	case tea.KeyCtrlT:
		lvl := a.cs.CycleThinkingLevel(context.Background(), 1)
		a.notice = "thinking: " + string(lvl)
		return nil

	case tea.KeyTab:
		if len(a.completions) > 0 {
			a.acceptCompletion()
			return nil
		}
	}

	submitted, ok := a.ed.Update(msg)
	a.refreshCompletions()
	if !ok {
		return nil
	}
	return a.submit(submitted)
}

// onInterrupt implements the two-press quit: the first press clears a draft or
// stops the agent, the second on an idle empty prompt exits.
func (a *app) onInterrupt() tea.Cmd {
	switch {
	case !a.ed.Empty():
		a.ed.Reset()
		a.refreshCompletions()
		return nil
	case a.running:
		a.cs.Agent.Abort()
		a.notice = "stopping…"
		return nil
	case a.ctrlCArmed:
		a.quitting = true
		return tea.Quit
	default:
		a.ctrlCArmed = true
		a.notice = "press Ctrl+C again to quit"
		return nil
	}
}

func (a *app) cycleModel(delta int) tea.Cmd {
	m := a.cs.CycleModel(context.Background(), delta)
	if m != nil {
		a.notice = "model: " + m.Provider + "/" + m.ID
	}
	return nil
}

// --- submission ---

func (a *app) submit(text string) tea.Cmd {
	a.ed.Remember(text)
	a.ed.Reset()
	a.completions = nil

	if _, isCmd := slashcmd.Parse(text); isCmd {
		return a.runCommand(text)
	}

	a.emit(append(a.rend.user(text), ""))

	// Typing while the agent works is steering, not a new turn: the message
	// joins the conversation at the next turn boundary and in-flight tools
	// still finish.
	if a.running {
		a.cs.Agent.Steer(ai.UserMessage{
			Content:   ai.UserContent{Text: text},
			Timestamp: time.Now().UnixMilli(),
		})
		a.notice = "steering — will be delivered at the next turn"
		return nil
	}

	a.running = true
	ctx, cancel := context.WithCancel(context.Background())
	return tea.Batch(tickCmd(), func() tea.Msg {
		defer cancel()
		_, err := a.cs.Prompt(ctx, text)
		return agentDoneMsg{err: err}
	})
}

// runCommand executes a slash command off the render goroutine, because a
// command may open a dialog and park until the user answers.
func (a *app) runCommand(line string) tea.Cmd {
	a.busy = true
	a.emit([]string{a.theme.Dim.Render(line)})
	return tea.Batch(tickCmd(), func() tea.Msg {
		res, err := a.cs.RunCommand(context.Background(), line)
		return commandMsg{res: res, err: err}
	})
}

func (a *app) onCommandDone(msg commandMsg) tea.Cmd {
	a.busy = false
	if msg.err != nil {
		a.emit(a.rend.notice(msg.err.Error(), a.theme.Error.Render))
		return nil
	}

	if msg.res.Output != "" {
		a.emit(append(wrapBlock(msg.res.Output, a.width), ""))
	}
	if msg.res.SessionChanged {
		a.rend = newRenderer(a.theme, a.width, a.cs.Settings.HideThinkingBlock())
		a.emit(a.replayTranscript())
	}
	if msg.res.Quit {
		a.quitting = true
		return tea.Quit
	}
	if msg.res.Prompt != "" {
		return a.submit(msg.res.Prompt)
	}
	return nil
}

// replayTranscript re-prints a restored conversation after /new or /resume, so
// the scrollback matches the context the model actually has.
func (a *app) replayTranscript() []string {
	var out []string
	for _, m := range a.cs.RestoredMessages() {
		switch msg := m.(type) {
		case ai.UserMessage:
			out = append(out, a.rend.user(msg.Content.String())...)
		case ai.AssistantMessage:
			out = append(out, a.rend.assistant(msg)...)
		}
	}
	if len(out) == 0 {
		return []string{a.theme.Dim.Render("— new session —"), ""}
	}
	return append(out, "")
}

// --- agent events ---

func (a *app) onAgentEvent(ev agent.Event) tea.Cmd {
	switch ev.Type {
	case agent.EventMessageStart:
		a.streamText.Reset()
		a.streamThnk.Reset()

	case agent.EventMessageUpdate:
		if ev.StreamEvent == nil {
			return nil
		}
		switch ev.StreamEvent.Type {
		case ai.EventTextDelta:
			a.streamText.WriteString(ev.StreamEvent.Delta)
		case ai.EventThinkingDelta:
			a.streamThnk.WriteString(ev.StreamEvent.Delta)
		}

	case agent.EventMessageEnd:
		a.streamText.Reset()
		a.streamThnk.Reset()
		if m, ok := ev.Message.(ai.AssistantMessage); ok {
			if lines := a.rend.assistant(m); len(lines) > 0 {
				a.emit(append(lines, ""))
			}
		}

	case agent.EventToolExecutionStart:
		a.liveTools[ev.ToolCallID] = &liveTool{
			name: ev.ToolName, args: ev.Args, started: time.Now(),
		}
		a.liveOrder = append(a.liveOrder, ev.ToolCallID)

	case agent.EventToolExecutionUpdate:
		if lt, ok := a.liveTools[ev.ToolCallID]; ok {
			lt.partial = ev.PartialResult
		}

	case agent.EventToolExecutionEnd:
		lt := a.liveTools[ev.ToolCallID]
		a.dropLiveTool(ev.ToolCallID)
		args := ev.Args
		if lt != nil {
			args = lt.args
		}
		lines := []string{a.rend.toolCall(ev.ToolName, args)}
		lines = append(lines, a.rend.toolResult(ev.Result, ev.IsError)...)
		a.emit(append(lines, ""))
	}
	return nil
}

func (a *app) dropLiveTool(id string) {
	delete(a.liveTools, id)
	for i, existing := range a.liveOrder {
		if existing == id {
			a.liveOrder = append(a.liveOrder[:i], a.liveOrder[i+1:]...)
			return
		}
	}
}

// --- view ---

// View implements tea.Model. It draws only the live region: streaming output,
// widgets, the editor, and the status line. Everything settled is already in
// the terminal's scrollback.
func (a *app) View() string {
	if a.quitting {
		return ""
	}

	var out []string
	out = append(out, a.liveView()...)
	out = append(out, a.widgetView(extension.WidgetAboveEditor)...)

	if top := a.dialogs.top(); top != nil {
		out = append(out, a.dialogView(top)...)
	} else {
		out = append(out, a.ed.View(true))
		out = append(out, a.completionView()...)
	}

	out = append(out, a.widgetView(extension.WidgetBelowEditor)...)
	out = append(out, a.statusView())
	if a.notice != "" {
		out = append(out, a.theme.Dim.Render(truncateCells(a.notice, a.width)))
	}
	return strings.Join(out, "\n")
}

// liveView renders in-flight output, clipped so a long stream cannot push the
// editor off the screen. Nothing is lost: the full message is printed to
// scrollback when it completes.
func (a *app) liveView() []string {
	budget := a.height - 8 - len(a.widgets)
	if budget < 2 {
		budget = 2
	}

	var out []string
	if thinking := a.streamThnk.String(); thinking != "" {
		lines := wrapBlock(thinking, a.width-2)
		for _, l := range lastLines(lines, budget/2, "…") {
			out = append(out, a.theme.Thinking.Render("  "+l))
		}
	}
	if text := a.streamText.String(); text != "" {
		out = append(out, lastLines(wrapBlock(text, a.width), budget-len(out), "…")...)
	}

	for _, id := range a.liveOrder {
		lt := a.liveTools[id]
		if lt == nil {
			continue
		}
		line := spinnerFrames[a.spinner] + " " + a.rend.toolCall(lt.name, lt.args)
		line += a.theme.Dim.Render(fmt.Sprintf("  %.1fs", time.Since(lt.started).Seconds()))
		out = append(out, truncateCells(line, a.width))
	}

	if len(out) > 0 {
		out = append(out, "")
	}
	return out
}

func (a *app) widgetView(pos extension.WidgetPosition) []string {
	var out []string
	for _, id := range a.widgetOrder {
		e := a.widgets[id]
		if e == nil || e.pos != pos {
			continue
		}
		out = append(out, renderWidget(e.w, a.width)...)
	}
	return out
}

// renderWidget calls into extension code from the render goroutine, so it is
// wrapped: a widget that panics must not take the session down with it.
func renderWidget(w extension.Widget, width int) (lines []string) {
	defer func() {
		if rec := recover(); rec != nil {
			lines = []string{fmt.Sprintf("widget panicked: %v", rec)}
		}
	}()
	return w.Render(width)
}

func (a *app) dialogView(d dialog) []string {
	body := d.view(a.width-4, a.theme)
	var inner []string
	if t := d.title(); t != "" {
		inner = append(inner, a.theme.Bold.Render(t))
	}
	inner = append(inner, body...)
	inner = append(inner, a.theme.Dim.Render("enter to confirm · esc to cancel"))
	return strings.Split(a.theme.DialogBox.Width(a.width-4).Render(strings.Join(inner, "\n")), "\n")
}

func (a *app) completionView() []string {
	if len(a.completions) == 0 {
		return nil
	}
	var out []string
	for i, c := range a.completions {
		row := "/" + c.Name
		if c.ArgumentHint != "" {
			row += " " + c.ArgumentHint
		}
		row += a.theme.Dim.Render("  " + c.Description)
		if i == a.completeIdx {
			out = append(out, a.theme.Selected.Render("▸ ")+truncateCells(row, a.width-2))
		} else {
			out = append(out, "  "+truncateCells(row, a.width-2))
		}
	}
	return out
}

// statusView is the persistent footer: what model is answering, what it has
// cost so far, and what the agent is doing.
func (a *app) statusView() string {
	t := a.theme
	parts := []string{a.cs.Model.Provider + "/" + a.cs.Model.ID}
	if lvl := a.cs.ThinkingLevel(); lvl != "" && lvl != ai.ThinkingOff {
		parts = append(parts, "thinking:"+string(lvl))
	}
	if u := a.cs.Usage(); u.TotalTokens > 0 || u.Cost.Total > 0 {
		parts = append(parts, fmt.Sprintf("%s tok", humanCount(u.Input+u.Output)))
		parts = append(parts, fmt.Sprintf("$%.4f", u.Cost.Total))
	}
	if a.extStatus != "" {
		parts = append(parts, a.extStatus)
	}
	left := t.Status.Render(strings.Join(parts, " · "))

	var right string
	switch {
	case a.running:
		right = t.Accent.Render(spinnerFrames[a.spinner] + " working · esc to stop")
	case a.busy:
		right = t.Accent.Render(spinnerFrames[a.spinner] + " running command")
	}
	if right == "" {
		return truncateCells(left, a.width)
	}

	gap := a.width - displayWidth(left) - displayWidth(right)
	if gap < 1 {
		return truncateCells(left+" "+right, a.width)
	}
	return left + strings.Repeat(" ", gap) + right
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}

// --- completions ---

// refreshCompletions offers slash commands as soon as the line starts with a
// slash and has no argument yet.
func (a *app) refreshCompletions() {
	a.completions = nil
	a.completeIdx = 0

	text := a.ed.Value()
	if !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \n") {
		return
	}
	prefix := strings.ToLower(text[1:])
	for _, info := range a.cs.Commands.List() {
		if strings.HasPrefix(strings.ToLower(info.Name), prefix) {
			a.completions = append(a.completions, info)
		}
		if len(a.completions) == 8 {
			break
		}
	}
}

func (a *app) acceptCompletion() {
	if a.completeIdx >= len(a.completions) {
		return
	}
	a.ed.SetValue("/" + a.completions[a.completeIdx].Name + " ")
	a.completions = nil
}

func isCancel(err error) bool {
	return err != nil && (strings.Contains(err.Error(), context.Canceled.Error()))
}
