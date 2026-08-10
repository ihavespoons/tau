package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/extension"
)

// harness is a minimal Bubble Tea program that handles dialogs exactly the way
// the real app does — through the same dialogStack — so these tests exercise
// the production contract rather than a reimplementation of it.
type harness struct {
	stack dialogStack
}

func (h *harness) Init() tea.Cmd { return nil }

func (h *harness) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlQ {
			h.stack.closeAll()
			return h, tea.Quit
		}
		h.stack.key(msg, nil)
	case openDialogMsg:
		h.stack.push(msg.d)
	case cancelDialogMsg:
		h.stack.cancel(msg.d)
	}
	return h, nil
}

func (h *harness) View() string {
	if top := h.stack.top(); top != nil {
		return strings.Join(top.view(40, DefaultTheme()), "\n")
	}
	return "idle"
}

// startHarness runs a real program wired to a pipe, so tests drive it with the
// bytes a terminal would actually send.
func startHarness(t *testing.T) (*uiBridge, func(string)) {
	t.Helper()

	pr, pw := io.Pipe()
	bridge := newUIBridge()
	prog := tea.NewProgram(&harness{},
		tea.WithInput(pr), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())
	bridge.attach(prog)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = prog.Run()
	}()

	t.Cleanup(func() {
		_, _ = pw.Write([]byte{0x11}) // Ctrl+Q
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			prog.Kill()
		}
		bridge.shutdown()
		_ = pw.Close()
		_ = pr.Close()
	})

	return bridge, func(keys string) {
		if _, err := pw.Write([]byte(keys)); err != nil {
			t.Errorf("writing keys: %v", err)
		}
	}
}

// The core contract: a caller blocks on a dialog while the render loop stays
// live, and the answer released from the render goroutine unblocks it. If this
// ever deadlocks, an extension asking a question freezes the whole agent.
func TestDialogBlocksCallerAndIsReleasedByTheRenderLoop(t *testing.T) {
	bridge, send := startHarness(t)

	type result struct {
		ok  bool
		err error
	}
	res := make(chan result, 1)
	go func() {
		ok, err := bridge.Confirm(context.Background(), extension.ConfirmRequest{
			Title: "Delete everything?", Message: "really?",
		})
		res <- result{ok, err}
	}()

	// Give the dialog time to reach the render loop before answering it.
	time.Sleep(150 * time.Millisecond)
	send("y")

	select {
	case got := <-res:
		if got.err != nil {
			t.Fatalf("confirm failed: %v", got.err)
		}
		if !got.ok {
			t.Error("answering y should confirm")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the caller was never released — the dialog contract deadlocked")
	}
}

func TestSelectDialogReturnsTheChosenIndex(t *testing.T) {
	bridge, send := startHarness(t)

	res := make(chan int, 1)
	go func() {
		idx, err := bridge.Select(context.Background(), extension.SelectRequest{
			Title: "Pick",
			Options: []extension.SelectOption{
				{Label: "first", Value: "a"},
				{Label: "second", Value: "b"},
				{Label: "third", Value: "c"},
			},
		})
		if err != nil {
			t.Errorf("select: %v", err)
		}
		res <- idx
	}()

	time.Sleep(150 * time.Millisecond)
	send("\x1b[B") // Down
	time.Sleep(50 * time.Millisecond)
	send("\r") // Enter

	select {
	case idx := <-res:
		if idx != 1 {
			t.Errorf("expected the second option, got index %d", idx)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("select never returned")
	}
}

// Escaping a select must report "no choice" rather than silently picking one.
func TestSelectCancelReturnsNoChoice(t *testing.T) {
	bridge, send := startHarness(t)

	res := make(chan int, 1)
	go func() {
		idx, _ := bridge.Select(context.Background(), extension.SelectRequest{
			Title:   "Pick",
			Options: []extension.SelectOption{{Label: "only", Value: "a"}},
		})
		res <- idx
	}()

	time.Sleep(150 * time.Millisecond)
	send("\x1b") // Esc

	select {
	case idx := <-res:
		if idx != -1 {
			t.Errorf("cancelling should yield -1, got %d", idx)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("select never returned after cancel")
	}
}

// A cancelled context must release the caller even though nobody touched the
// keyboard — otherwise aborting a turn would strand the agent goroutine on a
// dialog forever.
func TestDialogHonorsContextCancellation(t *testing.T) {
	bridge, _ := startHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := bridge.Input(ctx, extension.InputRequest{Title: "name?"})
		errs <- err
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the context did not release the caller")
	}
}

// With no program attached, a dialog must fail immediately. Blocking here
// would hang tau's headless modes the first time an extension asked anything.
func TestDialogWithoutProgramFailsFast(t *testing.T) {
	bridge := newUIBridge()

	done := make(chan error, 1)
	go func() {
		_, err := bridge.Confirm(context.Background(), extension.ConfirmRequest{Title: "?"})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, extension.ErrNoUI) {
			t.Errorf("expected ErrNoUI, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a dialog without a UI must fail rather than block")
	}
}

// Shutting the UI down must release anyone already parked on a dialog.
func TestShutdownReleasesParkedCallers(t *testing.T) {
	bridge, _ := startHarness(t)

	done := make(chan error, 1)
	go func() {
		_, err := bridge.Input(context.Background(), extension.InputRequest{Title: "name?"})
		done <- err
	}()

	time.Sleep(150 * time.Millisecond)
	bridge.shutdown()

	select {
	case err := <-done:
		if !errors.Is(err, extension.ErrNoUI) {
			t.Errorf("expected ErrNoUI after shutdown, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown left a caller parked on a dialog")
	}
}

// NoUI is what headless modes get: every dialog fails, nothing blocks, and the
// fire-and-forget calls are silently discarded.
func TestNoUIIsSafeEverywhere(t *testing.T) {
	var ui extension.UI = extension.NoUI{}
	ctx := context.Background()

	if _, err := ui.Confirm(ctx, extension.ConfirmRequest{}); !errors.Is(err, extension.ErrNoUI) {
		t.Errorf("Confirm: %v", err)
	}
	if _, err := ui.Select(ctx, extension.SelectRequest{}); !errors.Is(err, extension.ErrNoUI) {
		t.Errorf("Select: %v", err)
	}
	if _, err := ui.Input(ctx, extension.InputRequest{}); !errors.Is(err, extension.ErrNoUI) {
		t.Errorf("Input: %v", err)
	}
	ui.Notify(extension.Notification{})
	ui.SetStatus("x")
	ui.SetTitle("x")
	ui.SetWidget("id", extension.WidgetAboveEditor, nil)
}
