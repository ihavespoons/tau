package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ihavespoons/tau/settings"
)

// enabledModelsKey is the settings key holding the cycle-set patterns.
const enabledModelsKey = "enabledModels"

// ScopedModels renders the cycle set — the models Ctrl+P moves between — and
// says where it was configured.
//
// An unset enabledModels puts every model in the cycle, and printing a
// thousand of them helps nobody, so that case reports the count instead.
func (s *Session) ScopedModels() string {
	total := len(s.AvailableModels())

	patterns := s.Settings.EnabledModels()
	if len(patterns) == 0 {
		return fmt.Sprintf("All %d models are in the cycle set.\n"+
			"Narrow it with /scoped-models <pattern>... — for example: "+
			"/scoped-models anthropic/* openai/gpt-5.2", total)
	}

	matches, diags := s.Models.Scoped(patterns)

	current := ""
	if s.Model != nil {
		current = s.Model.Provider + "/" + s.Model.ID
	}

	var b strings.Builder
	origin := ""
	if scope, ok := s.setMgr.Origin(enabledModelsKey); ok {
		origin = ", from " + string(scope) + " settings"
	}
	fmt.Fprintf(&b, "Cycling %d of %d models%s:\n", len(matches), total, origin)
	for _, m := range matches {
		if m.Model == nil {
			continue
		}
		id := m.Model.Provider + "/" + m.Model.ID
		marker := "  "
		if id == current {
			marker = "* "
		}
		b.WriteString(marker + id + "\n")
	}
	fmt.Fprintf(&b, "\nPatterns: %s", strings.Join(patterns, " "))
	for _, d := range diags {
		b.WriteString("\n" + d.Message)
	}
	return b.String()
}

// SetScopedModels saves patterns as the cycle set. No patterns clears the
// setting, which puts every model back in the cycle.
//
// The write goes to global settings. Project scope would be refused outright
// in an untrusted directory, and a cycle set is a preference about the person
// using tau rather than about the code they are pointing it at.
func (s *Session) SetScopedModels(ctx context.Context, patterns []string) (string, error) {
	if s.setMgr == nil {
		return "", errors.New("this session has no settings file to write to")
	}

	if len(patterns) == 0 {
		if err := s.setMgr.Unset(ctx, settings.Global, enabledModelsKey); err != nil {
			return "", err
		}
		if err := s.refreshSettings(); err != nil {
			return "", err
		}
		return fmt.Sprintf("Cleared the cycle set — all %d models are back in it.",
			len(s.AvailableModels())), nil
	}

	// Resolve before writing. A pattern that matches nothing is almost always
	// a typo, and an empty cycle set falls back to every model, so the mistake
	// would otherwise look like it worked.
	matches, diags := s.Models.Scoped(patterns)
	if len(matches) == 0 {
		return "", fmt.Errorf("no models match %s", strings.Join(patterns, " "))
	}
	if err := s.setMgr.Set(ctx, settings.Global, enabledModelsKey, patterns); err != nil {
		return "", err
	}
	if err := s.refreshSettings(); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Cycling %d models, saved to %s", len(matches), s.setMgr.Path(settings.Global))
	for _, d := range diags {
		b.WriteString("\n" + d.Message)
	}
	return b.String(), nil
}

// refreshSettings rebuilds the merged view after a write, so the change takes
// effect in this session rather than the next one.
func (s *Session) refreshSettings() error {
	res, err := s.setMgr.Resolve()
	if err != nil {
		return fmt.Errorf("reloading settings: %w", err)
	}
	s.Settings = res
	return nil
}
