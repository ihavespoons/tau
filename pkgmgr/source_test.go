package pkgmgr

import "testing"

func TestParseSourceNPM(t *testing.T) {
	tests := []struct {
		in      string
		name    string
		version string
		pinned  bool
	}{
		{"npm:pkg", "pkg", "", false},
		{"npm:pkg@1.2.3", "pkg", "1.2.3", true},
		{"npm:pkg@^1.2.0", "pkg", "^1.2.0", false},
		{"npm:pkg@latest", "pkg", "latest", false},
		{"npm:@scope/pkg", "@scope/pkg", "", false},
		{"npm:@scope/pkg@2.0.0", "@scope/pkg", "2.0.0", true},
		{"npm:@scope/pkg@1.0.0-beta.1", "@scope/pkg", "1.0.0-beta.1", true},
	}
	for _, tt := range tests {
		got := ParseSource(tt.in)
		if got.Kind != KindNPM {
			t.Errorf("%s: kind = %s, want npm", tt.in, got.Kind)
			continue
		}
		if got.Name != tt.name || got.Version != tt.version || got.Pinned != tt.pinned {
			t.Errorf("%s: got name=%q version=%q pinned=%v, want %q/%q/%v",
				tt.in, got.Name, got.Version, got.Pinned, tt.name, tt.version, tt.pinned)
		}
	}
}

func TestParseSourceLocal(t *testing.T) {
	for _, in := range []string{".", "..", "./pkg", "../pkg", "~/pkg", "/abs/pkg", "C:\\pkg", "just-a-name", "owner/repo"} {
		got := ParseSource(in)
		if got.Kind != KindLocal {
			t.Errorf("%s: kind = %s, want local", in, got.Kind)
		}
		if got.LocalPath != in {
			t.Errorf("%s: localPath = %q", in, got.LocalPath)
		}
	}
}

func TestParseSourceGit(t *testing.T) {
	tests := []struct {
		in     string
		host   string
		path   string
		ref    string
		pinned bool
	}{
		{"https://github.com/o/r", "github.com", "o/r", "", false},
		{"https://github.com/o/r.git", "github.com", "o/r", "", false},
		{"https://github.com/o/r#v1.0", "github.com", "o/r", "v1.0", true},
		{"https://github.com/o/r@main", "github.com", "o/r", "main", true},
		{"git:git@github.com:o/r.git", "github.com", "o/r", "", false},
		{"git:git@github.com:o/r@dev", "github.com", "o/r", "dev", true},
		{"ssh://git@gitlab.com/o/r", "gitlab.com", "o/r", "", false},
		{"git://example.com/o/r", "example.com", "o/r", "", false},
		{"git:github.com/o/r", "github.com", "o/r", "", false},
		{"github:o/r", "github.com", "o/r", "", false},
		{"github:o/r#tag", "github.com", "o/r", "tag", true},
		{"gitlab:o/r", "gitlab.com", "o/r", "", false},
		{"bitbucket:o/r", "bitbucket.org", "o/r", "", false},
		{"https://github.com/o/r/sub", "github.com", "o/r/sub", "", false},
	}
	for _, tt := range tests {
		got := ParseSource(tt.in)
		if got.Kind != KindGit {
			t.Errorf("%s: kind = %s, want git", tt.in, got.Kind)
			continue
		}
		if got.Host != tt.host || got.Path != tt.path || got.Ref != tt.ref || got.Pinned != tt.pinned {
			t.Errorf("%s: got host=%q path=%q ref=%q pinned=%v, want %q/%q/%q/%v",
				tt.in, got.Host, got.Path, got.Ref, got.Pinned, tt.host, tt.path, tt.ref, tt.pinned)
		}
	}
}

// Shorthand without a "git:" prefix must not become a clone: "some/path" is a
// directory, and guessing otherwise would fetch code the user never named.
func TestBareShorthandIsLocal(t *testing.T) {
	for _, in := range []string{"o/r", "github.com/o/r", "git@github.com:o/r"} {
		if got := ParseSource(in); got.Kind != KindLocal {
			t.Errorf("%s: kind = %s, want local", in, got.Kind)
		}
	}
	if got := ParseSource("git:github.com/o/r"); got.Kind != KindGit {
		t.Errorf("git:github.com/o/r: kind = %s, want git", got.Kind)
	}
}

func TestParseGitURLRejectsTraversal(t *testing.T) {
	bad := []string{
		"https://github.com/../../etc/passwd",
		"https://github.com/o/../../../r",
		"https://github.com/o/%2e%2e/r",
		"git:..%2f..%2fetc/passwd",
		"https://github.com/o",
		"https://github.com/",
		"git@github.com:onlyone",
	}
	for _, in := range bad {
		if got, ok := ParseGitURL(in); ok {
			t.Errorf("%s: parsed as git host=%q path=%q, want rejected", in, got.Host, got.Path)
		}
	}
}

func TestUnsafePathPart(t *testing.T) {
	cases := []struct {
		value      string
		allowSlash bool
		unsafe     bool
	}{
		{"github.com", false, false},
		{"o/r", true, false},
		{"o/r", false, true},
		{"..", true, true},
		{"o/../r", true, true},
		{"o/%2e%2e/r", true, true},
		{"/abs", true, true},
		{`o\r`, true, true},
		{"o\x00r", true, true},
	}
	for _, c := range cases {
		if got := unsafePathPart(c.value, c.allowSlash); got != c.unsafe {
			t.Errorf("unsafePathPart(%q, %v) = %v, want %v", c.value, c.allowSlash, got, c.unsafe)
		}
	}
}
