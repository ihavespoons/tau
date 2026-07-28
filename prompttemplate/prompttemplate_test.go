package prompttemplate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a b c", []string{"a", "b", "c"}},
		{`"hello world" x`, []string{"hello world", "x"}},
		{`'single quoted' y`, []string{"single quoted", "y"}},
		{`  spaced   out  `, []string{"spaced", "out"}},
		{`mix"ed quo"tes`, []string{"mixed quotes"}},
	}
	for _, c := range cases {
		if got := ParseArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseArgs(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestSubstituteArgs(t *testing.T) {
	args := []string{"one", "two", "three"}
	cases := []struct {
		name, in, want string
		args           []string
	}{
		{"positional", "$1 and $2", "one and two", args},
		{"all via ARGUMENTS", "[$ARGUMENTS]", "[one two three]", args},
		{"all via at", "[$@]", "[one two three]", args},
		{"missing positional is empty", "[$5]", "[]", args},
		{"default when missing", "${5:-fallback}", "fallback", args},
		{"default not used when present", "${1:-fallback}", "one", args},
		{"default for empty ARGUMENTS", "${ARGUMENTS:-none}", "none", nil},
		{"slice from n", "${@:2}", "two three", args},
		{"slice with length", "${@:1:2}", "one two", args},
		{"slice past end", "${@:9}", "", args},
		{"zero start treated as one", "${@:0}", "one two three", args},
		{"no placeholders", "plain text", "plain text", args},
		{"values are not re-expanded", "$1", "$2", []string{"$2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SubstituteArgs(c.in, c.args); got != c.want {
				t.Errorf("SubstituteArgs(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func writeTemplate(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	p := writeTemplate(t, dir, "review.md", "---\ndescription: Review a PR\nargument-hint: <pr-url>\n---\nReview $1 carefully.\n")

	tmpl, err := LoadFromFile(p, "user")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Name != "review" {
		t.Errorf("Name = %q", tmpl.Name)
	}
	if tmpl.Description != "Review a PR" || tmpl.ArgumentHint != "<pr-url>" {
		t.Errorf("got %+v", tmpl)
	}
	if tmpl.Content != "Review $1 carefully." {
		t.Errorf("Content = %q", tmpl.Content)
	}
}

// Without a description, the first non-empty body line is used, truncated at
// 60 characters (prompt-templates.ts:113-120).
func TestLoadFromFileDescriptionFallback(t *testing.T) {
	dir := t.TempDir()
	short := writeTemplate(t, dir, "a.md", "\nFirst real line\nsecond\n")
	tmpl, err := LoadFromFile(short, "user")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Description != "First real line" {
		t.Errorf("Description = %q", tmpl.Description)
	}

	long := writeTemplate(t, dir, "b.md", "This line is quite a lot longer than sixty characters in total length yes\n")
	tmpl2, err := LoadFromFile(long, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpl2.Description) != 63 || tmpl2.Description[60:] != "..." {
		t.Errorf("expected truncation to 60 chars + ellipsis, got %q (%d)", tmpl2.Description, len(tmpl2.Description))
	}
}

func TestLoadDefaultsAndPaths(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "proj")
	writeTemplate(t, filepath.Join(agentDir, "prompts"), "global.md", "---\ndescription: g\n---\nglobal\n")
	writeTemplate(t, filepath.Join(cwd, ".tau", "prompts"), "local.md", "---\ndescription: l\n---\nlocal\n")
	writeTemplate(t, filepath.Join(root, "extra"), "other.md", "---\ndescription: o\n---\nother\n")

	got := Load(LoadOptions{Cwd: cwd, AgentDir: agentDir, IncludeDefaults: true, Paths: []string{filepath.Join(root, "extra")}})
	names := map[string]string{}
	for _, tm := range got {
		names[tm.Name] = tm.Source
	}
	if names["global"] != "user" || names["local"] != "project" || names["other"] != "path" {
		t.Errorf("unexpected templates/sources: %v", names)
	}
}

// Directories are scanned non-recursively (prompt-templates.ts:136).
func TestLoadFromDirNonRecursive(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "top.md", "---\ndescription: t\n---\ntop\n")
	writeTemplate(t, filepath.Join(dir, "nested"), "deep.md", "---\ndescription: d\n---\ndeep\n")

	got := LoadFromDir(dir, "user")
	if len(got) != 1 || got[0].Name != "top" {
		t.Errorf("expected only top-level templates, got %+v", got)
	}
}

func TestExpand(t *testing.T) {
	templates := []Template{{Name: "greet", Content: "Hello $1, from $2!"}}
	cases := []struct{ in, want string }{
		{"/greet world tau", "Hello world, from tau!"},
		{"/greet", "Hello , from !"},
		{"/unknown x", "/unknown x"},
		{"not a command", "not a command"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := Expand(c.in, templates); got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFind(t *testing.T) {
	list := []Template{{Name: "a"}, {Name: "b"}}
	if _, ok := Find(list, "a"); !ok {
		t.Error("expected to find a")
	}
	if _, ok := Find(list, "z"); ok {
		t.Error("should not find z")
	}
}
