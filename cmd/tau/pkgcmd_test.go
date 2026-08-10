package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/config"
)

// newPkgProject sets up a scratch project with a local package in it, and
// points tau's global state at a temp directory so the test never touches the
// real ~/.tau.
func newPkgProject(t *testing.T) string {
	t.Helper()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "pkg", "package.json"), `{"name":"demo","version":"1.0.0"}`)
	writeFile(t, filepath.Join(cwd, "pkg", "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: ship it\n---\n\nbody\n")
	writeFile(t, filepath.Join(cwd, "pkg", "themes", "midnight.json"), `{"name":"midnight"}`)
	t.Chdir(cwd)
	return cwd
}

// makeGated plants a project-local resource so the directory has something
// worth gating. A project with no .tau resources at all is trusted by default
// — there is nothing there to run — so the trust gate only has an observable
// effect once the checkout carries one of them.
func makeGated(t *testing.T, cwd string) {
	t.Helper()
	writeFile(t, filepath.Join(cwd, config.DirName, "skills", "local", "SKILL.md"),
		"---\nname: local\ndescription: from the checkout\n---\n\nbody\n")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readPackages returns the package sources configured in a settings file, or
// nil when the file or the key is absent.
func readPackages(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Packages []json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("settings at %s is not valid json: %v\n%s", path, err, b)
	}
	var out []string
	for _, raw := range doc.Packages {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out = append(out, s)
			continue
		}
		var obj struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("package entry %s: %v", raw, err)
		}
		out = append(out, obj.Source)
	}
	return out
}

// captureStdout runs fn with stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	runErr := fn()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, runErr
}

func TestInstallLocalPackageUserScope(t *testing.T) {
	newPkgProject(t)

	if _, err := captureStdout(t, func() error { return installCmd([]string{"./pkg"}) }); err != nil {
		t.Fatal(err)
	}

	got := readPackages(t, config.SettingsPath())
	if len(got) != 1 || got[0] != "./pkg" {
		t.Fatalf("global packages = %v, want [./pkg]", got)
	}
}

// Installing the same package twice must configure it once. Pi's own CLI
// replaces the entry rather than appending, and a duplicate here would mean the
// package's skills collide with themselves on every load.
func TestInstallIsIdempotent(t *testing.T) {
	newPkgProject(t)

	for i := 0; i < 2; i++ {
		if _, err := captureStdout(t, func() error { return installCmd([]string{"./pkg"}) }); err != nil {
			t.Fatal(err)
		}
	}
	if got := readPackages(t, config.SettingsPath()); len(got) != 1 {
		t.Fatalf("global packages = %v, want one entry", got)
	}
}

// An entry the user expanded into the object form carries their own per-resource
// filters. Reinstalling rewrites the source and must leave the rest alone.
func TestInstallPreservesObjectEntryFields(t *testing.T) {
	newPkgProject(t)
	writeFile(t, config.SettingsPath(),
		`{"packages":[{"source":"./pkg","skills":["deploy"],"autoload":false}]}`)

	if _, err := captureStdout(t, func() error { return installCmd([]string{"./pkg"}) }); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(config.SettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Packages []struct {
			Source   string   `json:"source"`
			Skills   []string `json:"skills"`
			Autoload *bool    `json:"autoload"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Packages) != 1 {
		t.Fatalf("packages = %s", b)
	}
	e := doc.Packages[0]
	if e.Source != "./pkg" || len(e.Skills) != 1 || e.Skills[0] != "deploy" {
		t.Errorf("entry = %+v, want the filters preserved", e)
	}
	if e.Autoload == nil || *e.Autoload {
		t.Errorf("autoload = %v, want it preserved as false", e.Autoload)
	}
}

func TestRemovePackage(t *testing.T) {
	newPkgProject(t)
	if _, err := captureStdout(t, func() error { return installCmd([]string{"./pkg"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return removeCmd([]string{"./pkg"}) }); err != nil {
		t.Fatal(err)
	}

	if got := readPackages(t, config.SettingsPath()); len(got) != 0 {
		t.Fatalf("global packages = %v, want none", got)
	}
	// A local package is the user's own directory; unconfiguring it must not
	// delete it.
	if _, err := os.Stat("pkg"); err != nil {
		t.Errorf("the local package directory was removed: %v", err)
	}
}

func TestRemoveUnknownPackageFails(t *testing.T) {
	newPkgProject(t)
	if _, err := captureStdout(t, func() error { return removeCmd([]string{"npm:nothing-here"}) }); err == nil {
		t.Fatal("removing a package that was never installed should fail")
	}
}

// The project scope is behind the trust gate: installing into .tau means
// writing to the checkout and then running what is written.
func TestLocalInstallRefusedInUntrustedProject(t *testing.T) {
	cwd := newPkgProject(t)
	makeGated(t, cwd)

	_, err := captureStdout(t, func() error { return installCmd([]string{"-l", "./pkg"}) })
	if err == nil {
		t.Fatal("a local install into an untrusted project should fail")
	}
	if !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("error = %v, want it to name the trust gate", err)
	}
	if _, statErr := os.Stat(filepath.Join(cwd, config.DirName, "settings.json")); statErr == nil {
		t.Error("an untrusted project's settings.json was written anyway")
	}
}

func TestLocalInstallWithApprove(t *testing.T) {
	cwd := newPkgProject(t)
	makeGated(t, cwd)

	if _, err := captureStdout(t, func() error {
		return installCmd([]string{"-l", "-approve", "./pkg"})
	}); err != nil {
		t.Fatal(err)
	}

	got := readPackages(t, filepath.Join(cwd, config.DirName, "settings.json"))
	if len(got) != 1 || got[0] != "./pkg" {
		t.Fatalf("project packages = %v, want [./pkg]", got)
	}
	if global := readPackages(t, config.SettingsPath()); len(global) != 0 {
		t.Errorf("global packages = %v, want the entry only in the project scope", global)
	}
}

func TestContradictoryTrustFlags(t *testing.T) {
	newPkgProject(t)
	_, err := captureStdout(t, func() error {
		return installCmd([]string{"-approve", "-no-approve", "./pkg"})
	})
	if err == nil {
		t.Fatal("-approve with -no-approve should fail rather than pick one")
	}
}

func TestPackagesListing(t *testing.T) {
	newPkgProject(t)

	out, err := captureStdout(t, func() error { return packagesCmd(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No packages installed.") {
		t.Errorf("empty listing = %q", out)
	}

	if _, err := captureStdout(t, func() error { return installCmd([]string{"./pkg"}) }); err != nil {
		t.Fatal(err)
	}
	out, err = captureStdout(t, func() error { return packagesCmd(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "User packages") || !strings.Contains(out, "./pkg") {
		t.Errorf("listing = %q", out)
	}
}

// A pinned source is left where it is: the user asked for that exact version.
func TestUpdateLeavesLocalPackagesAlone(t *testing.T) {
	newPkgProject(t)
	if _, err := captureStdout(t, func() error { return installCmd([]string{"./pkg"}) }); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return updateCmd(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "./pkg") {
		t.Errorf("update output = %q, want it to mention the package", out)
	}
}

func TestUpdateWithNothingConfigured(t *testing.T) {
	newPkgProject(t)
	out, err := captureStdout(t, func() error { return updateCmd(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No packages configured.") {
		t.Errorf("update output = %q", out)
	}
}
