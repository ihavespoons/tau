package export

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/theme"
)

func newSession(t *testing.T) (*session.Session, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sess-abc.jsonl")
	storage, err := session.CreateJSONL(path, session.CreateOptions{Cwd: "/proj", SessionID: "sess-abc"})
	if err != nil {
		t.Fatal(err)
	}
	return session.NewSession(storage), path
}

func darkTheme(t *testing.T) *theme.Theme {
	t.Helper()
	th, ok := theme.Builtin("dark")
	if !ok {
		t.Fatal("the built-in dark theme is missing")
	}
	return th
}

// decodePayload pulls the session JSON back out of a rendered page, which is
// what the viewer itself does on load.
func decodePayload(t *testing.T, html string) map[string]any {
	t.Helper()
	const open = `<script id="session-data" type="application/json">`
	i := strings.Index(html, open)
	if i < 0 {
		t.Fatal("rendered page has no session-data script")
	}
	rest := html[i+len(open):]
	j := strings.Index(rest, "</script>")
	if j < 0 {
		t.Fatal("session-data script is not closed")
	}
	raw, err := base64.StdEncoding.DecodeString(rest[:j])
	if err != nil {
		t.Fatalf("session data is not base64: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("session data is not json: %v\n%s", err, raw)
	}
	return out
}

func TestGenerateProducesSelfContainedPage(t *testing.T) {
	ctx := context.Background()
	sess, path := newSession(t)
	if _, err := sess.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hello"}}); err != nil {
		t.Fatal(err)
	}

	data, err := FromSession(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	html, err := Generate(data, darkTheme(t))
	if err != nil {
		t.Fatal(err)
	}

	// Every placeholder must be gone: one left behind means a section of the
	// page silently renders as literal text.
	for _, ph := range []string{
		"{{CSS}}", "{{JS}}", "{{SESSION_DATA}}", "{{MARKED_JS}}", "{{HIGHLIGHT_JS}}",
		"{{THEME_VARS}}", "{{BODY_BG}}", "{{CONTAINER_BG}}", "{{INFO_BG}}",
	} {
		if strings.Contains(html, ph) {
			t.Errorf("%s was never substituted", ph)
		}
	}
	// Nothing may be fetched at runtime.
	if strings.Contains(html, "<script src=") || strings.Contains(html, "<link rel=\"stylesheet\"") {
		t.Error("the page references an external asset")
	}
	if !strings.Contains(html, "marked") || !strings.Contains(html, "hljs") {
		t.Error("the vendored libraries are missing from the page")
	}

	payload := decodePayload(t, html)
	if _, ok := payload["header"]; !ok {
		t.Error("payload has no header")
	}
	entries, ok := payload["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %v, want one", payload["entries"])
	}
	// leafId must be present even when null — the viewer reads the key.
	if _, ok := payload["leafId"]; !ok {
		t.Error("payload has no leafId key")
	}
	if got := payload["header"].(map[string]any)["id"]; got != "sess-abc" {
		t.Errorf("header id = %v, want sess-abc", got)
	}
	_ = path
}

// A transcript containing markup or a closing script tag must not be able to
// break out of the page it is embedded in.
func TestSessionDataIsInert(t *testing.T) {
	ctx := context.Background()
	sess, _ := newSession(t)
	const hostile = `</script><script>alert('xss')</script>`
	if _, err := sess.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: hostile}}); err != nil {
		t.Fatal(err)
	}

	data, err := FromSession(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	html, err := Generate(data, darkTheme(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "alert('xss')") {
		t.Error("message text reached the page unencoded")
	}
	payload := decodePayload(t, html)
	entry := payload["entries"].([]any)[0].(map[string]any)
	msg := entry["message"].(map[string]any)
	if got := msg["content"]; got != hostile {
		t.Errorf("round-tripped content = %v, want it preserved verbatim", got)
	}
}

func TestFromSessionRejectsEmptyAndInMemory(t *testing.T) {
	ctx := context.Background()

	sess, _ := newSession(t)
	if _, err := FromSession(ctx, sess, nil); err != ErrEmpty {
		t.Errorf("empty session error = %v, want ErrEmpty", err)
	}

	mem := session.NewSession(session.NewMemStorage(session.Metadata{ID: "s1"}))
	if _, err := mem.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := FromSession(ctx, mem, nil); err != ErrInMemory {
		t.Errorf("in-memory session error = %v, want ErrInMemory", err)
	}
}

func TestFromFile(t *testing.T) {
	ctx := context.Background()
	sess, path := newSession(t)
	if _, err := sess.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hello"}}); err != nil {
		t.Fatal(err)
	}

	data, err := FromFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(data.Entries))
	}
	if data.Header.ID != "sess-abc" {
		t.Errorf("header id = %q", data.Header.ID)
	}
	if data.SystemPrompt != "" || data.Tools != nil {
		t.Error("a file export must not invent agent state")
	}

	if _, err := FromFile(ctx, filepath.Join(t.TempDir(), "gone.jsonl")); err == nil {
		t.Error("a missing file should fail")
	}
}

func TestStateIsCarriedThrough(t *testing.T) {
	ctx := context.Background()
	sess, _ := newSession(t)
	if _, err := sess.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hi"}}); err != nil {
		t.Fatal(err)
	}

	data, err := FromSession(ctx, sess, &State{
		SystemPrompt: "be helpful",
		Tools:        []Tool{{Name: "bash", Description: "run a command"}},
		RenderedTools: map[string]RenderedTool{
			"call-1": {CallHTML: "<div>custom</div>"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	html, err := Generate(data, darkTheme(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := decodePayload(t, html)
	if payload["systemPrompt"] != "be helpful" {
		t.Errorf("systemPrompt = %v", payload["systemPrompt"])
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", payload["tools"])
	}
	rendered, _ := payload["renderedTools"].(map[string]any)
	if len(rendered) != 1 {
		t.Fatalf("renderedTools = %v", payload["renderedTools"])
	}
}

func TestDefaultOutputPath(t *testing.T) {
	if got := DefaultOutputPath("/x/y/2026-08-10-abc.jsonl"); got != "tau-session-2026-08-10-abc.html" {
		t.Errorf("DefaultOutputPath = %q", got)
	}
}

func TestWriteFileDefaultsName(t *testing.T) {
	ctx := context.Background()
	sess, path := newSession(t)
	if _, err := sess.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	data, err := FromSession(ctx, sess, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	out, err := WriteFile(data, nil, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out) != "tau-session-sess-abc.html" {
		t.Errorf("output = %q", out)
	}
}
