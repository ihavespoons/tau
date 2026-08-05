package exthost

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeExec(t *testing.T, path, content string) string {
	t.Helper()
	write(t, path, content)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClassifiesTypeScriptFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.ts", "b.js", "c.mts", "d.cjs"} {
		c, err := classify(write(t, filepath.Join(dir, name), ""), "user")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Kind != KindTypeScript {
			t.Fatalf("%s classified as %q", name, c.Kind)
		}
	}
}

func TestClassifiesExecutableAsNative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not carry the same meaning on Windows")
	}
	p := writeExec(t, filepath.Join(t.TempDir(), "myext"), "#!/bin/sh\n")
	c, err := classify(p, "user")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if c.Kind != KindNative || c.Entry != p {
		t.Fatalf("candidate = %+v", c)
	}
}

// A shared library is refused rather than attempted: Go plugins require the
// host and the plugin to be built by the same toolchain against identical
// dependency versions, and the failure mode is a crash, not a message.
func TestSharedLibrariesAreRefusedWithAnExplanation(t *testing.T) {
	for _, name := range []string{"x.so", "x.dylib", "x.dll"} {
		_, err := classify(write(t, filepath.Join(t.TempDir(), name), ""), "user")
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
		if !strings.Contains(err.Error(), "wire protocol") {
			t.Fatalf("%s: err does not say what to do instead: %v", name, err)
		}
	}
}

func TestNonExecutableFileIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not carry the same meaning on Windows")
	}
	_, err := classify(write(t, filepath.Join(t.TempDir(), "notes.txt"), "hi"), "user")
	if err == nil {
		t.Fatal("a plain data file was accepted as an extension")
	}
}

func TestPackageDirectoryUsesTauKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	write(t, filepath.Join(dir, "package.json"),
		`{"name":"my-ext","main":"main.js","tau":{"extension":"tau-entry.ts"}}`)

	c, err := classify(dir, "user")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if c.Name != "my-ext" {
		t.Fatalf("name = %q", c.Name)
	}
	if c.Entry != filepath.Join(dir, "tau-entry.ts") {
		t.Fatalf("entry = %q", c.Entry)
	}
}

// A Pi extension published before tau existed must be discoverable unedited.
func TestPackageDirectoryAcceptsPiKeyAsAnAlias(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	write(t, filepath.Join(dir, "package.json"),
		`{"name":"pi-ext","pi":{"extension":"pi-entry.ts"}}`)

	c, err := classify(dir, "user")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if c.Entry != filepath.Join(dir, "pi-entry.ts") {
		t.Fatalf("entry = %q", c.Entry)
	}
}

// An author who wrote both meant the specific one.
func TestTauKeyWinsOverPiKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	write(t, filepath.Join(dir, "package.json"),
		`{"name":"both","tau":{"extension":"tau.ts"},"pi":{"extension":"pi.ts"}}`)

	c, err := classify(dir, "user")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if filepath.Base(c.Entry) != "tau.ts" {
		t.Fatalf("entry = %q", c.Entry)
	}
}

func TestPackageDirectoryFallsBackToMain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	write(t, filepath.Join(dir, "package.json"), `{"name":"plain","main":"index.js"}`)
	c, err := classify(dir, "user")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if filepath.Base(c.Entry) != "index.js" {
		t.Fatalf("entry = %q", c.Entry)
	}
}

func TestPackageDirectoryWithNoEntryPointIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	write(t, filepath.Join(dir, "package.json"), `{"name":"empty"}`)
	_, err := classify(dir, "user")
	if err == nil || !strings.Contains(err.Error(), "entry point") {
		t.Fatalf("err = %v", err)
	}
}

func TestNativePackageSkipsTheShim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg")
	write(t, filepath.Join(dir, "package.json"), `{"name":"n","tau":{"native":"bin/ext"}}`)
	c, err := classify(dir, "user")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if c.Kind != KindNative {
		t.Fatalf("kind = %q", c.Kind)
	}
}

// Cloning a repository must not be enough to make tau execute what is in it.
func TestProjectDirectoryIsGatedOnTrust(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, ".tau", "extensions")
	write(t, filepath.Join(proj, "evil.ts"), "")

	got, errs := Discover(DiscoverOptions{ProjectDir: proj, Trusted: false, Cwd: root})
	if len(got) != 0 {
		t.Fatalf("untrusted project extensions were discovered: %+v", got)
	}
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}

	got, _ = Discover(DiscoverOptions{ProjectDir: proj, Trusted: true, Cwd: root})
	if len(got) != 1 {
		t.Fatalf("trusted project extensions were not discovered: %+v", got)
	}
	if got[0].Source != "project" {
		t.Fatalf("source = %q", got[0].Source)
	}
}

func TestDiscoveryOrderIsFlagsThenSettingsThenUserThenProject(t *testing.T) {
	root := t.TempDir()
	flag := write(t, filepath.Join(root, "flag.ts"), "")
	set := write(t, filepath.Join(root, "set.ts"), "")
	userDir := filepath.Join(root, "user")
	write(t, filepath.Join(userDir, "u.ts"), "")
	projDir := filepath.Join(root, "proj")
	write(t, filepath.Join(projDir, "p.ts"), "")

	got, errs := Discover(DiscoverOptions{
		Flags: []string{flag}, Settings: []string{set},
		UserDir: userDir, ProjectDir: projDir, Trusted: true, Cwd: root,
	})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	want := []string{"flag", "set", "u", "p"}
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v want %v", names, want)
		}
	}
}

// The same file named twice is one extension. Loading it twice would dispatch
// every event to it twice, and a gate would vote twice.
func TestDuplicatePathsAreLoadedOnce(t *testing.T) {
	root := t.TempDir()
	p := write(t, filepath.Join(root, "e.ts"), "")

	got, _ := Discover(DiscoverOptions{
		Flags: []string{p}, Settings: []string{p, "./e.ts"}, Cwd: root,
	})
	if len(got) != 1 {
		t.Fatalf("got %d candidates for one file: %+v", len(got), got)
	}
	if got[0].Source != "flag" {
		t.Fatalf("the nearest source did not win: %q", got[0].Source)
	}
}

func TestScanSkipsDotfilesAndNodeModules(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".hidden.ts"), "")
	write(t, filepath.Join(dir, "node_modules", "dep", "package.json"), `{"name":"d"}`)
	write(t, filepath.Join(dir, "real.ts"), "")
	write(t, filepath.Join(dir, "README.md"), "")

	got, errs := Discover(DiscoverOptions{UserDir: dir, Cwd: dir})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(got) != 1 || got[0].Name != "real" {
		t.Fatalf("got %+v", got)
	}
}

func TestMissingDirectoryIsNotAnError(t *testing.T) {
	_, errs := Discover(DiscoverOptions{UserDir: filepath.Join(t.TempDir(), "nope"), Cwd: "."})
	if len(errs) != 0 {
		t.Fatalf("a missing extensions directory was reported as an error: %v", errs)
	}
}

// tau never runs npx on an extension's behalf: that would fetch and execute
// code from the network as a side effect of opening a directory.
func TestMissingShimIsReportedNotInstalled(t *testing.T) {
	c := Candidate{Path: "/x/e.ts", Entry: "/x/e.ts", Kind: KindTypeScript, Name: "e"}
	_, err := SpecFor(c, func(string) (string, error) { return "", errors.New("not found") })
	if !errors.Is(err, ErrShimMissing) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), ShimPackage) {
		t.Fatalf("the error does not name the package to install: %v", err)
	}
}

func TestSpecForTypeScriptRunsTheShim(t *testing.T) {
	c := Candidate{Path: "/x/e.ts", Entry: "/x/e.ts", Kind: KindTypeScript, Name: "e"}
	spec, err := SpecFor(c, func(string) (string, error) { return "/usr/bin/tau-pi-host", nil })
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if spec.Command != "/usr/bin/tau-pi-host" || len(spec.Args) != 1 || spec.Args[0] != "/x/e.ts" {
		t.Fatalf("spec = %+v", spec)
	}
}

// A native extension must never be routed through the shim, which would try to
// import a binary as JavaScript.
func TestSpecForNativeRunsItDirectly(t *testing.T) {
	c := Candidate{Path: "/x/ext", Entry: "/x/ext", Kind: KindNative, Name: "ext"}
	spec, err := SpecFor(c, func(string) (string, error) {
		t.Fatal("the shim was looked up for a native extension")
		return "", nil
	})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if spec.Command != "/x/ext" || len(spec.Args) != 0 {
		t.Fatalf("spec = %+v", spec)
	}
}
