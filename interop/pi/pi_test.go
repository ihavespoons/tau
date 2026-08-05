package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/session"
)

// piInstall builds a directory shaped like a real ~/.pi/agent.
func piInstall(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	write := func(rel, content string, mode os.FileMode) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}

	write("settings.json", `{"defaultModel":"claude-sonnet-5","compaction":{"keepRecentTokens":9000}}`, 0o644)
	write("auth.json", `{"anthropic":{"type":"oauth","oauth":{"access":"live-token","refresh":"r","expires":1}},"openai":{"type":"api_key","key":"sk-secret"}}`, 0o600)
	write("models.json", `{"providers":{"local":{"baseUrl":"http://127.0.0.1:9/v1","models":[{"id":"m"}]}}}`, 0o644)
	write("models-store.json", `{"radius":{"models":[],"checkedAt":1}}`, 0o644)
	write("skills/review/SKILL.md", "# Review\n", 0o644)
	write("prompts/fix.md", "Fix $1\n", 0o644)

	// A current-format session, and a v1 one from before ids existed.
	write("sessions/--Users-ben-Code-tau--/2024-01-01T00-00-00-000Z_aaa.jsonl", strings.Join([]string{
		`{"type":"session","version":3,"id":"aaa","timestamp":"2024-01-01T00:00:00.000Z","cwd":"/Users/ben/Code/tau"}`,
		`{"type":"message","id":"e1","parentId":null,"timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"user","content":"port the thing"}}`,
		`{"type":"session_info","id":"e2","parentId":"e1","timestamp":"2024-01-01T00:00:02.000Z","name":"The Port"}`,
	}, "\n")+"\n", 0o644)

	write("sessions/--Users-ben-Code-other--/2023-06-01T00-00-00-000Z_bbb.jsonl", strings.Join([]string{
		`{"type":"session","id":"bbb","timestamp":"2023-06-01T00:00:00.000Z","cwd":"/Users/ben/Code/other"}`,
		`{"type":"message","timestamp":"2023-06-01T00:00:01.000Z","message":{"role":"user","content":"an old conversation"}}`,
		`{"type":"message","timestamp":"2023-06-01T00:00:02.000Z","message":{"role":"user","content":"and more of it"}}`,
	}, "\n")+"\n", 0o644)

	return root
}

func TestInspectFindsEverythingWorthImporting(t *testing.T) {
	snap, err := Inspect(piInstall(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("found %d sessions, want 2", len(snap.Sessions))
	}
	if snap.SettingsPath == "" || snap.AuthPath == "" || snap.ModelsPath == "" || snap.ModelsStorePath == "" {
		t.Errorf("missed a config file: %+v", snap)
	}
	if len(snap.Resources) != 2 {
		t.Errorf("resources = %v, want skills and prompts", snap.Resources)
	}
	if len(snap.Problems) != 0 {
		t.Errorf("problems = %v", snap.Problems)
	}
}

// The names say which providers a user would be logging in to again; the
// values are live tokens and must not leave the file.
func TestInspectReportsAuthProviderNamesAndNotTheirSecrets(t *testing.T) {
	snap, err := Inspect(piInstall(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(snap.AuthProviders, ",") != "anthropic,openai" {
		t.Errorf("providers = %v", snap.AuthProviders)
	}
	described := snap.Describe()
	for _, secret := range []string{"live-token", "sk-secret"} {
		if strings.Contains(described, secret) {
			t.Errorf("a credential leaked into the report:\n%s", described)
		}
	}
}

// The version reported is the file's, not the migrated view's — otherwise the
// report would say every session is current and the migration count would look
// like a lie.
func TestInspectReportsTheOnDiskFormatVersion(t *testing.T) {
	snap, err := Inspect(piInstall(t))
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]int{}
	for _, s := range snap.Sessions {
		versions[s.ID] = s.Version
	}
	if versions["aaa"] != 3 {
		t.Errorf("aaa version = %d, want 3", versions["aaa"])
	}
	if versions["bbb"] != 1 {
		t.Errorf("bbb version = %d, want 1 (it has no version field at all)", versions["bbb"])
	}
	if !strings.Contains(snap.Describe(), "need migrating") {
		t.Errorf("the report should say a migration is coming:\n%s", snap.Describe())
	}
}

func TestInspectReadsSessionNamesAndEntryCounts(t *testing.T) {
	snap, err := Inspect(piInstall(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range snap.Sessions {
		switch s.ID {
		case "aaa":
			if s.Name != "The Port" {
				t.Errorf("name = %q, want The Port", s.Name)
			}
			if s.Entries != 2 {
				t.Errorf("entries = %d, want 2", s.Entries)
			}
			if s.Cwd != "/Users/ben/Code/tau" {
				t.Errorf("cwd = %q", s.Cwd)
			}
		case "bbb":
			if s.Entries != 2 {
				t.Errorf("entries = %d, want 2", s.Entries)
			}
		}
	}
}

// "Is there a Pi installation here" is a question, not a failure.
func TestInspectingNothingIsNotAnError(t *testing.T) {
	snap, err := Inspect(filepath.Join(t.TempDir(), "no-such-pi"))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Empty() {
		t.Error("a missing directory has nothing in it")
	}
	if !strings.Contains(snap.Describe(), "nothing to import") {
		t.Errorf("describe = %q", snap.Describe())
	}
}

// One corrupt session must not cost the user the report on the rest.
func TestAnUnreadableSessionIsReportedNotFatal(t *testing.T) {
	root := piInstall(t)
	bad := filepath.Join(root, "sessions", "--broken--", "x.jsonl")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("this is not a session\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 2 {
		t.Errorf("found %d readable sessions, want the 2 good ones", len(snap.Sessions))
	}
	if len(snap.Problems) != 1 {
		t.Errorf("problems = %v, want one", snap.Problems)
	}
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

func destDirs(t *testing.T) (string, string) {
	t.Helper()
	agentDir := t.TempDir()
	return agentDir, filepath.Join(agentDir, "sessions")
}

// The importer's first obligation: leave the source exactly as it found it.
// The two agents have to be able to coexist while the user decides.
func TestImportNeverModifiesThePiInstallation(t *testing.T) {
	source := piInstall(t)
	before := snapshotTree(t, source)

	agentDir, sessionsDir := destDirs(t)
	if _, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir,
		Sessions: true, Settings: true, Models: true, Auth: true, Resources: true,
	}); err != nil {
		t.Fatal(err)
	}

	after := snapshotTree(t, source)
	if len(before) != len(after) {
		t.Fatalf("the source gained or lost files: %d then %d", len(before), len(after))
	}
	for path, content := range before {
		if after[path] != content {
			t.Errorf("the importer modified %s", path)
		}
	}
}

// The session that comes across has to be one tau can actually open, at the
// path tau would have written it to — the cwd encoding is identical, so an
// imported session must be found by a plain `tau --continue` in that directory.
func TestImportedSessionsLandWhereTauLooksForThem(t *testing.T) {
	ctx := context.Background()
	source := piInstall(t)
	agentDir, sessionsDir := destDirs(t)

	report, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir, Sessions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Migrated != 1 {
		t.Errorf("migrated %d sessions, want the one v1 file", report.Migrated)
	}

	repo := session.NewJSONLRepo(sessionsDir)
	metas, err := repo.List(ctx, "/Users/ben/Code/tau")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("tau found %d sessions for that directory, want 1", len(metas))
	}

	opened, err := repo.Open(ctx, metas[0])
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := opened.Name(ctx); !ok || name != "The Port" {
		t.Errorf("name = %q %v", name, ok)
	}
	sctx, err := opened.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sctx.Messages) != 1 {
		t.Errorf("context has %d messages, want 1", len(sctx.Messages))
	}
}

// A v1 session imported without migration would be unopenable, so the
// migration has to happen on the way in rather than on first use.
func TestAV1SessionIsMigratedOnTheWayIn(t *testing.T) {
	ctx := context.Background()
	source := piInstall(t)
	agentDir, sessionsDir := destDirs(t)

	if _, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir, Sessions: true,
	}); err != nil {
		t.Fatal(err)
	}

	repo := session.NewJSONLRepo(sessionsDir)
	metas, err := repo.List(ctx, "/Users/ben/Code/other")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("found %d sessions, want 1", len(metas))
	}
	opened, err := repo.Open(ctx, metas[0])
	if err != nil {
		t.Fatalf("the migrated session did not open: %v", err)
	}
	entries := opened.Entries(ctx, nil)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Base().ID == "" || entries[1].Base().ParentID == nil {
		t.Error("the migrated entries have no tree structure")
	}
}

// Credentials are the one thing that must not come across by accident.
func TestNothingIsImportedUnlessItWasAskedFor(t *testing.T) {
	source := piInstall(t)
	agentDir, sessionsDir := destDirs(t)

	report, err := Import(ImportOptions{Source: source, AgentDir: agentDir, SessionsDir: sessionsDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Copied) != 0 {
		t.Errorf("copied %v with nothing selected", report.Copied)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "auth.json")); err == nil {
		t.Error("credentials were written without being asked for")
	}
}

// An imported credential file is as sensitive as one tau wrote itself, so it
// gets the same permissions regardless of how lax the source was.
func TestImportedCredentialsAreLockedDown(t *testing.T) {
	source := piInstall(t)
	// Make the source world-readable, the mistake the import must not inherit.
	if err := os.Chmod(filepath.Join(source, "auth.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir, sessionsDir := destDirs(t)

	if _, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir, Auth: true,
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(agentDir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("auth.json mode = %o, want 600", perm)
	}
}

// A second import must not be able to undo work done in tau since the first.
func TestAnExistingTauFileIsLeftAloneByDefault(t *testing.T) {
	source := piInstall(t)
	agentDir, sessionsDir := destDirs(t)

	mine := `{"defaultModel":"my-own-choice"}`
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir, Settings: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != mine {
		t.Error("the existing tau settings were overwritten")
	}
	if len(report.Skipped) != 1 {
		t.Errorf("skipped = %v, want one entry explaining why", report.Skipped)
	}
}

func TestOverwriteReplacesTheExistingFile(t *testing.T) {
	source := piInstall(t)
	agentDir, sessionsDir := destDirs(t)

	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir,
		Settings: true, Overwrite: true,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "claude-sonnet-5") {
		t.Errorf("settings were not replaced: %s", data)
	}
}

// A dry run has to report exactly what a real run would, and write nothing.
func TestADryRunWritesNothing(t *testing.T) {
	source := piInstall(t)
	agentDir, sessionsDir := destDirs(t)

	report, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir,
		Sessions: true, Settings: true, Auth: true, Models: true, Resources: true,
		DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Copied) == 0 {
		t.Error("a dry run should still say what it would copy")
	}
	if report.Migrated != 1 {
		t.Errorf("migrated = %d; a dry run should still count the migrations", report.Migrated)
	}

	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a dry run wrote %d entries into the destination", len(entries))
	}
}

func TestImportingIntoTheSourceIsRefused(t *testing.T) {
	source := piInstall(t)
	if _, err := Import(ImportOptions{Source: source, AgentDir: source, Sessions: true}); err == nil {
		t.Error("importing a directory into itself should be refused")
	}
}

func TestResourcesComeAcrossFileByFile(t *testing.T) {
	source := piInstall(t)
	agentDir, sessionsDir := destDirs(t)

	if _, err := Import(ImportOptions{
		Source: source, AgentDir: agentDir, SessionsDir: sessionsDir, Resources: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"skills/review/SKILL.md", "prompts/fix.md"} {
		if _, err := os.Stat(filepath.Join(agentDir, rel)); err != nil {
			t.Errorf("%s did not come across: %v", rel, err)
		}
	}
}

// snapshotTree reads every file under root into a map for comparison.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
