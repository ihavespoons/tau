package pkgmgr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeRunner stands in for npm and git: it records what it was asked to do and
// makes the filesystem look the way the real tool would have left it.
type fakeRunner struct {
	calls []string
	fail  error
}

func (f *fakeRunner) run(_ context.Context, dir, name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " ")+" (in "+dir+")")
	if f.fail != nil {
		return "", f.fail
	}
	switch {
	case name == "npm" && len(args) > 1 && args[0] == "install":
		spec := args[len(args)-1]
		pkgName, _ := parseNPMSpec(spec)
		_ = os.MkdirAll(filepath.Join(dir, "node_modules", filepath.FromSlash(pkgName)), 0o755)
	case name == "npm" && len(args) > 1 && args[0] == "uninstall":
		_ = os.RemoveAll(filepath.Join(dir, "node_modules", filepath.FromSlash(args[len(args)-1])))
	case name == "git" && len(args) > 0 && args[0] == "clone":
		_ = os.MkdirAll(filepath.Join(args[len(args)-1], ".git"), 0o755)
	}
	return "", nil
}

func testManager(t *testing.T, trusted bool) (*Manager, *fakeRunner, string, string) {
	t.Helper()
	agentDir := t.TempDir()
	cwd := t.TempDir()
	run := &fakeRunner{}
	m := New(Options{AgentDir: agentDir, Cwd: cwd, ProjectTrusted: trusted, Run: run.run})
	return m, run, agentDir, cwd
}

// A project must not be able to install anything until the user trusts it:
// cloning a repository would otherwise be enough to make tau fetch and run code.
func TestProjectScopeRequiresTrust(t *testing.T) {
	m, _, _, _ := testManager(t, false)

	if _, _, err := m.Install(context.Background(), "npm:pkg", ScopeProject); !errors.Is(err, ErrUntrusted) {
		t.Errorf("Install: err = %v, want ErrUntrusted", err)
	}
	if _, err := m.InstallRoot(KindNPM, ScopeProject); !errors.Is(err, ErrUntrusted) {
		t.Errorf("InstallRoot: err = %v, want ErrUntrusted", err)
	}
	if err := m.Remove(context.Background(), "npm:pkg", ScopeProject); !errors.Is(err, ErrUntrusted) {
		t.Errorf("Remove: err = %v, want ErrUntrusted", err)
	}
	res := m.Resolve([]Entry{{Source: "npm:pkg"}}, ScopeProject)
	if len(res.Resources) != 0 || len(res.Warnings) != 1 {
		t.Errorf("Resolve: resources=%v warnings=%v", res.Resources, res.Warnings)
	}

	// User scope is unaffected — trust is about the checkout, not the user.
	if _, err := m.InstallRoot(KindNPM, ScopeUser); err != nil {
		t.Errorf("user scope: %v", err)
	}
}

func TestPackagePath(t *testing.T) {
	m, _, agentDir, cwd := testManager(t, true)

	got, err := m.PackagePath(ParseSource("npm:@scope/pkg@1.0.0"), ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(agentDir, "npm", "node_modules", "@scope", "pkg")
	if got != want {
		t.Errorf("npm: got %q, want %q", got, want)
	}

	got, err = m.PackagePath(ParseSource("git:github.com/o/r"), ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(cwd, ".tau", "git", "github.com", "o", "r")
	if got != want {
		t.Errorf("git: got %q, want %q", got, want)
	}

	got, err = m.PackagePath(ParseSource("./local/pkg"), ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "local", "pkg"); got != want {
		t.Errorf("local: got %q, want %q", got, want)
	}
}

func TestManagedPathRefusesEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := managedPath(root, "..", "elsewhere"); err == nil {
		t.Error("traversal accepted")
	}
	if _, err := managedPath(root, "ok", "nested"); err != nil {
		t.Errorf("legitimate path rejected: %v", err)
	}
}

func TestInstallNPM(t *testing.T) {
	m, run, agentDir, _ := testManager(t, true)

	src, path, err := m.Install(context.Background(), "npm:pkg@1.2.3", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != KindNPM || src.Name != "pkg" {
		t.Fatalf("src = %+v", src)
	}
	if want := filepath.Join(agentDir, "npm", "node_modules", "pkg"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if len(run.calls) != 1 || !strings.Contains(run.calls[0], "npm install") {
		t.Fatalf("calls = %v", run.calls)
	}
	if !strings.Contains(run.calls[0], "pkg@1.2.3") {
		t.Errorf("spec not passed through: %v", run.calls)
	}

	// Without a package.json of its own, npm walks upward and installs into
	// whatever it finds — possibly the user's home directory.
	manifest := filepath.Join(agentDir, "npm", "package.json")
	if !exists(manifest) {
		t.Fatal("install root has no package.json")
	}
	var pkg map[string]any
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg["private"] != true {
		t.Errorf("install root is not private: %v", pkg)
	}
}

func TestInstallGit(t *testing.T) {
	m, run, agentDir, _ := testManager(t, true)

	_, path, err := m.Install(context.Background(), "git:https://github.com/o/r#v1", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(agentDir, "git", "github.com", "o", "r"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if len(run.calls) != 2 {
		t.Fatalf("calls = %v", run.calls)
	}
	if !strings.HasPrefix(run.calls[0], "git clone ") {
		t.Errorf("first call = %q", run.calls[0])
	}
	if !strings.HasPrefix(run.calls[1], "git checkout v1") {
		t.Errorf("second call = %q", run.calls[1])
	}

	// Installing again syncs the existing checkout rather than re-cloning.
	run.calls = nil
	if _, _, err := m.Install(context.Background(), "git:https://github.com/o/r#v1", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 2 || !strings.HasPrefix(run.calls[0], "git fetch") {
		t.Errorf("calls = %v", run.calls)
	}
}

func TestInstallLocalRequiresDirectory(t *testing.T) {
	m, run, _, cwd := testManager(t, true)

	if _, _, err := m.Install(context.Background(), "./nope", ScopeUser); err == nil {
		t.Error("missing directory accepted")
	}
	if err := os.MkdirAll(filepath.Join(cwd, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, path, err := m.Install(context.Background(), "./pkg", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "pkg"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	// A local package is used where it lies, so nothing is fetched or copied.
	if len(run.calls) != 0 {
		t.Errorf("local install ran commands: %v", run.calls)
	}
}

func TestUpdateLeavesPinnedAlone(t *testing.T) {
	m, run, _, _ := testManager(t, true)

	changed, err := m.Update(context.Background(), "npm:pkg@1.2.3", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a pinned package was updated")
	}
	if len(run.calls) != 0 {
		t.Errorf("calls = %v", run.calls)
	}

	changed, err = m.Update(context.Background(), "npm:pkg", ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(run.calls) != 1 || !strings.Contains(run.calls[0], "pkg@latest") {
		t.Errorf("changed=%v calls=%v", changed, run.calls)
	}
}

func TestRemove(t *testing.T) {
	m, run, agentDir, cwd := testManager(t, true)
	ctx := context.Background()

	if _, _, err := m.Install(ctx, "npm:pkg", ScopeUser); err != nil {
		t.Fatal(err)
	}
	run.calls = nil
	if err := m.Remove(ctx, "npm:pkg", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 1 || !strings.HasPrefix(run.calls[0], "npm uninstall ") || !strings.Contains(run.calls[0], " pkg ") {
		t.Errorf("calls = %v", run.calls)
	}

	if _, _, err := m.Install(ctx, "git:https://github.com/o/r", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "git:https://github.com/o/r", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(agentDir, "git", "github.com")) {
		t.Error("empty host directory left behind")
	}
	// Pruning stops at the install root: removing the last package must not
	// take tau's own directory with it.
	if !isDir(filepath.Join(agentDir, "git")) {
		t.Error("install root was pruned away")
	}

	// A local package is the user's own work; tau did not put it there and must
	// not delete it.
	local := filepath.Join(cwd, "mine")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "./mine", ScopeUser); err != nil {
		t.Fatal(err)
	}
	if !isDir(local) {
		t.Error("local package deleted")
	}
}

func TestList(t *testing.T) {
	m, _, _, _ := testManager(t, true)
	ctx := context.Background()

	for _, s := range []string{"npm:pkg", "npm:@scope/other", "git:https://github.com/o/r"} {
		if _, _, err := m.Install(ctx, s, ScopeUser); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	installed, err := m.List(ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, i := range installed {
		sources = append(sources, i.Source)
	}
	want := []string{"git:github.com/o/r", "npm:@scope/other", "npm:pkg"}
	if !reflect.DeepEqual(sources, want) {
		t.Errorf("got %v, want %v", sources, want)
	}
}

func TestParseEntry(t *testing.T) {
	e, err := ParseEntry(json.RawMessage(`"npm:pkg"`))
	if err != nil || e.Source != "npm:pkg" || e.Autoload != nil {
		t.Fatalf("string form: %+v %v", e, err)
	}

	e, err = ParseEntry(json.RawMessage(`{"source":"npm:pkg","autoload":false,"skills":["+a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Source != "npm:pkg" || e.Autoload == nil || *e.Autoload {
		t.Fatalf("object form: %+v", e)
	}
	if patterns, declared := e.Patterns(TypeSkills); !declared || !reflect.DeepEqual(patterns, []string{"+a"}) {
		t.Errorf("skills = %v %v", patterns, declared)
	}
	if _, declared := e.Patterns(TypeThemes); declared {
		t.Error("themes should not be declared")
	}

	if _, err := ParseEntry(json.RawMessage(`{"skills":["a"]}`)); err == nil {
		t.Error("entry without a source accepted")
	}
}

// One malformed entry must not cost the user every other package.
func TestParseEntriesKeepsGoing(t *testing.T) {
	entries, warnings := ParseEntries([]json.RawMessage{
		json.RawMessage(`"npm:a"`),
		json.RawMessage(`{"nope":1}`),
		json.RawMessage(`"npm:b"`),
	})
	if len(entries) != 2 || entries[0].Source != "npm:a" || entries[1].Source != "npm:b" {
		t.Errorf("entries = %+v", entries)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v", warnings)
	}
}

// fixturePackage installs a package by hand and returns the source string that
// names it.
func fixturePackage(t *testing.T, m *Manager, scope Scope, files map[string]string) string {
	t.Helper()
	root, err := m.InstallRoot(KindGit, scope)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "github.com", "o", "r")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, files)
	return "git:github.com/o/r"
}

func resolvedRel(t *testing.T, res Resolution, root string, ty ResourceType) []string {
	t.Helper()
	var paths []string
	for _, r := range res.Resources {
		if r.Type == ty && r.Enabled {
			paths = append(paths, r.Path)
		}
	}
	return relSorted(t, root, paths)
}

func TestResolve(t *testing.T) {
	m, _, agentDir, _ := testManager(t, true)
	source := fixturePackage(t, m, ScopeUser, map[string]string{
		"skills/deploy/SKILL.md": "s",
		"skills/review/SKILL.md": "s",
		"prompts/a.md":           "p",
		"themes/dark.json":       "{}",
	})
	root := filepath.Join(agentDir, "git", "github.com", "o", "r")
	no := false

	t.Run("everything by default", func(t *testing.T) {
		res := m.Resolve([]Entry{{Source: source}}, ScopeUser)
		if len(res.Warnings) != 0 {
			t.Fatalf("warnings = %v", res.Warnings)
		}
		want := []string{"skills/deploy/SKILL.md", "skills/review/SKILL.md"}
		if got := resolvedRel(t, res, root, TypeSkills); !reflect.DeepEqual(got, want) {
			t.Errorf("skills = %v, want %v", got, want)
		}
		if got := resolvedRel(t, res, root, TypeThemes); len(got) != 1 {
			t.Errorf("themes = %v", got)
		}
	})

	t.Run("patterns select", func(t *testing.T) {
		res := m.Resolve([]Entry{{Source: source, Skills: []string{"!review"}}}, ScopeUser)
		want := []string{"skills/deploy/SKILL.md"}
		if got := resolvedRel(t, res, root, TypeSkills); !reflect.DeepEqual(got, want) {
			t.Errorf("skills = %v, want %v", got, want)
		}
		// Types the entry did not mention are untouched.
		if got := resolvedRel(t, res, root, TypePrompts); len(got) != 1 {
			t.Errorf("prompts = %v", got)
		}
	})

	t.Run("empty list disables the type", func(t *testing.T) {
		res := m.Resolve([]Entry{{Source: source, Skills: []string{}}}, ScopeUser)
		if got := resolvedRel(t, res, root, TypeSkills); len(got) != 0 {
			t.Errorf("skills = %v, want none", got)
		}
		if got := resolvedRel(t, res, root, TypePrompts); len(got) != 1 {
			t.Errorf("prompts = %v", got)
		}
	})

	// autoload:false is how a user takes one skill from a package they would
	// otherwise rather not load at all.
	t.Run("autoload false opts in", func(t *testing.T) {
		res := m.Resolve([]Entry{{Source: source, Autoload: &no, Skills: []string{"+skills/deploy"}}}, ScopeUser)
		want := []string{"skills/deploy/SKILL.md"}
		if got := resolvedRel(t, res, root, TypeSkills); !reflect.DeepEqual(got, want) {
			t.Errorf("skills = %v, want %v", got, want)
		}
		if got := resolvedRel(t, res, root, TypePrompts); len(got) != 0 {
			t.Errorf("prompts = %v, want none", got)
		}
	})

	t.Run("uninstalled package warns", func(t *testing.T) {
		res := m.Resolve([]Entry{{Source: "git:github.com/o/absent"}}, ScopeUser)
		if len(res.Resources) != 0 {
			t.Errorf("resources = %v", res.Resources)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "not installed") {
			t.Errorf("warnings = %v", res.Warnings)
		}
	})
}

// Project resources outrank user ones, the same way the rest of settings work.
func TestResolvePriorityByScope(t *testing.T) {
	m, _, _, _ := testManager(t, true)
	files := map[string]string{"skills/a/SKILL.md": "s"}
	userSrc := fixturePackage(t, m, ScopeUser, files)
	projSrc := fixturePackage(t, m, ScopeProject, files)

	user := m.Resolve([]Entry{{Source: userSrc}}, ScopeUser)
	proj := m.Resolve([]Entry{{Source: projSrc}}, ScopeProject)
	if len(user.Resources) == 0 || len(proj.Resources) == 0 {
		t.Fatal("nothing resolved")
	}
	if proj.Resources[0].Priority >= user.Resources[0].Priority {
		t.Errorf("project priority %d should beat user priority %d",
			proj.Resources[0].Priority, user.Resources[0].Priority)
	}
}

func TestFilterPaths(t *testing.T) {
	base := filepath.FromSlash("/home/agent")
	all := paths(base, "skills/deploy/SKILL.md", "skills/review/SKILL.md")

	if got := FilterPaths(all, nil, base); !reflect.DeepEqual(got, all) {
		t.Errorf("no patterns changed the list: %v", got)
	}
	got := FilterPaths(all, []string{"!review"}, base)
	if len(got) != 1 || got[0] != all[0] {
		t.Errorf("got %v, want just deploy", got)
	}
}

func TestSplitEntries(t *testing.T) {
	pathsOut, patterns := SplitEntries([]string{"a/SKILL.md", "!b", "-c"})
	if !reflect.DeepEqual(pathsOut, []string{"a/SKILL.md"}) {
		t.Errorf("paths = %v", pathsOut)
	}
	if !reflect.DeepEqual(patterns, []string{"!b", "-c"}) {
		t.Errorf("patterns = %v", patterns)
	}
}
