package coding

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/settings"
)

func TestScopedModelsReportsTheWholeCatalogWhenUnset(t *testing.T) {
	cs := newTestSession(t, Options{})

	out := cs.ScopedModels()
	if !strings.Contains(out, "in the cycle set") {
		t.Errorf("output = %q", out)
	}
	// The whole catalog is thousands of models; naming them would bury the
	// answer to the question that was asked.
	if strings.Count(out, "\n") > 2 {
		t.Errorf("an unset cycle set should not list models:\n%s", out)
	}
}

func TestScopedModelsRoundTripsThroughSettings(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	before := len(cs.CycleModels())

	out, err := cs.SetScopedModels(ctx, []string{cs.Model.Provider + "/" + cs.Model.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Cycling 1 model") {
		t.Errorf("output = %q", out)
	}

	// The change has to reach this session, not just the file.
	if got := cs.CycleModels(); len(got) != 1 {
		t.Errorf("cycle set has %d models, want 1", len(got))
	}
	if got := cs.ScopedModels(); !strings.Contains(got, "* "+cs.Model.Provider+"/"+cs.Model.ID) {
		t.Errorf("the active model should be marked:\n%s", got)
	}

	// And it has to reach the file, not just this session.
	raw, err := os.ReadFile(cs.setMgr.Path(settings.Global))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), enabledModelsKey) {
		t.Errorf("settings.json does not carry the setting:\n%s", raw)
	}

	if _, err := cs.SetScopedModels(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(cs.CycleModels()); got != before {
		t.Errorf("clearing left %d models in the cycle, want %d", got, before)
	}
}

// A pattern is globbed against both "provider/id" and the bare id, so a
// provider glob also catches resellers who prefix that provider's name into
// their model ids. That is Pi's behaviour and the useful one: asking for
// anthropic/* and getting the same models through OpenRouter is the point.
func TestScopedModelsAcceptsGlobs(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	if _, err := cs.SetScopedModels(ctx, []string{cs.Model.Provider + "/*"}); err != nil {
		t.Fatal(err)
	}
	got := cs.CycleModels()
	switch {
	case len(got) < 2:
		t.Fatalf("a provider glob matched %d models, want the provider's catalog", len(got))
	case len(got) == len(cs.AvailableModels()):
		t.Fatal("a provider glob should not match the whole catalog")
	}

	found := false
	for _, m := range got {
		if m.Provider == cs.Model.Provider && m.ID == cs.Model.ID {
			found = true
		}
		if !strings.Contains(m.Provider+"/"+m.ID, cs.Model.Provider) {
			t.Errorf("%s/%s does not mention the globbed provider", m.Provider, m.ID)
		}
	}
	if !found {
		t.Error("the active model should be in its own provider's glob")
	}
}

// A typo would otherwise be saved and then silently ignored, because an empty
// cycle set falls back to every model.
func TestScopedModelsRefusesPatternsThatMatchNothing(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	if _, err := cs.SetScopedModels(ctx, []string{"no-such-provider/no-such-model"}); err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := cs.setMgr.Origin(enabledModelsKey); ok {
		t.Error("nothing should have been written")
	}
}

// A pattern that matches nothing alongside ones that do is reported but not
// fatal: Pi keeps the usable part of the scope rather than dropping all of it.
func TestScopedModelsReportsUnmatchedPatterns(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	out, err := cs.SetScopedModels(ctx, []string{cs.Model.ID, "no-such-model-at-all"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no-such-model-at-all") {
		t.Errorf("the unmatched pattern should be reported:\n%s", out)
	}
}

func TestScopedModelsCommand(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	res, err := cs.RunCommand(ctx, "/scoped-models "+cs.Model.Provider+"/*")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "Cycling") {
		t.Errorf("output = %q", res.Output)
	}
	if len(cs.CycleModels()) == len(cs.AvailableModels()) {
		t.Error("the cycle set was not narrowed")
	}

	if _, err := cs.RunCommand(ctx, "/scoped-models all"); err != nil {
		t.Fatal(err)
	}
	if len(cs.CycleModels()) != len(cs.AvailableModels()) {
		t.Error("`all` should clear the cycle set")
	}
}
