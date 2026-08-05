package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSession(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line is not JSON: %s", line)
		}
		out = append(out, rec)
	}
	return out
}

// A v1 session is a flat list with no ids at all. Migrating it has to invent
// the tree the format never had, and the only honest tree is file order.
func TestAV1SessionBecomesASingleChain(t *testing.T) {
	ctx := context.Background()
	path := writeSession(t,
		`{"type":"session","id":"s1","timestamp":"2024-01-01T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"user","content":"one"}}`,
		`{"type":"message","timestamp":"2024-01-01T00:00:02.000Z","message":{"role":"user","content":"two"}}`,
		`{"type":"message","timestamp":"2024-01-01T00:00:03.000Z","message":{"role":"user","content":"three"}}`,
	)

	storage, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	if !storage.Migrated() {
		t.Error("a v1 file should report that it was migrated")
	}
	if v := storage.Header().Version; v != Version {
		t.Errorf("header version = %d, want %d", v, Version)
	}

	entries := storage.Entries(ctx, nil)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	var prev string
	for i, e := range entries {
		id := e.Base().ID
		if id == "" {
			t.Fatalf("entry %d has no id", i)
		}
		parent := e.Base().ParentID
		if i == 0 {
			if parent != nil {
				t.Errorf("first entry parent = %v, want nil", *parent)
			}
		} else if parent == nil || *parent != prev {
			t.Errorf("entry %d parent = %v, want %s", i, parent, prev)
		}
		prev = id
	}

	// The leaf is the last entry, so the migrated session resumes where the
	// v1 one left off rather than at the root.
	leaf, err := storage.LeafID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if leaf == nil || *leaf != prev {
		t.Errorf("leaf = %v, want %s", leaf, prev)
	}
}

// The migrated file is what tau appends to next, so it has to be on disk. An
// in-memory-only migration would mint different ids on the next open and leave
// the appended entries parented to nothing.
func TestMigrationIsWrittenBackToTheFile(t *testing.T) {
	path := writeSession(t,
		`{"type":"session","id":"s1","timestamp":"2024-01-01T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"user","content":"one"}}`,
	)
	first, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	wantID := first.Entries(context.Background(), nil)[0].Base().ID

	records := readLines(t, path)
	if records[0]["version"] != float64(Version) {
		t.Errorf("on-disk version = %v, want %d", records[0]["version"], Version)
	}
	if records[1]["id"] != wantID {
		t.Errorf("on-disk id = %v, want %s", records[1]["id"], wantID)
	}

	second, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Migrated() {
		t.Error("a second open should find nothing left to migrate")
	}
	if got := second.Entries(context.Background(), nil)[0].Base().ID; got != wantID {
		t.Errorf("id changed across opens: %s then %s", wantID, got)
	}
}

// Importing must not edit what it reads.
func TestAReadOnlyOpenLeavesTheFileAlone(t *testing.T) {
	path := writeSession(t,
		`{"type":"session","id":"s1","timestamp":"2024-01-01T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"user","content":"one"}}`,
	)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	storage, err := OpenJSONLReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if !storage.Migrated() {
		t.Error("the in-memory view should still be migrated")
	}
	if id := storage.Entries(context.Background(), nil)[0].Base().ID; id == "" {
		t.Error("the in-memory entry should still have been given an id")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("file was rewritten:\nbefore: %s\nafter:  %s", before, after)
	}
}

// v1 compaction pointed at an entry by its position in the file. After
// migration the position is meaningless, so it must become an id — or the
// checkpoint would replay from the wrong place.
func TestAV1CompactionIndexBecomesAnEntryID(t *testing.T) {
	ctx := context.Background()
	path := writeSession(t,
		`{"type":"session","id":"s1","timestamp":"2024-01-01T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"user","content":"one"}}`,
		`{"type":"message","timestamp":"2024-01-01T00:00:02.000Z","message":{"role":"user","content":"two"}}`,
		`{"type":"compaction","timestamp":"2024-01-01T00:00:03.000Z","summary":"so far","tokensBefore":100,"firstKeptEntryIndex":2}`,
	)
	storage, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := storage.Entries(ctx, nil)
	compaction, ok := entries[2].(*CompactionEntry)
	if !ok {
		t.Fatalf("entry 2 is %T, want *CompactionEntry", entries[2])
	}
	if compaction.FirstKeptEntryID != entries[1].Base().ID {
		t.Errorf("firstKeptEntryId = %q, want %q", compaction.FirstKeptEntryID, entries[1].Base().ID)
	}
	for _, rec := range readLines(t, path) {
		if _, stale := rec["firstKeptEntryIndex"]; stale {
			t.Error("firstKeptEntryIndex should be gone from the migrated file")
		}
	}
}

// v2 called extension-injected messages hookMessage. The role is the decoder's
// only dispatch key, so an unrenamed one decodes as an opaque message and
// silently stops reaching the model.
func TestAV2HookMessageBecomesACustomMessage(t *testing.T) {
	ctx := context.Background()
	path := writeSession(t,
		`{"type":"session","version":2,"id":"s1","timestamp":"2024-01-01T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","id":"aaaa1111","parentId":null,"timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"hookMessage","customType":"note","content":"hi","display":true}}`,
	)
	storage, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := storage.Entries(ctx, nil)[0].(*MessageEntry)
	if !ok {
		t.Fatalf("entry is %T, want *MessageEntry", storage.Entries(ctx, nil)[0])
	}
	custom, ok := entry.Message.(*CustomMessage)
	if !ok {
		t.Fatalf("message is %T, want *CustomMessage", entry.Message)
	}
	if custom.CustomType != "note" {
		t.Errorf("customType = %q, want note", custom.CustomType)
	}
}

// v2 already has ids. Regenerating them would break every parent link in the
// file, so the v1 step must not run.
func TestAV2SessionKeepsItsExistingIDs(t *testing.T) {
	ctx := context.Background()
	path := writeSession(t,
		`{"type":"session","version":2,"id":"s1","timestamp":"2024-01-01T00:00:00.000Z","cwd":"/w"}`,
		`{"type":"message","id":"aaaa1111","parentId":null,"timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"user","content":"one"}}`,
		`{"type":"message","id":"bbbb2222","parentId":"aaaa1111","timestamp":"2024-01-01T00:00:02.000Z","message":{"role":"user","content":"two"}}`,
	)
	storage, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := storage.Entries(ctx, nil)
	if entries[0].Base().ID != "aaaa1111" || entries[1].Base().ID != "bbbb2222" {
		t.Errorf("ids = %q, %q; want aaaa1111, bbbb2222", entries[0].Base().ID, entries[1].Base().ID)
	}
	if p := entries[1].Base().ParentID; p == nil || *p != "aaaa1111" {
		t.Errorf("parent = %v, want aaaa1111", p)
	}
}

// A current-version file must come back byte-identical: migration that touches
// a session it has nothing to do to is a rewrite risk for no gain.
func TestACurrentVersionFileIsNotTouched(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"/w"}`),
		[]byte(`{"type":"message","id":"aaaa1111","parentId":null,"timestamp":"t","message":{"role":"user","content":"one"}}`),
	}
	out, changed, err := MigrateLines(lines)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a v3 file needs no migration")
	}
	for i := range lines {
		if string(out[i]) != string(lines[i]) {
			t.Errorf("line %d changed:\n got %s\nwant %s", i, out[i], lines[i])
		}
	}
}

// Numbers must survive as they were written. Session files are full of
// per-turn costs — small floats that a round trip through float64 re-renders
// in exponent form, and of token counts long enough to lose their last digits.
// Both are still valid JSON and neither is the number Pi wrote.
func TestMigrationPreservesNumbersExactly(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"session","id":"s1","timestamp":"t","cwd":"/w"}`),
		[]byte(`{"type":"message","timestamp":"t","message":{"role":"assistant","content":[],` +
			`"usage":{"input":1200,"totalTokens":9007199254740993,"cost":{"total":0.0000123456789}},` +
			`"timestamp":1712345678901}}`),
	}
	out, changed, err := MigrateLines(lines)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a v1 file must migrate")
	}
	migrated := string(out[1])
	for _, want := range []string{
		"1712345678901",    // a millisecond timestamp
		"0.0000123456789",  // a cost, which float64 would render as 1.23456789e-05
		"9007199254740993", // past float64's exact-integer range
	} {
		if !strings.Contains(migrated, want) {
			t.Errorf("%s lost its exact form:\n%s", want, migrated)
		}
	}
}

// One unparseable line should not cost the user the migration of every other.
func TestMigrationPassesThroughLinesItCannotRead(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"session","id":"s1","timestamp":"t","cwd":"/w"}`),
		[]byte(`not json at all`),
		[]byte(`{"type":"message","timestamp":"t","message":{"role":"user","content":"one"}}`),
	}
	out, changed, err := MigrateLines(lines)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a v1 file must migrate")
	}
	if string(out[1]) != "not json at all" {
		t.Errorf("unreadable line = %s, want it untouched", out[1])
	}
	var rec map[string]any
	if err := json.Unmarshal(out[2], &rec); err != nil {
		t.Fatal(err)
	}
	if rec["id"] == nil {
		t.Error("the readable entry should still have been given an id")
	}
}
