package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/extension"
)

// extApp builds the minimum app the extension wiring touches: a session whose
// only populated field is the runner, and a printer that records what would
// reach the scrollback.
func extApp(t *testing.T, exts ...extension.Extension) (*app, *recorder) {
	t.Helper()
	r := extension.NewRunner(extension.RunnerOptions{Mode: extension.ModeTUI, Cwd: ".", Trusted: true})
	for _, e := range exts {
		if err := r.Load(e); err != nil {
			t.Fatalf("load %s: %v", e.Name, err)
		}
	}
	rec := &recorder{}
	p := newPrinter(rec)
	go p.run()
	t.Cleanup(p.stop)

	return &app{
		cs:        &coding.Session{Extensions: r},
		theme:     DefaultTheme(),
		rend:      newRenderer(DefaultTheme(), 60, false),
		ed:        newEditor(DefaultTheme()),
		printer:   p,
		liveTools: map[string]*liveTool{},
		widgets:   map[string]*widgetEntry{},
		width:     60, height: 24,
	}, rec
}

func drawer(name string, lines []string, err error) extension.Extension {
	return extension.Extension{Name: name, Path: "/ext/" + name, Factory: func(a *extension.API) error {
		a.RegisterMessageRenderer(extension.MessageRenderer{
			Role: "assistant",
			Render: func(context.Context, ai.Message, int) ([]string, error) {
				return lines, err
			},
		})
		return nil
	}}
}

func settle(rec *recorder, want string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(strings.Join(rec.snapshot(), "\n"), want) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func assistant(text string) ai.AssistantMessage {
	return ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: text}}}
}

func TestExtensionRendererReplacesBuiltInRendering(t *testing.T) {
	a, rec := extApp(t, drawer("draw", []string{"«drawn by the extension»"}, nil))

	a.onAgentEvent(agent.Event{Type: agent.EventMessageEnd, Message: assistant("built-in text")})

	if !settle(rec, "«drawn by the extension»") {
		t.Fatalf("extension lines never reached the transcript: %q", rec.snapshot())
	}
	if strings.Contains(strings.Join(rec.snapshot(), "\n"), "built-in text") {
		t.Error("both renderings were emitted; the extension's should have replaced tau's")
	}
}

// Zero lines is "no opinion", not "draw nothing": an extension that wants a
// message to take up no space has to say so with a blank line, otherwise there
// would be no way to decline a message it does not recognise.
func TestRendererReturningNoLinesFallsBackToBuiltIn(t *testing.T) {
	a, rec := extApp(t, drawer("quiet", nil, nil))

	a.onAgentEvent(agent.Event{Type: agent.EventMessageEnd, Message: assistant("built-in text")})

	if !settle(rec, "built-in text") {
		t.Fatalf("declining renderer swallowed the message: %q", rec.snapshot())
	}
}

func TestFailingRendererFallsBackAndSaysSo(t *testing.T) {
	a, rec := extApp(t, drawer("broken", []string{"never seen"}, errors.New("boom")))

	a.onAgentEvent(agent.Event{Type: agent.EventMessageEnd, Message: assistant("built-in text")})

	if !settle(rec, "built-in text") {
		t.Fatalf("a broken renderer left a hole in the transcript: %q", rec.snapshot())
	}
	if !strings.Contains(a.notice, "boom") {
		t.Errorf("notice = %q, want the renderer failure", a.notice)
	}
}

// The draw path is the one place an extension runs on the Update goroutine, so
// the deadline is what keeps a wedged renderer from freezing the transcript.
func TestSlowRendererIsBounded(t *testing.T) {
	slow := extension.Extension{Name: "slow", Path: "/ext/slow", Factory: func(a *extension.API) error {
		a.RegisterMessageRenderer(extension.MessageRenderer{
			Render: func(ctx context.Context, _ ai.Message, _ int) ([]string, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		})
		return nil
	}}
	a, rec := extApp(t, slow)

	start := time.Now()
	a.onAgentEvent(agent.Event{Type: agent.EventMessageEnd, Message: assistant("built-in text")})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a slow renderer held the draw path for %s", elapsed)
	}
	if !settle(rec, "built-in text") {
		t.Fatalf("nothing was drawn after the renderer timed out: %q", rec.snapshot())
	}
}

func TestShortcutClaimsAKeyAndRunsOffTheUpdateGoroutine(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	var once sync.Once
	bound := extension.Extension{Name: "keys", Path: "/ext/keys", Factory: func(a *extension.API) error {
		a.RegisterShortcut(extension.Shortcut{Key: "ctrl+g", Handler: func(context.Context, *extension.Context) error {
			once.Do(wg.Done)
			return nil
		}})
		return nil
	}}
	a, _ := extApp(t, bound)

	cmd := a.onKey(tea.KeyMsg{Type: tea.KeyCtrlG})
	if cmd == nil {
		t.Fatal("a bound key produced no command; the handler would have run inline")
	}
	// The command, not onKey, is what runs the handler — that is the whole
	// point: a shortcut that opens a dialog must not wait on the loop that
	// has to draw it.
	go cmd()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the shortcut handler never ran")
	}
	if a.ed.Value() != "" {
		t.Errorf("a claimed key still reached the editor: %q", a.ed.Value())
	}
}

// Interrupt and abort are the two ways out of a wedged turn; an extension that
// could swallow them could make the agent unstoppable.
func TestReservedKeysAreNotClaimable(t *testing.T) {
	var fired []string
	greedy := extension.Extension{Name: "greedy", Path: "/ext/greedy", Factory: func(a *extension.API) error {
		for _, k := range []string{"ctrl+c", "esc"} {
			key := k
			a.RegisterShortcut(extension.Shortcut{Key: key, Handler: func(context.Context, *extension.Context) error {
				fired = append(fired, key)
				return nil
			}})
		}
		return nil
	}}
	a, _ := extApp(t, greedy)

	for _, msg := range []tea.KeyMsg{{Type: tea.KeyCtrlC}, {Type: tea.KeyEsc}} {
		if cmd := a.onKey(msg); cmd != nil {
			cmd()
		}
	}
	if len(fired) != 0 {
		t.Errorf("an extension intercepted %v", fired)
	}
}

func TestUnboundKeysStillReachTheEditor(t *testing.T) {
	a, _ := extApp(t, drawer("draw", []string{"x"}, nil))
	a.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	a.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if a.ed.Value() != "hi" {
		t.Errorf("editor value = %q, want %q", a.ed.Value(), "hi")
	}
}
