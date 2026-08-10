package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env/osenv"
	"github.com/ihavespoons/tau/ai"
)

func newTestTools(t *testing.T) (map[string]agent.Tool, string) {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	e, err := osenv.New(osenv.Options{Cwd: dir})
	if err != nil {
		t.Fatalf("osenv.New: %v", err)
	}
	byName := map[string]agent.Tool{}
	for _, tool := range CodingTools(e, nil) {
		byName[tool.Def().Name] = tool
	}
	return byName, dir
}

// run invokes a tool the way the agent loop does: JSON args in, result out.
func run(t *testing.T, tool agent.Tool, args any) (agent.ToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), "call-1", raw, nil)
}

func resultText(res agent.ToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if txt, ok := c.(ai.TextContent); ok {
			b.WriteString(txt.Text)
		}
	}
	return b.String()
}

func TestToolSchemas(t *testing.T) {
	tools, _ := newTestTools(t)
	want := map[string][]string{
		"read":  {"path"},
		"bash":  {"command"},
		"edit":  {"path", "edits"},
		"write": {"path", "content"},
	}
	for name, required := range want {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("tool %q missing from CodingTools", name)
		}
		def := tool.Def()
		if def.Parameters == nil {
			t.Fatalf("%s: nil schema", name)
		}
		if def.Label == "" || def.Description == "" {
			t.Errorf("%s: label=%q description empty=%v", name, def.Label, def.Description == "")
		}
		got := map[string]bool{}
		for _, r := range def.Parameters.Required {
			got[r] = true
		}
		for _, r := range required {
			if !got[r] {
				t.Errorf("%s: schema is missing required field %q (got %v)",
					name, r, def.Parameters.Required)
			}
		}
		// A tool the model can't fill in correctly is worse than no tool.
		for _, prop := range def.Parameters.Properties {
			if prop.Description == "" {
				t.Errorf("%s: a property has no description", name)
			}
		}
	}
}

func TestReadTool(t *testing.T) {
	tools, dir := newTestTools(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("l1\nl2\nl3"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := run(t, tools["read"], ReadParams{Path: "f.txt"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := resultText(res); got != "l1\nl2\nl3" {
		t.Errorf("read = %q, want raw content with no line numbers", got)
	}

	res, err = run(t, tools["read"], ReadParams{Path: "f.txt", Offset: 2, Limit: 1})
	if err != nil {
		t.Fatalf("read offset/limit: %v", err)
	}
	text := resultText(res)
	if !strings.HasPrefix(text, "l2") {
		t.Errorf("windowed read = %q", text)
	}
	if !strings.Contains(text, "more lines in file") {
		t.Errorf("expected a continuation hint, got %q", text)
	}
}

func TestReadToolErrors(t *testing.T) {
	tools, dir := newTestTools(t)

	if _, err := run(t, tools["read"], ReadParams{Path: "missing.txt"}); err == nil {
		t.Error("expected an error for a missing file")
	}
	if _, err := run(t, tools["read"], ReadParams{Path: ""}); err == nil {
		t.Error("expected an error for an empty path")
	}

	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte{0x00, 0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, tools["read"], ReadParams{Path: "b.bin"}); err == nil {
		t.Error("expected an error for a binary file")
	}

	if err := os.WriteFile(filepath.Join(dir, "s.txt"), []byte("a\nb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, tools["read"], ReadParams{Path: "s.txt", Offset: 99}); err == nil {
		t.Error("expected an error when offset is beyond EOF")
	}
}

func TestReadToolImage(t *testing.T) {
	tools, dir := newTestTools(t)
	gif := []byte("GIF89a" + strings.Repeat("\x00", 20))
	if err := os.WriteFile(filepath.Join(dir, "a.gif"), gif, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := run(t, tools["read"], ReadParams{Path: "a.gif"})
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	var sawImage bool
	for _, c := range res.Content {
		if img, ok := c.(ai.ImageContent); ok {
			sawImage = true
			if img.MimeType != "image/gif" {
				t.Errorf("mimeType = %q", img.MimeType)
			}
			if img.Data == "" {
				t.Error("image data is empty")
			}
		}
	}
	if !sawImage {
		t.Error("expected an ImageContent block")
	}
}

func TestWriteTool(t *testing.T) {
	tools, dir := newTestTools(t)

	res, err := run(t, tools["write"], WriteParams{Path: "a/b/f.txt", Content: "hello"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(resultText(res), "5 bytes") {
		t.Errorf("result = %q, want a byte count", resultText(res))
	}
	got, err := os.ReadFile(filepath.Join(dir, "a/b/f.txt"))
	if err != nil {
		t.Fatalf("parent directories were not created: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("file = %q", got)
	}

	if _, err := run(t, tools["write"], WriteParams{Path: "", Content: "x"}); err == nil {
		t.Error("expected an error for an empty path")
	}
}

func TestEditTool(t *testing.T) {
	tools, dir := newTestTools(t)
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := run(t, tools["edit"], EditParams{
		Path:  "f.txt",
		Edits: []Edit{{OldText: "alpha", NewText: "ALPHA"}, {OldText: "gamma", NewText: "GAMMA"}},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(resultText(res), "2 block(s)") {
		t.Errorf("result = %q", resultText(res))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ALPHA\nbeta\nGAMMA\n" {
		t.Errorf("file = %q", got)
	}

	details, ok := res.Details.(*EditDetails)
	if !ok {
		t.Fatalf("details = %T, want *EditDetails", res.Details)
	}
	if details.Diff == "" || details.Patch == "" {
		t.Error("expected both a diff and a patch in details")
	}
}

func TestEditToolUniquenessErrors(t *testing.T) {
	tools, dir := newTestTools(t)
	path := filepath.Join(dir, "f.txt")
	original := "dup\nunique\ndup\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		edits   []Edit
		wantErr string
	}{
		{"zero matches", []Edit{{OldText: "absent", NewText: "x"}}, "could not find"},
		{"multiple matches", []Edit{{OldText: "dup", NewText: "x"}}, "found 2 occurrences"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, tools["edit"], EditParams{Path: "f.txt", Edits: tc.edits})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			// A rejected edit must leave the file byte-identical.
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != original {
				t.Errorf("file was modified by a failed edit: %q", got)
			}
		})
	}
}

func TestEditToolEmptyEdits(t *testing.T) {
	tools, dir := newTestTools(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, tools["edit"], EditParams{Path: "f.txt", Edits: nil}); err == nil {
		t.Error("expected an error when edits is empty")
	}
}

func TestEditToolPreservesCRLFAndBOM(t *testing.T) {
	tools, dir := newTestTools(t)
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("\ufeffalpha\r\nbeta\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, tools["edit"], EditParams{
		Path:  "f.txt",
		Edits: []Edit{{OldText: "alpha", NewText: "ALPHA"}},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "\ufeff") {
		t.Error("BOM was not preserved")
	}
	if !strings.Contains(string(got), "\r\n") {
		t.Error("CRLF line endings were not preserved")
	}
	if strings.Contains(strings.TrimPrefix(string(got), "\ufeff"), "\ufeff") {
		t.Error("BOM was duplicated")
	}
}

// Models sometimes send edits as a JSON string, or use legacy top-level
// oldText/newText. Both must be repaired rather than rejected.
func TestEditToolArgumentRepair(t *testing.T) {
	tools, dir := newTestTools(t)

	t.Run("edits as JSON string", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		raw := json.RawMessage(`{"path":"a.txt","edits":"[{\"oldText\":\"hello\",\"newText\":\"world\"}]"}`)
		if _, err := tools["edit"].Execute(context.Background(), "c1", raw, nil); err != nil {
			t.Fatalf("edit with stringified edits: %v", err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
		if string(got) != "world" {
			t.Errorf("file = %q", got)
		}
	})

	t.Run("legacy oldText/newText", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		raw := json.RawMessage(`{"path":"b.txt","oldText":"hello","newText":"world"}`)
		if _, err := tools["edit"].Execute(context.Background(), "c2", raw, nil); err != nil {
			t.Fatalf("edit with legacy args: %v", err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "b.txt"))
		if string(got) != "world" {
			t.Errorf("file = %q", got)
		}
	})
}

func TestBashTool(t *testing.T) {
	tools, _ := newTestTools(t)

	res, err := run(t, tools["bash"], BashParams{Command: "echo hello"})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(resultText(res), "hello") {
		t.Errorf("output = %q", resultText(res))
	}
	details, ok := res.Details.(*BashDetails)
	if !ok {
		t.Fatalf("details = %T, want *BashDetails", res.Details)
	}
	if details.ExitCode != 0 {
		t.Errorf("exitCode = %d", details.ExitCode)
	}
}

// The session metadata is read per command, not captured once, because the
// model and reasoning level change while a session runs.
func TestBashExposesSessionEnvironment(t *testing.T) {
	dir := t.TempDir()
	e, err := osenv.New(osenv.Options{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	model := "claude-opus-5"
	bash := Bash(e, func() []string {
		return []string{"TAU_MODEL=" + model, "TAU_SESSION_ID=abc123"}
	})

	res, err := run(t, bash, BashParams{Command: `echo "$TAU_MODEL/$TAU_SESSION_ID"`})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if got := strings.TrimSpace(resultText(res)); got != "claude-opus-5/abc123" {
		t.Errorf("output = %q, want the session metadata", got)
	}

	model = "claude-sonnet-5"
	res, err = run(t, bash, BashParams{Command: `echo "$TAU_MODEL"`})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if got := strings.TrimSpace(resultText(res)); got != "claude-sonnet-5" {
		t.Errorf("output = %q, want the model switch to be visible", got)
	}
}

// A tau running inside another tau's bash tool inherits the outer session's
// variables. Reporting those as its own would be worse than reporting none.
func TestBashMasksInheritedSessionEnvironment(t *testing.T) {
	t.Setenv("TAU_SESSION_ID", "outer-session")
	t.Setenv("TAU_MODEL", "outer-model")

	dir := t.TempDir()
	e, err := osenv.New(osenv.Options{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	res, err := run(t, Bash(e, nil), BashParams{Command: `echo "[$TAU_SESSION_ID][$TAU_MODEL]"`})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if got := strings.TrimSpace(resultText(res)); got != "[][]" {
		t.Errorf("output = %q, want the inherited values blanked", got)
	}
}

// The guideline tells the model to go looking for these variables, so it must
// only be present when something actually sets them.
func TestBashGuidelineTracksSessionEnvironment(t *testing.T) {
	dir := t.TempDir()
	e, err := osenv.New(osenv.Options{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	guidelines := func(sessionEnv SessionEnv) []string {
		for _, tool := range CodingTools(e, sessionEnv) {
			if tool.Def().Name == "bash" {
				return tool.Def().PromptGuidelines
			}
		}
		t.Fatal("no bash tool in the coding tool set")
		return nil
	}

	if got := guidelines(nil); len(got) != 0 {
		t.Errorf("guidelines = %q, want none without session metadata", got)
	}
	got := guidelines(func() []string { return nil })
	if len(got) != 1 || !strings.Contains(got[0], "TAU_*") {
		t.Errorf("guidelines = %q, want the TAU_* hint", got)
	}
}

// env.Exec treats a non-zero exit as data; the bash *tool* must surface it as
// an error so the model reads the command as failed.
func TestBashToolNonZeroExitIsError(t *testing.T) {
	tools, _ := newTestTools(t)

	_, err := run(t, tools["bash"], BashParams{Command: "echo oops; exit 3"})
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "exited with code 3") {
		t.Errorf("error = %q, want the exit code", err)
	}
	// The output leading up to the failure has to survive into the error.
	if !strings.Contains(err.Error(), "oops") {
		t.Errorf("error = %q, want it to include the command output", err)
	}
}

func TestBashToolTimeout(t *testing.T) {
	tools, _ := newTestTools(t)

	_, err := run(t, tools["bash"], BashParams{Command: "sleep 30", Timeout: 0.3})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want a timeout message", err)
	}
}

func TestBashToolStreamsUpdates(t *testing.T) {
	tools, _ := newTestTools(t)

	var (
		mu      sync.Mutex
		updates int
	)
	raw, _ := json.Marshal(BashParams{Command: "for i in 1 2 3; do echo $i; sleep 0.15; done"})
	_, err := tools["bash"].Execute(context.Background(), "c1", raw, func(agent.ToolResult) {
		mu.Lock()
		updates++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if updates == 0 {
		t.Error("expected streaming updates")
	}
}

func TestBashToolEmptyCommand(t *testing.T) {
	tools, _ := newTestTools(t)
	if _, err := run(t, tools["bash"], BashParams{Command: ""}); err == nil {
		t.Error("expected an error for an empty command")
	}
}

func TestBashToolNoOutput(t *testing.T) {
	tools, _ := newTestTools(t)
	res, err := run(t, tools["bash"], BashParams{Command: "true"})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if resultText(res) != "(no output)" {
		t.Errorf("result = %q, want the empty-output placeholder", resultText(res))
	}
}

// Concurrent mutations to one path must serialize: the file has to end up as
// exactly one writer's complete content, never an interleaving.
func TestConcurrentMutationsSerialize(t *testing.T) {
	tools, dir := newTestTools(t)
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			body := strings.Repeat("abcdefghij", 200)
			_, _ = run(t, tools["write"], WriteParams{
				Path:    "shared.txt",
				Content: body + "\n",
			})
		}(i)
	}
	// An edit racing the writers exercises the read-modify-write path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = run(t, tools["edit"], EditParams{
			Path:  "shared.txt",
			Edits: []Edit{{OldText: "start", NewText: "edited"}},
		})
	}()
	wg.Wait()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)
	valid := content == "edited\n" ||
		content == strings.Repeat("abcdefghij", 200)+"\n"
	if !valid {
		t.Errorf("file is a torn interleaving (%d bytes): %.80q", len(content), content)
	}
}
