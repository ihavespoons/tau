package pkgmgr

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.md", "a.md", true},
		{"*.md", "dir/a.md", false},
		{"**/*.md", "dir/a.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**", "anything/at/all", true},
		{"skills/*", "skills/deploy", true},
		{"skills/*", "skills/deploy/SKILL.md", false},
		{"skills/**", "skills/deploy/SKILL.md", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"deploy", "deploy", true},
		{"deploy", "deploying", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestIsPattern(t *testing.T) {
	for _, s := range []string{"!a", "+a", "-a", "a*", "a?b"} {
		if !isPattern(s) {
			t.Errorf("isPattern(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"a", "dir/a.md", "./a"} {
		if isPattern(s) {
			t.Errorf("isPattern(%q) = true, want false", s)
		}
	}
}

func paths(base string, rel ...string) []string {
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = filepath.Join(base, filepath.FromSlash(r))
	}
	return out
}

func enabledList(base string, m map[string]bool) []string {
	var out []string
	for p, on := range m {
		if !on {
			continue
		}
		rel, _ := filepath.Rel(base, p)
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

func TestApplyPatterns(t *testing.T) {
	base := filepath.FromSlash("/pkg")
	all := paths(base, "skills/deploy/SKILL.md", "skills/review/SKILL.md", "prompts/a.md", "prompts/b.md")

	cases := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{"no patterns selects everything", nil,
			[]string{"prompts/a.md", "prompts/b.md", "skills/deploy/SKILL.md", "skills/review/SKILL.md"}},
		{"include acts as a whitelist", []string{"prompts/*"},
			[]string{"prompts/a.md", "prompts/b.md"}},
		{"exclude removes", []string{"!prompts/*"},
			[]string{"skills/deploy/SKILL.md", "skills/review/SKILL.md"}},
		{"skill named by its directory", []string{"!skills/deploy"},
			[]string{"prompts/a.md", "prompts/b.md", "skills/review/SKILL.md"}},
		{"skill named by its directory base name", []string{"!deploy"},
			[]string{"prompts/a.md", "prompts/b.md", "skills/review/SKILL.md"}},
		{"force-include beats exclude", []string{"!prompts/*", "+prompts/a.md"},
			[]string{"prompts/a.md", "skills/deploy/SKILL.md", "skills/review/SKILL.md"}},
		{"force-exclude beats everything", []string{"prompts/*", "+prompts/a.md", "-prompts/a.md"},
			[]string{"prompts/b.md"}},
		{"include plus exclude", []string{"**/*.md", "!skills/**"},
			[]string{"prompts/a.md", "prompts/b.md"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := enabledList(base, applyPatterns(all, c.patterns, base))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A force-include or force-exclude names one file, so it must not match on a
// bare base name the way a glob does — otherwise "-SKILL.md" would disable
// every skill at once.
func TestOverridesDoNotMatchBaseName(t *testing.T) {
	base := filepath.FromSlash("/pkg")
	all := paths(base, "skills/deploy/SKILL.md", "skills/review/SKILL.md")

	got := enabledList(base, applyPatterns(all, []string{"-SKILL.md"}, base))
	want := []string{"skills/deploy/SKILL.md", "skills/review/SKILL.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestApplyAutoloadDisabledPatterns(t *testing.T) {
	base := filepath.FromSlash("/pkg")
	all := paths(base, "skills/deploy/SKILL.md", "skills/review/SKILL.md", "prompts/a.md")

	// Nothing loads by default; only what a pattern names gets a verdict.
	got := applyAutoloadDisabledPatterns(all, []string{"+skills/deploy"}, base)
	if len(got) != 1 {
		t.Fatalf("got %d verdicts, want 1: %v", len(got), got)
	}
	if !got[filepath.Join(base, filepath.FromSlash("skills/deploy/SKILL.md"))] {
		t.Errorf("deploy not enabled: %v", got)
	}

	// A later pattern wins over an earlier one for the same path.
	got = applyAutoloadDisabledPatterns(all, []string{"skills/**", "-skills/deploy"}, base)
	if got[filepath.Join(base, filepath.FromSlash("skills/deploy/SKILL.md"))] {
		t.Error("deploy should be disabled by the later pattern")
	}
	if !got[filepath.Join(base, filepath.FromSlash("skills/review/SKILL.md"))] {
		t.Error("review should stay enabled")
	}
	if _, mentioned := got[filepath.Join(base, filepath.FromSlash("prompts/a.md"))]; mentioned {
		t.Error("prompts/a.md was not named by any pattern")
	}
}

func TestIsEnabledByOverrides(t *testing.T) {
	base := filepath.FromSlash("/home/agent")
	p := filepath.Join(base, filepath.FromSlash("skills/deploy/SKILL.md"))

	cases := []struct {
		patterns []string
		want     bool
	}{
		{nil, true},
		{[]string{"!deploy"}, false},
		{[]string{"!deploy", "+skills/deploy"}, true},
		{[]string{"-skills/deploy"}, false},
		{[]string{"+skills/deploy", "-skills/deploy"}, false},
		// A plain include is not a whitelist here: these resources were found
		// by scanning, and one unrelated include must not hide the rest.
		{[]string{"something-else"}, true},
	}
	for _, c := range cases {
		if got := isEnabledByOverrides(p, c.patterns, base); got != c.want {
			t.Errorf("isEnabledByOverrides(%v) = %v, want %v", c.patterns, got, c.want)
		}
	}
}

func TestSplitPatternEntries(t *testing.T) {
	plain, patterns := splitPatternEntries([]string{"a.md", "!b", "dir/c.md", "+d", "e*"})
	if !reflect.DeepEqual(plain, []string{"a.md", "dir/c.md"}) {
		t.Errorf("plain = %v", plain)
	}
	if !reflect.DeepEqual(patterns, []string{"!b", "+d", "e*"}) {
		t.Errorf("patterns = %v", patterns)
	}
}
