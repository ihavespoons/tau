package tui

import "github.com/charmbracelet/lipgloss"

// Theme is the TUI's palette. Colors are adaptive so tau reads correctly on
// light and dark terminals without asking which one it is.
//
// P9 replaces this with Pi's loadable JSON themes; the field names are chosen
// to survive that change.
type Theme struct {
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
	Status    lipgloss.Style
	Selected  lipgloss.Style
	Border    lipgloss.Style
	DialogBox lipgloss.Style
}

func adaptive(light, dark string) lipgloss.AdaptiveColor {
	return lipgloss.AdaptiveColor{Light: light, Dark: dark}
}

// DefaultTheme is tau's built-in palette.
func DefaultTheme() Theme {
	var (
		dim    = adaptive("#6c6f85", "#8a8fa3")
		accent = adaptive("#1e66f5", "#89b4fa")
		green  = adaptive("#40a02b", "#a6e3a1")
		red    = adaptive("#d20f39", "#f38ba8")
		yellow = adaptive("#df8e1d", "#f9e2af")
		cyan   = adaptive("#179299", "#94e2d5")
	)
	return Theme{
		User:      lipgloss.NewStyle().Foreground(accent).Bold(true),
		Assistant: lipgloss.NewStyle(),
		Thinking:  lipgloss.NewStyle().Foreground(dim).Italic(true),
		ToolName:  lipgloss.NewStyle().Foreground(cyan).Bold(true),
		ToolArgs:  lipgloss.NewStyle().Foreground(dim),
		ToolOut:   lipgloss.NewStyle().Foreground(dim),
		Error:     lipgloss.NewStyle().Foreground(red),
		Warning:   lipgloss.NewStyle().Foreground(yellow),
		Success:   lipgloss.NewStyle().Foreground(green),
		Dim:       lipgloss.NewStyle().Foreground(dim),
		Accent:    lipgloss.NewStyle().Foreground(accent),
		Bold:      lipgloss.NewStyle().Bold(true),
		Prompt:    lipgloss.NewStyle().Foreground(accent),
		Status:    lipgloss.NewStyle().Foreground(dim),
		Selected:  lipgloss.NewStyle().Foreground(accent).Bold(true),
		Border:    lipgloss.NewStyle().Foreground(dim),
		DialogBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1),
	}
}
