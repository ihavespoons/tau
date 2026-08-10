package coding

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/export"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/slashcmd"
)

// The commands are wired through a runtime type assertion, so nothing but a
// check like this catches a host that stopped satisfying the interface.
func TestCodingHostIsAnExporter(t *testing.T) {
	var h slashcmd.Host = codingHost{}
	if _, ok := h.(slashcmd.Exporter); !ok {
		t.Fatal("codingHost no longer implements slashcmd.Exporter, so /export and /share are dead")
	}
	if _, ok := slashcmd.Host(hostWithUI{}).(slashcmd.Exporter); !ok {
		t.Fatal("hostWithUI no longer implements slashcmd.Exporter")
	}
}

func TestExportSessionWritesHTML(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	if _, err := cs.Session.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hello"}}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "nested", "page.html")
	path, err := cs.ExportSession(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if path != out {
		t.Errorf("wrote %q, want %q", path, out)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	if !strings.Contains(page, "session-data") {
		t.Error("the page has no session payload")
	}
	if strings.Contains(page, "{{") {
		t.Error("a template placeholder survived into the page")
	}

	// The system prompt and tool list travel with a live session, unlike an
	// export read back from a file.
	payload := decodePage(t, page)
	if payload["systemPrompt"] == nil || payload["systemPrompt"] == "" {
		t.Error("the live session's system prompt is missing from the export")
	}
}

func TestExportSessionWritesJSONL(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	for _, text := range []string{"one", "two"} {
		if _, err := cs.Session.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: text}}); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "copy.jsonl")
	path, err := cs.ExportSession(ctx, out)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var lines []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not json: %v\n%s", err, sc.Text())
		}
		lines = append(lines, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header and two entries", len(lines))
	}
	if lines[0]["type"] != "session" {
		t.Errorf("first line is %v, want the session header", lines[0]["type"])
	}
	// The chain must be intact, or the copy will not open.
	if lines[1]["parentId"] != nil {
		t.Errorf("first entry has parent %v, want null", lines[1]["parentId"])
	}
	if lines[2]["parentId"] != lines[1]["id"] {
		t.Errorf("second entry points at %v, want %v", lines[2]["parentId"], lines[1]["id"])
	}

	// The written file must be openable as a session again.
	data, err := export.FromFile(ctx, path)
	if err != nil {
		t.Fatalf("the exported file does not load as a session: %v", err)
	}
	if len(data.Entries) != 2 {
		t.Errorf("reopened session has %d entries, want 2", len(data.Entries))
	}
}

// An export is the transcript, not the model's context, so a compaction must
// not truncate it the way Session.Branch does.
func TestExportKeepsHistoryBehindACompaction(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	if _, err := cs.Session.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "ancient"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Session.AppendCompaction(ctx, "summary of history", 50000, session.CompactionOptions{
		RetainedTail: []ai.Message{ai.UserMessage{Content: ai.UserContent{Text: "recent"}}},
	}); err != nil {
		t.Fatal(err)
	}

	branch, err := cs.Session.Branch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	full, err := branchToRoot(ctx, cs.Session)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) <= len(branch) {
		t.Fatalf("branchToRoot returned %d entries and Branch %d; the walk is not going past the compaction", len(full), len(branch))
	}
	if full[0].Base().ID != cs.Session.Entries(ctx, nil)[0].Base().ID {
		t.Error("the export does not start at the root entry")
	}

	out := filepath.Join(t.TempDir(), "copy.jsonl")
	if _, err := cs.ExportSession(ctx, out); err != nil {
		t.Fatal(err)
	}
	data, err := export.FromFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Entries) != len(full) {
		t.Errorf("reopened export has %d entries, want %d", len(data.Entries), len(full))
	}
}

// The suffix decides the format, and it is not case-sensitive: someone typing
// a path is not thinking about that.
func TestExportSessionPicksFormatBySuffix(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	if _, err := cs.Session.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hi"}}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	for _, name := range []string{"a.JSONL", "b.jsonl"} {
		path, err := cs.ExportSession(ctx, filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "<!DOCTYPE") || strings.Contains(string(body), "<html") {
			t.Errorf("%s was rendered as a page", name)
		}
	}
}

func TestExportSessionRefusesAnEmptySession(t *testing.T) {
	cs := newTestSession(t, Options{})
	if _, err := cs.ExportSession(context.Background(), ""); err != export.ErrEmpty {
		t.Errorf("error = %v, want ErrEmpty", err)
	}
	if _, err := cs.ExportSession(context.Background(), "out.jsonl"); err != export.ErrEmpty {
		t.Errorf("jsonl error = %v, want ErrEmpty", err)
	}
}

func decodePage(t *testing.T, page string) map[string]any {
	t.Helper()
	const open = `<script id="session-data" type="application/json">`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatal("no session-data script in the page")
	}
	rest := page[i+len(open):]
	j := strings.Index(rest, "</script>")
	if j < 0 {
		t.Fatal("session-data script is not closed")
	}
	raw, err := base64.StdEncoding.DecodeString(rest[:j])
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("payload is not json: %v", err)
	}
	return out
}
