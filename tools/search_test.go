package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env/osenv"
)

// searchTree builds a small project to search: a nested directory, a hidden
// file, and one the .gitignore excludes.
func searchTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		"main.go":           "package main\n\nfunc main() {\n\tprintln(\"needle\")\n}\n",
		"internal/util.go":  "package internal\n\n// needle lives here too\nfunc Util() {}\n",
		"README.md":         "no match here\n",
		".gitignore":        "ignored/\n",
		"ignored/secret.go": "package ignored\n\n// needle in an ignored file\n",
		".hidden/config.go": "package hidden\n\n// needle in a hidden dir\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runTool(t *testing.T, tool agent.Tool, args string) agent.ToolResult {
	t.Helper()
	res, err := tool.Execute(context.Background(), "call_1", []byte(args), nil)
	if err != nil {
		t.Fatalf("%s failed: %v", tool.Def().Name, err)
	}
	return res
}

func toolsFor(t *testing.T, root string) (grep, find, ls agent.Tool) {
	t.Helper()
	e, err := osenv.New(osenv.Options{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	return Grep(e), Find(e), Ls(e)
}

// requireBinary skips when the tool is neither installed nor fetchable. These
// tests exercise real ripgrep and fd on purpose — the value is in the flags
// and output parsing, and a fake would test only the fake.
func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, ok := binaryPath(name); !ok {
		t.Skipf("%s is not installed", name)
	}
}

func TestGrepFindsMatchesWithPaths(t *testing.T) {
	requireBinary(t, "rg")
	root := searchTree(t)
	grep, _, _ := toolsFor(t, root)

	text := resultText(runTool(t, grep, `{"pattern":"needle"}`))

	for _, want := range []string{"main.go:4:", "internal/util.go:3:"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	// Paths are shown relative to the search root; absolute ones would be a
	// column of noise.
	if strings.Contains(text, root) {
		t.Errorf("absolute paths leaked into the output:\n%s", text)
	}
}

// .gitignore is the whole reason for shelling out to ripgrep: a searcher that
// walks everything buries real matches under build output.
func TestGrepRespectsGitignore(t *testing.T) {
	requireBinary(t, "rg")
	root := searchTree(t)
	// fd and ripgrep only apply ignore files inside a repository.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	grep, _, _ := toolsFor(t, root)

	text := resultText(runTool(t, grep, `{"pattern":"needle"}`))

	if strings.Contains(text, "ignored/secret.go") {
		t.Errorf("an ignored file was searched:\n%s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Errorf("the ignore rules swallowed a real match:\n%s", text)
	}
}

// Hidden files are searched: a coding agent asked about configuration should
// find .github and .config, which a plain search would skip.
func TestGrepSearchesHiddenFiles(t *testing.T) {
	requireBinary(t, "rg")
	grep, _, _ := toolsFor(t, searchTree(t))

	if text := resultText(runTool(t, grep, `{"pattern":"needle"}`)); !strings.Contains(text, ".hidden/config.go") {
		t.Errorf("a hidden file was skipped:\n%s", text)
	}
}

func TestGrepContextLines(t *testing.T) {
	requireBinary(t, "rg")
	grep, _, _ := toolsFor(t, searchTree(t))

	text := resultText(runTool(t, grep, `{"pattern":"needle","context":1,"glob":"main.go"}`))

	// A match line is marked with a colon and a context line with a dash,
	// which is grep's own convention.
	if !strings.Contains(text, "main.go:4:") {
		t.Errorf("no match line:\n%s", text)
	}
	if !strings.Contains(text, "main.go-3-") {
		t.Errorf("no context line before the match:\n%s", text)
	}
}

func TestGrepLiteralMode(t *testing.T) {
	requireBinary(t, "rg")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("cost is a.b\ncost is axb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	grep, _, _ := toolsFor(t, root)

	regex := resultText(runTool(t, grep, `{"pattern":"a.b"}`))
	if !strings.Contains(regex, "axb") {
		t.Errorf("as a regex, a.b should match axb:\n%s", regex)
	}

	literal := resultText(runTool(t, grep, `{"pattern":"a.b","literal":true}`))
	if strings.Contains(literal, "axb") {
		t.Errorf("as a literal, a.b must not match axb:\n%s", literal)
	}
}

// The limit is a promise about output size; exceeding it would blow the
// context window the tool exists to protect.
func TestGrepHonoursItsLimit(t *testing.T) {
	requireBinary(t, "rg")
	root := t.TempDir()
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "needle")
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	grep, _, _ := toolsFor(t, root)

	res := runTool(t, grep, `{"pattern":"needle","limit":5}`)
	if got := len(strings.Split(resultText(res), "\n")); got != 5 {
		t.Errorf("returned %d lines, want 5", got)
	}
	details, ok := res.Details.(*SearchDetails)
	if !ok || details.LimitReached != 5 {
		t.Errorf("the result must say it was capped: %#v", res.Details)
	}
}

// One minified line should not fill the entire result.
func TestGrepTruncatesLongLines(t *testing.T) {
	requireBinary(t, "rg")
	root := t.TempDir()
	long := "needle" + strings.Repeat("x", grepMaxLineLength*2)
	if err := os.WriteFile(filepath.Join(root, "min.js"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	grep, _, _ := toolsFor(t, root)

	res := runTool(t, grep, `{"pattern":"needle"}`)
	if got := len(resultText(res)); got > grepMaxLineLength+100 {
		t.Errorf("output is %d chars; the line was not truncated", got)
	}
	if details, ok := res.Details.(*SearchDetails); !ok || !details.LinesCut {
		t.Error("truncation must be reported, or the model trusts a partial line as complete")
	}
}

// No matches is a result, not a failure — ripgrep exits non-zero for it.
func TestGrepNoMatchesIsNotAnError(t *testing.T) {
	requireBinary(t, "rg")
	grep, _, _ := toolsFor(t, searchTree(t))

	if text := resultText(runTool(t, grep, `{"pattern":"nothing-matches-this"}`)); !strings.Contains(text, "No matches") {
		t.Errorf("output: %q", text)
	}
}

func TestGrepRequiresAPattern(t *testing.T) {
	grep, _, _ := toolsFor(t, t.TempDir())
	if _, err := grep.Execute(context.Background(), "c", []byte(`{}`), nil); err == nil {
		t.Error("an empty pattern must be rejected rather than matching everything")
	}
}

func TestFindMatchesAGlob(t *testing.T) {
	requireBinary(t, "fd")
	_, find, _ := toolsFor(t, searchTree(t))

	text := resultText(runTool(t, find, `{"pattern":"*.go"}`))

	if !strings.Contains(text, "main.go") {
		t.Errorf("missing main.go:\n%s", text)
	}
	if strings.Contains(text, "README.md") {
		t.Errorf("the glob matched a file it should not have:\n%s", text)
	}
}

// A pattern naming a directory has to match the whole path, not each base
// name — otherwise 'internal/*.go' finds nothing.
func TestFindMatchesAPathPattern(t *testing.T) {
	requireBinary(t, "fd")
	_, find, _ := toolsFor(t, searchTree(t))

	text := resultText(runTool(t, find, `{"pattern":"**/internal/*.go"}`))
	if !strings.Contains(text, "internal/util.go") {
		t.Errorf("a path pattern found nothing:\n%s", text)
	}
}

func TestFindNoMatchesIsNotAnError(t *testing.T) {
	requireBinary(t, "fd")
	_, find, _ := toolsFor(t, searchTree(t))

	if text := resultText(runTool(t, find, `{"pattern":"*.nothing"}`)); !strings.Contains(text, "No files found") {
		t.Errorf("output: %q", text)
	}
}

func TestLsListsAndMarksDirectories(t *testing.T) {
	_, _, ls := toolsFor(t, searchTree(t))

	text := resultText(runTool(t, ls, `{}`))
	lines := strings.Split(text, "\n")

	var hasDir, hasFile bool
	for _, line := range lines {
		if line == "internal/" {
			hasDir = true
		}
		if line == "main.go" {
			hasFile = true
		}
	}
	if !hasDir {
		t.Errorf("a directory was not marked with a trailing slash:\n%s", text)
	}
	if !hasFile {
		t.Errorf("a file is missing:\n%s", text)
	}
}

// Sorting is case-insensitive, so a listing reads the way a person would sort
// it rather than putting every capitalised name first.
func TestLsSortsCaseInsensitively(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Beta.txt", "alpha.txt", "Gamma.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, _, ls := toolsFor(t, root)

	got := strings.Split(resultText(runTool(t, ls, `{}`)), "\n")
	want := []string{"alpha.txt", "Beta.txt", "Gamma.txt"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order: %v, want %v", got, want)
		}
	}
}

func TestLsRejectsAFile(t *testing.T) {
	root := searchTree(t)
	_, _, ls := toolsFor(t, root)

	_, err := ls.Execute(context.Background(), "c", []byte(`{"path":"main.go"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error: %v", err)
	}
}

func TestLsOnAMissingPath(t *testing.T) {
	_, _, ls := toolsFor(t, t.TempDir())

	_, err := ls.Execute(context.Background(), "c", []byte(`{"path":"nowhere"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("error: %v", err)
	}
}

func TestLsEmptyDirectory(t *testing.T) {
	_, _, ls := toolsFor(t, t.TempDir())

	if text := resultText(runTool(t, ls, `{}`)); !strings.Contains(text, "empty") {
		t.Errorf("an empty directory should say so, not return nothing: %q", text)
	}
}

// Paths resolve against the SESSION's directory, not the process's — they can
// differ, and resolving against the wrong one searches the wrong tree.
func TestPathsResolveAgainstTheSessionCwd(t *testing.T) {
	root := searchTree(t)

	if got := resolveToCwd("internal", root); got != filepath.Join(root, "internal") {
		t.Errorf("relative path resolved to %q", got)
	}
	if got := resolveToCwd("", root); got != root {
		t.Errorf("an empty path should mean the cwd, got %q", got)
	}
	abs := filepath.Join(root, "main.go")
	if got := resolveToCwd(abs, "/somewhere/else"); got != abs {
		t.Errorf("an absolute path must not be re-rooted: %q", got)
	}
}

// TAU_OFFLINE is a promise: a user who sets it does not want a coding agent
// reaching GitHub mid-session, and the failure has to say why rather than
// timing out.
func TestOfflineRefusesToDownload(t *testing.T) {
	t.Setenv("TAU_AGENT_DIR", t.TempDir())
	t.Setenv("TAU_OFFLINE", "1")
	resolved.Delete("fd")
	t.Cleanup(func() { resolved.Delete("fd") })

	// Only meaningful when the binary is genuinely absent.
	if _, ok := binaryPath("fd"); ok {
		t.Skip("fd is installed, so nothing would be downloaded anyway")
	}

	_, err := ensureBinary(context.Background(), "fd")
	if err == nil {
		t.Fatal("offline mode must refuse")
	}
	if !errors.Is(err, ErrOffline) {
		t.Errorf("error should say it is offline: %v", err)
	}
}

// PI_OFFLINE is honoured too, so a setup migrating from Pi keeps behaving the
// way its owner configured it.
func TestPiOfflineIsHonoured(t *testing.T) {
	t.Setenv("PI_OFFLINE", "yes")
	if !offline() {
		t.Error("PI_OFFLINE should be honoured")
	}
	t.Setenv("PI_OFFLINE", "0")
	if offline() {
		t.Error("only truthy values mean offline")
	}
}

// tau's own copy wins over a system install: it is the version tau downloaded
// and knows the flags of, where a system one could be old enough to reject them.
func TestLocalBinaryWinsOverPath(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv("TAU_AGENT_DIR", agentDir)
	resolved.Delete("rg")
	t.Cleanup(func() { resolved.Delete("rg") })

	binDir := filepath.Join(agentDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(binDir, "rg"+exeSuffix())
	if err := os.WriteFile(local, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := binaryPath("rg")
	if !ok || got != local {
		t.Errorf("binaryPath returned %q, want tau's own copy at %q", got, local)
	}
}

// The asset names are what make a download land on the right build; a wrong
// one is a 404 the user sees as "could not download".
func TestReleaseAssetNames(t *testing.T) {
	cases := []struct {
		tool, goos, goarch, want string
	}{
		{"rg", "darwin", "arm64", "ripgrep-14.1.0-aarch64-apple-darwin.tar.gz"},
		{"rg", "linux", "amd64", "ripgrep-14.1.0-x86_64-unknown-linux-musl.tar.gz"},
		{"rg", "linux", "arm64", "ripgrep-14.1.0-aarch64-unknown-linux-gnu.tar.gz"},
		{"rg", "windows", "amd64", "ripgrep-14.1.0-x86_64-pc-windows-msvc.zip"},
		{"fd", "darwin", "arm64", "fd-v10.4.2-aarch64-apple-darwin.tar.gz"},
		{"fd", "linux", "amd64", "fd-v10.4.2-x86_64-unknown-linux-gnu.tar.gz"},
	}
	versions := map[string]string{"rg": "14.1.0", "fd": "10.4.2"}

	for _, tc := range cases {
		got := binarySpecs[tc.tool].Asset(versions[tc.tool], tc.goos, tc.goarch)
		if got != tc.want {
			t.Errorf("%s %s/%s: %q, want %q", tc.tool, tc.goos, tc.goarch, got, tc.want)
		}
	}

	// An unsupported platform must say so rather than build a nonsense URL.
	if got := binarySpecs["rg"].Asset("14.1.0", "plan9", "amd64"); got != "" {
		t.Errorf("an unsupported platform produced %q", got)
	}
}
