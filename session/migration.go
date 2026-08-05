package session

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// Port of Pi's session migrations (session-manager.ts:230-296).
//
// Migration runs on raw lines rather than decoded entries because the older
// formats are missing the fields the decoder needs: a v1 entry has no id and
// no parentId at all, so there is no tree to decode until they exist.

// MigrateLines brings raw session lines up to the current version.
//
// lines are the file's records in order, header first. The returned slice is
// the migrated file; the bool reports whether anything changed. Lines that are
// not JSON objects are passed through untouched — a session with one unreadable
// record should still migrate the rest.
func MigrateLines(lines [][]byte) ([][]byte, bool, error) {
	records := make([]map[string]any, len(lines))
	for i, line := range lines {
		dec := json.NewDecoder(bytes.NewReader(line))
		// Numbers stay as written. A timestamp or a token count that went
		// through float64 could come back in exponent form, which is still
		// valid JSON but no longer the bytes the file had.
		dec.UseNumber()
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			records[i] = nil
			continue
		}
		records[i] = rec
	}

	header := findHeader(records)
	if header == nil {
		return lines, false, nil
	}
	version := headerVersion(header)
	if version >= Version {
		return lines, false, nil
	}

	if version < 2 {
		if err := migrateV1ToV2(records); err != nil {
			return nil, false, err
		}
	}
	if version < 3 {
		migrateV2ToV3(records)
	}
	header["version"] = Version

	out := make([][]byte, len(lines))
	for i, rec := range records {
		if rec == nil {
			out[i] = lines[i]
			continue
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			return nil, false, errorf(CodeInvalidEntry, err, "failed to re-encode migrated session line %d", i+1)
		}
		out[i] = encoded
	}
	return out, true, nil
}

func findHeader(records []map[string]any) map[string]any {
	for _, rec := range records {
		if rec == nil {
			continue
		}
		if t, _ := rec["type"].(string); t == "session" {
			return rec
		}
	}
	return nil
}

// headerVersion reads the header's version. A header without one is v1 — the
// version field did not exist yet.
func headerVersion(header map[string]any) int {
	switch v := header["version"].(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 1
		}
		return int(n)
	case float64:
		return int(v)
	default:
		return 1
	}
}

// migrateV1ToV2 gives every entry an id and links it to its predecessor.
//
// A v1 session is a flat list, so the tree it becomes is a single chain in file
// order — which is exactly what it always meant.
func migrateV1ToV2(records []map[string]any) error {
	minter := newIDMinter(records)

	var prevID any // nil for the first entry, matching a root's null parent
	for _, rec := range records {
		if rec == nil {
			continue
		}
		if t, _ := rec["type"].(string); t == "session" {
			continue
		}
		id, err := minter.mint()
		if err != nil {
			return err
		}
		rec["id"] = id
		rec["parentId"] = prevID
		prevID = id
	}

	// Second pass: a compaction's firstKeptEntryIndex indexes the file, so it
	// can only be resolved once every line has an id. Pi resolves it inline and
	// yields undefined for a forward reference; resolving it here means a
	// hand-edited file keeps a checkpoint tau would otherwise silently drop.
	for _, rec := range records {
		if rec == nil {
			continue
		}
		if t, _ := rec["type"].(string); t != "compaction" {
			continue
		}
		raw, ok := rec["firstKeptEntryIndex"]
		if !ok {
			continue
		}
		delete(rec, "firstKeptEntryIndex")
		idx, ok := indexValue(raw)
		if !ok || idx < 0 || idx >= len(records) {
			continue
		}
		target := records[idx]
		if target == nil {
			continue
		}
		if t, _ := target["type"].(string); t == "session" {
			continue
		}
		if id, ok := target["id"].(string); ok {
			rec["firstKeptEntryId"] = id
		}
	}
	return nil
}

func indexValue(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// migrateV2ToV3 renames the hookMessage role to custom.
func migrateV2ToV3(records []map[string]any) {
	for _, rec := range records {
		if rec == nil {
			continue
		}
		if t, _ := rec["type"].(string); t != "message" {
			continue
		}
		message, ok := rec["message"].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "hookMessage" {
			message["role"] = RoleCustom
		}
	}
}

// idMinter hands out entry ids that do not collide with ids already in the
// file. A v1 file has none, but a partially-migrated one might.
type idMinter struct {
	taken map[string]bool
}

func newIDMinter(records []map[string]any) *idMinter {
	m := &idMinter{taken: map[string]bool{}}
	for _, rec := range records {
		if rec == nil {
			continue
		}
		if id, ok := rec["id"].(string); ok && id != "" {
			m.taken[id] = true
		}
	}
	return m
}

func (m *idMinter) mint() (string, error) {
	for i := 0; i < 100; i++ {
		u, err := uuid.NewV7()
		if err != nil {
			continue
		}
		s := u.String()
		id := s[len(s)-8:]
		if !m.taken[id] {
			m.taken[id] = true
			return id, nil
		}
	}
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("failed to generate an entry id: %w", err)
	}
	m.taken[u.String()] = true
	return u.String(), nil
}
