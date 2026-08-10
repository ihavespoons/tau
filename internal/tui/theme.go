package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/ihavespoons/tau/theme"
)

// Theme is the TUI's palette: the handful of lipgloss styles the interface
// paints with, derived from a loaded theme.
//
// The styles are a projection, not the whole theme — Colors keeps the full
// token set so the markdown renderer, diff views, and the HTML export can read
// the tokens they need without going back through here.
type Theme struct {
	// Name is the loaded theme's name.
	Name string
	// Colors is the theme the styles were built from; never nil.
	Colors *theme.Theme

	User      lipgloss.Style
	Assistant lipgloss.Style
	Thinking  lipgloss.Style
	ToolName  lipgloss.Style
	ToolArgs  lipgloss.Style
	ToolOut   lipgloss.Style
	Error     lipgloss.Style
	Warning   lipgloss.Style
	Success   lipgloss.Style
	Dim       lipgloss.Style
	Accent    lipgloss.Style
	Bold      lipgloss.Style
	Prompt    lipgloss.Style
	BashMode  lipgloss.Style
	Status    lipgloss.Style
	Selected  lipgloss.Style
	Border    lipgloss.Style
	DialogBox lipgloss.Style
}

// FromTheme builds the TUI palette from a loaded theme. A nil theme falls back
// to the built-in dark one.
//
// Where tau's interface has no counterpart to a Pi token — tau marks the
// selected row with "▸ " rather than a filled background, for instance — the
// nearest foreground token is used, so a theme written for Pi still reads the
// way its author intended.
func FromTheme(t *theme.Theme) Theme {
	if t == nil {
		if b, ok := theme.Builtin("dark"); ok {
			t = b
		} else {
			// Unreachable short of a corrupt binary: the built-ins are
			// embedded and parsed at init. Plain styles beat a panic.
			return Theme{Name: "plain", Colors: &theme.Theme{}}
		}
	}

	fg := func(tok theme.Token) lipgloss.Style {
		s := lipgloss.NewStyle()
		if c := t.Color(tok); c != "" {
			s = s.Foreground(lipgloss.Color(c))
		}
		return s
	}

	return Theme{
		Name:      t.Name,
		Colors:    t,
		User:      fg(theme.Accent).Bold(true),
		Assistant: fg(theme.Text),
		Thinking:  fg(theme.ThinkingText).Italic(true),
		ToolName:  fg(theme.ToolTitle).Bold(true),
		ToolArgs:  fg(theme.Muted),
		ToolOut:   fg(theme.ToolOutput),
		Error:     fg(theme.Error),
		Warning:   fg(theme.Warning),
		Success:   fg(theme.Success),
		Dim:       fg(theme.Dim),
		Accent:    fg(theme.Accent),
		Bold:      lipgloss.NewStyle().Bold(true),
		Prompt:    fg(theme.Accent),
		BashMode:  fg(theme.BashMode).Bold(true),
		Status:    fg(theme.Muted),
		Selected:  fg(theme.Accent).Bold(true),
		Border:    fg(theme.Border),
		DialogBox: fg(theme.Text).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(orDefault(t.Color(theme.BorderAccent), t.Color(theme.Border)))).
			Padding(0, 1),
	}
}

func orDefault(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// DefaultTheme is the built-in dark palette, used when nothing has been
// configured and by tests that do not care which theme they render under.
func DefaultTheme() Theme {
	t, _ := theme.Builtin("dark")
	return FromTheme(t)
}

// LoadTheme resolves the configured theme setting against the discovered
// themes and builds the palette. It returns the palette and one warning line
// per theme that could not be loaded or was asked for and not found — the
// caller decides where those go.
func LoadTheme(setting string, opts theme.Options) (Theme, []string) {
	set := theme.Discover(opts)

	var warnings []string
	for _, d := range set.Diagnostics() {
		if d.Path != "" {
			warnings = append(warnings, "theme "+d.Path+": "+d.Message)
			continue
		}
		warnings = append(warnings, "theme: "+d.Message)
	}

	t, ok := set.Resolve(setting, detectBackground())
	if !ok && setting != "" {
		warnings = append(warnings, "theme not found: "+setting)
	}
	return FromTheme(t), warnings
}

// terminalIsDark asks the terminal itself. Replaced in tests, which have no
// terminal to ask.
var terminalIsDark = lipgloss.HasDarkBackground

// detectBackground decides which built-in theme an unqualified setting means.
//
// COLORFGBG is a reading rather than a guess, so it wins outright. Absent it,
// lipgloss queries the terminal over OSC 11 — the query Pi makes too, kept out
// of the theme package because it needs the terminal rather than the
// environment.
func detectBackground() theme.Mode {
	if d := theme.DetectBackground(nil); d.Confident {
		return d.Mode
	}
	if terminalIsDark() {
		return theme.Dark
	}
	return theme.Light
}
