package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func TestHeaderVersionValidation(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr string
	}{
		{
			name: "valid",
			line: `{"type":"session","version":3,"id":"s1","timestamp":"2026-07-28T14:00:00.000Z","cwd":"/tmp/x"}`,
		},
		{
			name:    "version 2 rejected",
			line:    `{"type":"session","version":2,"id":"s1","timestamp":"2026-07-28T14:00:00.000Z","cwd":"/tmp/x"}`,
			wantErr: "unsupported session version",
		},
		{
			name:    "version 1 rejected",
			line:    `{"type":"session","version":1,"id":"s1","timestamp":"2026-07-28T14:00:00.000Z","cwd":"/tmp/x"}`,
			wantErr: "unsupported session version",
		},
		{
			name:    "missing version rejected",
			line:    `{"type":"session","id":"s1","timestamp":"2026-07-28T14:00:00.000Z","cwd":"/tmp/x"}`,
			wantErr: "unsupported session version",
		},
		{
			name:    "wrong type rejected",
			line:    `{"type":"message","version":3,"id":"s1","timestamp":"t","cwd":"/tmp/x"}`,
			wantErr: "not a valid session header",
		},
		{
			name:    "missing id",
			line:    `{"type":"session","version":3,"timestamp":"2026-07-28T14:00:00.000Z","cwd":"/tmp/x"}`,
			wantErr: "missing id",
		},
		{
			name:    "missing cwd",
			line:    `{"type":"session","version":3,"id":"s1","timestamp":"2026-07-28T14:00:00.000Z"}`,
			wantErr: "missing cwd",
		},
		{
			name:    "missing timestamp",
			line:    `{"type":"session","version":3,"id":"s1","cwd":"/tmp/x"}`,
			wantErr: "missing timestamp",
		},
		{
			name:    "parentSession must be string",
			line:    `{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"/tmp/x","parentSession":42}`,
			wantErr: "parentSession must be a string",
		},
		{
			name:    "metadata must be object",
			line:    `{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"/tmp/x","metadata":[1,2]}`,
			wantErr: "metadata must be an object",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var h Header
			err := json.Unmarshal([]byte(tc.line), &h)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// Every entry type must survive decode → encode with its bytes intact,
// including fields this build does not model. That is what makes fork
// lossless and keeps a session written by a newer agent readable.
func TestEntryRoundTripPreservesBytes(t *testing.T) {
	lines := []struct {
		name string
		line string
	}{
		{"message user", `{"type":"message","id":"a1","parentId":null,"timestamp":"2026-07-28T14:00:01.000Z","message":{"role":"user","content":"hi","timestamp":1}}`},
		{"message assistant", `{"type":"message","id":"a2","parentId":"a1","timestamp":"2026-07-28T14:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"yo"}],"api":"anthropic-messages","provider":"anthropic","model":"m","usage":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":3,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}`},
		{"message toolResult", `{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-07-28T14:00:03.000Z","message":{"role":"toolResult","toolCallId":"t1","toolName":"bash","content":[{"type":"text","text":"ok"}],"isError":false,"timestamp":3}}`},
		{"thinking_level_change", `{"type":"thinking_level_change","id":"b1","parentId":"a3","timestamp":"2026-07-28T14:00:04.000Z","thinkingLevel":"high"}`},
		{"model_change", `{"type":"model_change","id":"b2","parentId":"b1","timestamp":"2026-07-28T14:00:05.000Z","provider":"anthropic","modelId":"claude-opus-5"}`},
		{"active_tools_change", `{"type":"active_tools_change","id":"b3","parentId":"b2","timestamp":"2026-07-28T14:00:06.000Z","activeToolNames":["read","bash"]}`},
		{"compaction firstKept", `{"type":"compaction","id":"c1","parentId":"b3","timestamp":"2026-07-28T14:00:07.000Z","summary":"did stuff","firstKeptEntryId":"a3","tokensBefore":50000}`},
		{"compaction retainedTail", `{"type":"compaction","id":"c2","parentId":"c1","timestamp":"2026-07-28T14:00:08.000Z","summary":"more","tokensBefore":60000,"retainedTail":[{"role":"user","content":"latest","timestamp":9}]}`},
		{"branch_summary", `{"type":"branch_summary","id":"d1","parentId":"c2","timestamp":"2026-07-28T14:00:09.000Z","fromId":"c1","summary":"explored A"}`},
		{"custom", `{"type":"custom","id":"e1","parentId":"d1","timestamp":"2026-07-28T14:00:10.000Z","customType":"ext","data":{"n":42}}`},
		{"custom_message", `{"type":"custom_message","id":"e2","parentId":"e1","timestamp":"2026-07-28T14:00:11.000Z","customType":"ext","content":"injected","display":true}`},
		{"label", `{"type":"label","id":"f1","parentId":"e2","timestamp":"2026-07-28T14:00:12.000Z","targetId":"a1","label":"mark"}`},
		{"label cleared", `{"type":"label","id":"f2","parentId":"f1","timestamp":"2026-07-28T14:00:13.000Z","targetId":"a1","label":null}`},
		{"session_info", `{"type":"session_info","id":"g1","parentId":"f2","timestamp":"2026-07-28T14:00:14.000Z","name":"my session"}`},
		{"leaf", `{"type":"leaf","id":"h1","parentId":"g1","timestamp":"2026-07-28T14:00:15.000Z","targetId":"a1"}`},
		{"leaf null target", `{"type":"leaf","id":"h2","parentId":"h1","timestamp":"2026-07-28T14:00:16.000Z","targetId":null}`},
		// Fields this build does not model must survive untouched.
		{"unknown fields preserved", `{"type":"message","id":"i1","parentId":null,"timestamp":"2026-07-28T14:00:17.000Z","message":{"role":"user","content":"hi","timestamp":1},"futureField":{"deep":[1,2,3]},"anotherOne":"keep me"}`},
		{"unknown entry type", `{"type":"quantum_entanglement","id":"j1","parentId":null,"timestamp":"2026-07-28T14:00:18.000Z","spooky":true}`},
	}

	for _, tc := range lines {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := UnmarshalEntry([]byte(tc.line))
			if entry == nil {
				t.Fatalf("no entry returned: %v", err)
			}
			out, err := json.Marshal(entry)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(out) != tc.line {
				t.Errorf("round trip changed bytes:\n got %s\nwant %s", out, tc.line)
			}
		})
	}
}

// A constructed entry has no source bytes, so it marshals from struct with the
// type discriminator first.
func TestConstructedEntryMarshalsTypeFirst(t *testing.T) {
	e := &ModelChangeEntry{
		EntryBase: EntryBase{ID: "x1", ParentID: nil, Timestamp: "2026-07-28T14:00:00.000Z"},
		Provider:  "anthropic",
		ModelID:   "claude-opus-5",
	}
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"model_change","id":"x1","parentId":null,"timestamp":"2026-07-28T14:00:00.000Z","provider":"anthropic","modelId":"claude-opus-5"}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

// parentId must serialize as explicit null for a root entry, never be omitted:
// Pi validates its presence.
func TestRootEntryEmitsNullParent(t *testing.T) {
	e := &SessionInfoEntry{
		EntryBase: EntryBase{ID: "x1", Timestamp: "2026-07-28T14:00:00.000Z"},
		Name:      "n",
	}
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"parentId":null`) {
		t.Errorf("missing explicit null parentId: %s", out)
	}
}

func TestEntryValidationRejectsMalformed(t *testing.T) {
	tests := []struct {
		name, line, wantErr string
	}{
		{"not json", `{oops`, "not valid JSON"},
		{"missing type", `{"id":"a","timestamp":"t"}`, "missing entry type"},
		{"missing id", `{"type":"message","timestamp":"t"}`, "missing entry id"},
		{"empty id", `{"type":"message","id":"","timestamp":"t"}`, "missing entry id"},
		{"missing timestamp", `{"type":"message","id":"a"}`, "missing timestamp"},
		{"bad parentId", `{"type":"message","id":"a","timestamp":"t","parentId":42}`, "invalid parentId"},
		{"bad leaf targetId", `{"type":"leaf","id":"a","timestamp":"t","targetId":42}`, "invalid targetId"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UnmarshalEntry([]byte(tc.line))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// An unknown message role must not fail the entry: the message is preserved
// opaquely and the problem is reported, so one odd line cannot make a user's
// history unreadable.
func TestUnknownMessageRoleIsSoftFailure(t *testing.T) {
	line := `{"type":"message","id":"a1","parentId":null,"timestamp":"2026-07-28T14:00:00.000Z","message":{"role":"telepathy","payload":"???"}}`
	entry, err := UnmarshalEntry([]byte(line))
	if err == nil {
		t.Fatal("expected a soft error describing the unknown role")
	}
	if !strings.Contains(err.Error(), "telepathy") {
		t.Errorf("error should name the role: %v", err)
	}
	msgEntry, ok := entry.(*MessageEntry)
	if !ok {
		t.Fatalf("entry type = %T, want *MessageEntry", entry)
	}
	opaque, ok := msgEntry.Message.(*OpaqueMessage)
	if !ok {
		t.Fatalf("message type = %T, want *OpaqueMessage", msgEntry.Message)
	}
	if opaque.Role() != "telepathy" {
		t.Errorf("role = %q", opaque.Role())
	}
	out, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != line {
		t.Errorf("round trip changed bytes:\n got %s\nwant %s", out, line)
	}
}

func TestRegisterMessageDecoder(t *testing.T) {
	type customRole struct {
		Field string `json:"field"`
	}
	RegisterMessageDecoder("testRole", func(raw json.RawMessage) (ai.Message, error) {
		var v customRole
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		return &testMessage{Field: v.Field}, nil
	})
	t.Cleanup(func() {
		decodersMu.Lock()
		delete(decoders, "testRole")
		decodersMu.Unlock()
	})

	msg, err := decodeAgentMessage(json.RawMessage(`{"role":"testRole","field":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	tm, ok := msg.(*testMessage)
	if !ok {
		t.Fatalf("type = %T", msg)
	}
	if tm.Field != "hello" {
		t.Errorf("field = %q", tm.Field)
	}
}

type testMessage struct {
	Field string `json:"field"`
}

func (*testMessage) Role() string { return "testRole" }

func TestLeafIDAfterEntry(t *testing.T) {
	target := "target99"
	leaf := &LeafEntry{EntryBase: EntryBase{ID: "leaf01"}, TargetID: &target}
	if got := leafIDAfterEntry(leaf); got == nil || *got != "target99" {
		t.Errorf("leaf entry should redirect to its target, got %v", got)
	}

	nullLeaf := &LeafEntry{EntryBase: EntryBase{ID: "leaf02"}}
	if got := leafIDAfterEntry(nullLeaf); got != nil {
		t.Errorf("null-target leaf should clear the leaf, got %v", *got)
	}

	msg := &MessageEntry{EntryBase: EntryBase{ID: "msg01"}}
	if got := leafIDAfterEntry(msg); got == nil || *got != "msg01" {
		t.Errorf("ordinary entry should become the leaf, got %v", got)
	}
}

func TestTimestampFormat(t *testing.T) {
	ts := Now()
	if !strings.HasSuffix(ts, "Z") {
		t.Errorf("timestamp %q should be UTC with a Z suffix", ts)
	}
	if len(ts) != len("2026-07-28T14:00:00.000Z") {
		t.Errorf("timestamp %q should carry millisecond precision like JS toISOString", ts)
	}
	if parseTimestamp(ts) == 0 {
		t.Errorf("timestamp %q should parse back to unix ms", ts)
	}
	if parseTimestamp("not a timestamp") != 0 {
		t.Error("unparseable timestamps should yield 0, not fail")
	}
}

func TestDetachAllowsMutation(t *testing.T) {
	line := `{"type":"session_info","id":"a1","parentId":null,"timestamp":"2026-07-28T14:00:00.000Z","name":"old"}`
	entry, err := UnmarshalEntry([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	info := entry.(*SessionInfoEntry)
	info.Name = "new"

	out, _ := json.Marshal(info)
	if !strings.Contains(string(out), `"name":"old"`) {
		t.Error("without Detach, a decoded entry should replay its source bytes")
	}

	info.Detach()
	out, _ = json.Marshal(info)
	if !strings.Contains(string(out), `"name":"new"`) {
		t.Errorf("after Detach the struct should marshal: %s", out)
	}
}

func TestConvertToLLM(t *testing.T) {
	exitCode := 1
	messages := []ai.Message{
		ai.UserMessage{Content: ai.UserContent{Text: "hello"}, Timestamp: 1},
		&CompactionSummaryMessage{Summary: "earlier stuff", TokensBefore: 100, Timestamp: 2},
		&BranchSummaryMessage{Summary: "branch stuff", FromID: "x", Timestamp: 3},
		&CustomMessage{CustomType: "ext", Content: ai.UserContent{Text: "injected"}, Timestamp: 4},
		&BashExecutionMessage{Command: "ls", Output: "a.go", ExitCode: &exitCode, Timestamp: 5},
		&BashExecutionMessage{Command: "secret", Output: "x", ExcludeFromContext: true, Timestamp: 6},
		&OpaqueMessage{RoleName: "unknown", Raw: json.RawMessage(`{"role":"unknown"}`)},
	}

	out := ConvertToLLM(messages)
	if len(out) != 5 {
		t.Fatalf("got %d messages, want 5 (excluded bash run and unknown role must drop)", len(out))
	}
	for i, m := range out {
		if m.Role() != "user" && m.Role() != "assistant" && m.Role() != "toolResult" {
			t.Errorf("message %d has non-provider role %q", i, m.Role())
		}
	}

	compaction := out[1].(ai.UserMessage).Content.String()
	if !strings.HasPrefix(compaction, CompactionSummaryPrefix) || !strings.HasSuffix(compaction, CompactionSummarySuffix) {
		t.Errorf("compaction summary not wrapped: %q", compaction)
	}
	if !strings.Contains(compaction, "earlier stuff") {
		t.Errorf("compaction summary lost its text: %q", compaction)
	}

	branch := out[2].(ai.UserMessage).Content.String()
	if !strings.HasPrefix(branch, BranchSummaryPrefix) || !strings.HasSuffix(branch, BranchSummarySuffix) {
		t.Errorf("branch summary not wrapped: %q", branch)
	}

	bash := out[4].(ai.UserMessage).Content.String()
	if !strings.Contains(bash, "Ran `ls`") || !strings.Contains(bash, "exited with code 1") {
		t.Errorf("bash rendering = %q", bash)
	}
}

// The summary wrappers are matched byte-for-byte against Pi; the branch suffix
// deliberately has no leading newline while the compaction one does.
func TestSummaryConstantsMatchPi(t *testing.T) {
	if CompactionSummaryPrefix != "The conversation history before this point was compacted into the following summary:\n\n<summary>\n" {
		t.Error("compaction prefix drifted from Pi")
	}
	if CompactionSummarySuffix != "\n</summary>" {
		t.Error("compaction suffix drifted from Pi")
	}
	if BranchSummaryPrefix != "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n" {
		t.Error("branch prefix drifted from Pi")
	}
	if BranchSummarySuffix != "</summary>" {
		t.Error("branch suffix drifted from Pi (it has no leading newline)")
	}
}

func TestSyntheticMessageRoundTrip(t *testing.T) {
	msgs := []ai.Message{
		&CompactionSummaryMessage{Summary: "s", TokensBefore: 10, Timestamp: 1},
		&BranchSummaryMessage{Summary: "s", FromID: "f", Timestamp: 2},
		&CustomMessage{CustomType: "ext", Content: ai.UserContent{Text: "c"}, Display: true, Timestamp: 3},
		&BashExecutionMessage{Command: "ls", Output: "o", Timestamp: 4},
	}
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("%s: %v", m.Role(), err)
		}
		decoded, err := decodeAgentMessage(raw)
		if err != nil {
			t.Fatalf("%s: %v", m.Role(), err)
		}
		if decoded.Role() != m.Role() {
			t.Errorf("role = %q, want %q", decoded.Role(), m.Role())
		}
		if !reflect.DeepEqual(decoded, m) {
			t.Errorf("%s round trip mismatch:\n got %#v\nwant %#v", m.Role(), decoded, m)
		}
	}
}
