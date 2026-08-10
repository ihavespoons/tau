// Package tui is tau's interactive terminal interface, built on Bubble Tea.
//
// The design point that shapes everything else: tau renders inline instead of
// taking over the alternate screen. Completed messages are printed into the
// terminal's own scrollback and never touched again; only the live region —
// streaming output, the editor, and the status line — is repainted. Your
// scrollback, selection, and search keep working, and transcript length costs
// nothing to render.
package tui

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"runtime"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/keybindings"
	"github.com/ihavespoons/tau/theme"
)

// Options configures the interactive session.
type Options struct {
	// Coding carries everything coding.New needs except the UI wiring, which
	// Run supplies.
	Coding coding.Options
}

// Run starts the interactive TUI and blocks until the user exits.
//
// Construction order matters. The UI bridge exists before the coding session
// so extensions can reach the UI from inside their factory, and the Bubble Tea
// program is attached to the bridge before it starts, so no dialog can be
// requested against a program that is not listening yet.
func Run(ctx context.Context, opts Options) error {
	bridge := newUIBridge()
	h := &host{bridge: bridge}

	copts := opts.Coding
	copts.Mode = extension.ModeTUI
	copts.UI = bridge
	copts.Interactive = h

	cs, err := coding.New(ctx, copts)
	if err != nil {
		return err
	}
	h.cs = cs
	defer cs.Close(ctx, "exit")

	th, warnings := LoadTheme(cs.Settings.ThemeSetting(), theme.Options{
		Dir:   config.ThemesDir(),
		Paths: cs.Settings.ThemePaths(),
	})
	cs.Warnings = append(cs.Warnings, warnings...)

	// Keybindings are global-only, so there is nothing project-scoped to merge
	// and no trust gate to clear: a repository does not get to decide what
	// Ctrl+C does in someone else's terminal.
	km, kwarnings := keybindings.Load(config.KeybindingsPath())
	cs.Warnings = append(cs.Warnings, kwarnings...)
	h.keys = km

	a := newApp(cs, bridge, th, km)

	prog := tea.NewProgram(a)
	bridge.attach(prog)
	defer bridge.shutdown()

	pr := newPrinter(prog)
	a.printer = pr
	go pr.run()
	defer pr.stop()

	// Agent events reach the UI by being posted into the program's queue.
	// Program.Send blocks until the render loop takes the message, which is
	// exactly Pi's awaited-listener backpressure: a slow UI slows the agent
	// rather than dropping its output.
	cs.Agent.Subscribe(agentSink(prog))

	if len(cs.Agent.Messages()) > 0 {
		prog.Send(printMsg{lines: a.replayTranscript()})
	}

	_, err = prog.Run()
	return err
}

// agentSink forwards loop events into the render loop.
func agentSink(prog *tea.Program) agent.Sink {
	return func(_ context.Context, ev agent.Event) error {
		prog.Send(agentEventMsg{ev: ev})
		return nil
	}
}

// osc52 encodes a clipboard write the terminal itself performs.
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}

// writeClipboard emits the OSC 52 sequence.
func writeClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		_, _ = os.Stdout.WriteString(osc52(text))
		return nil
	}
}

// openBrowser opens a URL, ignoring failure: the URL is always printed too, so
// a headless or locked-down machine still has a way through.
func openBrowser(url string) {
	if url == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
