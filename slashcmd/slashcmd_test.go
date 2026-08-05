package slashcmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/prompttemplate"
	"github.com/ihavespoons/tau/skills"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in        string
		ok        bool
		name      string
		args      string
		skillName string
	}{
		{in: "/model", ok: true, name: "model"},
		{in: "/model opus", ok: true, name: "model", args: "opus"},
		{in: "  /model  opus  ", ok: true, name: "model", args: "opus"},
		{in: "/skill:foo bar baz", ok: true, name: "skill:foo", args: "bar baz", skillName: "foo"},
		{in: "/unknown", ok: true, name: "unknown"},
		{in: "/scoped-models", ok: true, name: "scoped-models"},
		{in: "/name My Session Title", ok: true, name: "name", args: "My Session Title"},
		// Not commands:
		{in: "/", ok: false},
		{in: "", ok: false},
		{in: "hello", ok: false},
		{in: "/usr/bin/env", ok: false}, // a path, not a command
		{in: "/tmp/foo.txt", ok: false},
	}
	for _, c := range cases {
		got, ok := Parse(c.in)
		if ok != c.ok {
			t.Errorf("Parse(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Name != c.name || got.Args != c.args || got.SkillName != c.skillName {
			t.Errorf("Parse(%q) = %+v, want name=%q args=%q skill=%q", c.in, got, c.name, c.args, c.skillName)
		}
	}
}

type stubCmd struct{ info Info }

func (s stubCmd) Info() Info { return s.info }
func (s stubCmd) Run(context.Context, string) (Result, error) {
	return Result{Output: "ran " + s.info.Name}, nil
}

func TestRegistryDuplicateNamesAreSuffixed(t *testing.T) {
	r := NewRegistry()
	first := r.Register(stubCmd{Info{Name: "dup", Description: "first"}})
	second := r.Register(stubCmd{Info{Name: "dup", Description: "second"}})

	if first != "dup" || second != "dup:1" {
		t.Fatalf("names = %q, %q; want dup, dup:1", first, second)
	}
	c, ok := r.Lookup("dup")
	if !ok || c.Info().Description != "first" {
		t.Error("the original registration must keep the bare name")
	}
	if _, ok := r.Lookup("dup:1"); !ok {
		t.Error("the duplicate should be reachable under its suffixed name")
	}
}

func TestRegistryListReflectsSuffixedNames(t *testing.T) {
	r := NewRegistry()
	r.Register(stubCmd{Info{Name: "x"}})
	r.Register(stubCmd{Info{Name: "x"}})

	infos := r.List()
	if len(infos) != 2 || infos[0].Name != "x" || infos[1].Name != "x:1" {
		t.Errorf("List() = %+v", infos)
	}
}

func TestRegistryLookupMissing(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup("nope"); ok {
		t.Error("expected miss")
	}
}

type stubHost struct {
	models    []string
	current   string
	named     string
	compacted string
	movedTo   string
	forkedAt  string
	forkCalls int
	labelled  string
}

func (h *stubHost) Models() []string     { return h.models }
func (h *stubHost) CurrentModel() string { return h.current }
func (h *stubHost) SetModel(id string) error {
	for _, m := range h.models {
		if m == id {
			h.current = id
			return nil
		}
	}
	return errors.New("unknown model " + id)
}
func (h *stubHost) SetSessionName(n string) error { h.named = n; return nil }
func (h *stubHost) SessionInfo() string           { return "session info" }

func (h *stubHost) Compact(_ context.Context, instructions string) (string, error) {
	h.compacted = instructions
	return "compacted", nil
}
func (h *stubHost) SessionTree() string { return "tree" }
func (h *stubHost) SetLabel(_ context.Context, label string) (string, error) {
	h.labelled = label
	return "labelled", nil
}
func (h *stubHost) ForkPoints() string { return "fork points" }
func (h *stubHost) MoveTo(_ context.Context, entryID string) (string, error) {
	h.movedTo = entryID
	return "moved", nil
}
func (h *stubHost) ForkSession(_ context.Context, entryID string) (string, error) {
	h.forkedAt = entryID
	h.forkCalls++
	return "forked", nil
}

func TestRegisterBuiltinsCoversPiList(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, nil)

	for _, b := range Builtins {
		if _, ok := r.Lookup(b.Name); !ok {
			t.Errorf("built-in %q was not registered", b.Name)
		}
	}
	if _, ok := r.Lookup("help"); !ok {
		t.Error("/help should be registered")
	}
}

// Unimplemented built-ins must still advertise correct metadata, but report
// clearly when run.
func TestUnimplementedBuiltinsReportClearly(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, nil)

	c, _ := r.Lookup("export")
	if _, err := c.Run(context.Background(), ""); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected ErrNotImplemented, got %v", err)
	}
	if c.Info().Description == "" || c.Info().Source != SourceBuiltin {
		t.Errorf("metadata should still be accurate: %+v", c.Info())
	}
}

func TestModelCommand(t *testing.T) {
	host := &stubHost{models: []string{"claude-sonnet-5", "claude-opus-5"}, current: "claude-sonnet-5"}
	r := NewRegistry()
	RegisterBuiltins(r, host)
	c, _ := r.Lookup("model")

	// No args lists models, marking the current one.
	res, err := c.Run(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "* claude-sonnet-5") {
		t.Errorf("current model should be marked:\n%s", res.Output)
	}

	if _, err := c.Run(context.Background(), "claude-opus-5"); err != nil {
		t.Fatal(err)
	}
	if host.current != "claude-opus-5" {
		t.Errorf("model not switched, current = %q", host.current)
	}
	if _, err := c.Run(context.Background(), "nope"); err == nil {
		t.Error("expected an error for an unknown model")
	}
}

func TestModelCompletion(t *testing.T) {
	host := &stubHost{models: []string{"claude-sonnet-5", "claude-opus-5"}}
	r := NewRegistry()
	RegisterBuiltins(r, host)

	items := r.Complete("model", "claude-o")
	if len(items) != 1 || items[0].Value != "claude-opus-5" {
		t.Errorf("completions = %+v", items)
	}
	if got := r.Complete("quit", ""); got != nil {
		t.Errorf("a command without a completer should return nil, got %+v", got)
	}
}

func TestNameAndQuitAndSession(t *testing.T) {
	host := &stubHost{}
	r := NewRegistry()
	RegisterBuiltins(r, host)

	name, _ := r.Lookup("name")
	if _, err := name.Run(context.Background(), "My Session"); err != nil {
		t.Fatal(err)
	}
	if host.named != "My Session" {
		t.Errorf("session name = %q", host.named)
	}
	if _, err := name.Run(context.Background(), "  "); err == nil {
		t.Error("empty name should error")
	}

	quit, _ := r.Lookup("quit")
	res, err := quit.Run(context.Background(), "")
	if err != nil || !res.Quit {
		t.Errorf("quit should request exit, got %+v %v", res, err)
	}

	sess, _ := r.Lookup("session")
	if res, err := sess.Run(context.Background(), ""); err != nil || res.Output != "session info" {
		t.Errorf("session = %+v %v", res, err)
	}
}

func TestHelpListsCommands(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, nil)
	help, _ := r.Lookup("help")

	res, err := help.Run(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/model", "/quit", "/compact"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("help output missing %q:\n%s", want, res.Output)
		}
	}
}

func TestRegisterTemplatesProducePrompts(t *testing.T) {
	r := NewRegistry()
	RegisterTemplates(r, []prompttemplate.Template{
		{Name: "review", Description: "Review code", Content: "Please review $1", FilePath: "/p/review.md"},
	})
	c, ok := r.Lookup("review")
	if !ok {
		t.Fatal("template command not registered")
	}
	if c.Info().Source != SourcePrompt || c.Info().SourceInfo != "/p/review.md" {
		t.Errorf("info = %+v", c.Info())
	}
	res, err := c.Run(context.Background(), "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Prompt != "Please review main.go" {
		t.Errorf("Prompt = %q", res.Prompt)
	}
	if res.Output != "" {
		t.Error("a template should yield a Prompt, not Output")
	}
}

func TestRegisterSkillsProducePrompts(t *testing.T) {
	r := NewRegistry()
	RegisterSkills(r, []skills.Skill{
		{Name: "deploy", Description: "How to deploy", FilePath: "/s/deploy/SKILL.md", BaseDir: "/s/deploy"},
	})
	c, ok := r.Lookup("skill:deploy")
	if !ok {
		t.Fatal("skill command not registered")
	}
	if c.Info().Source != SourceSkill {
		t.Errorf("source = %q", c.Info().Source)
	}
	res, err := c.Run(context.Background(), "to staging")
	if err != nil {
		t.Fatal(err)
	}
	// The prompt points at the file rather than inlining it.
	for _, want := range []string{"/s/deploy/SKILL.md", "/s/deploy", "to staging"} {
		if !strings.Contains(res.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, res.Prompt)
		}
	}
}

func TestNewDefaultsToExtensionSource(t *testing.T) {
	c := New(Info{Name: "custom"}, func(context.Context, string) (Result, error) {
		return Result{Output: "ok"}, nil
	})
	if c.Info().Source != SourceExtension {
		t.Errorf("source = %q, want extension", c.Info().Source)
	}
}

// /compact must work without a UI: a scripted run that outgrows its window
// still needs a way to keep going.
func TestCompactWorksHeadlessAndForwardsItsFocus(t *testing.T) {
	host := &stubHost{}
	r := NewRegistry()
	RegisterBuiltins(r, host)

	c, _ := r.Lookup("compact")
	res, err := c.Run(context.Background(), "  keep the migration details  ")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "compacted" {
		t.Errorf("output = %q", res.Output)
	}
	if host.compacted != "keep the migration details" {
		t.Errorf("instructions = %q, want them trimmed and passed through", host.compacted)
	}
}

// With an id, /tree moves. Without one and headless, it prints — a scripted
// session still needs to be able to see the shape of its own history.
func TestTreeMovesWithAnIDAndPrintsWithout(t *testing.T) {
	host := &stubHost{}
	r := NewRegistry()
	RegisterBuiltins(r, host)
	c, _ := r.Lookup("tree")

	if res, err := c.Run(context.Background(), "abc123"); err != nil || res.Output != "moved" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if host.movedTo != "abc123" {
		t.Errorf("moved to %q", host.movedTo)
	}

	res, err := c.Run(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "tree" {
		t.Errorf("output = %q, want the rendered tree", res.Output)
	}
}

// A fork replaces the session, so the host has to be told to redraw everything
// derived from it — otherwise the transcript on screen belongs to a file that
// is no longer open.
func TestForkAndCloneReportTheSessionChanged(t *testing.T) {
	host := &stubHost{}
	r := NewRegistry()
	RegisterBuiltins(r, host)

	fork, _ := r.Lookup("fork")
	res, err := fork.Run(context.Background(), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !res.SessionChanged {
		t.Error("a fork changes the session")
	}
	if host.forkedAt != "abc123" {
		t.Errorf("forked at %q", host.forkedAt)
	}

	clone, _ := r.Lookup("clone")
	res, err = clone.Run(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.SessionChanged {
		t.Error("a clone changes the session")
	}
	// A clone is a fork of everything, which is the empty entry id.
	if host.forkedAt != "" {
		t.Errorf("clone forked at %q, want the whole session", host.forkedAt)
	}
	if host.forkCalls != 2 {
		t.Errorf("fork calls = %d, want 2", host.forkCalls)
	}
}

// Headless /fork with no argument has nothing to open a picker with, so it
// lists the points rather than forking somewhere arbitrary.
func TestHeadlessForkWithoutAnIDListsRatherThanForks(t *testing.T) {
	host := &stubHost{}
	r := NewRegistry()
	RegisterBuiltins(r, host)

	c, _ := r.Lookup("fork")
	res, err := c.Run(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "fork points" {
		t.Errorf("output = %q", res.Output)
	}
	if host.forkCalls != 0 {
		t.Error("nothing should have been forked")
	}
}

// A label is how a point in a long session stays findable — the entry ids are
// eight random characters. Setting one must not need a UI.
func TestLabelSetsAndClears(t *testing.T) {
	host := &stubHost{}
	r := NewRegistry()
	RegisterBuiltins(r, host)
	c, _ := r.Lookup("label")

	if _, err := c.Run(context.Background(), "  before the refactor  "); err != nil {
		t.Fatal(err)
	}
	if host.labelled != "before the refactor" {
		t.Errorf("label = %q, want it trimmed and passed through", host.labelled)
	}

	if _, err := c.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if host.labelled != "" {
		t.Errorf("an empty argument should clear the label, got %q", host.labelled)
	}
}
