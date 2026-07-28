package settings

import "encoding/json"

// merge implements Pi's deepMergeSettings (settings-manager.ts:132-160).
//
// Despite that function's doc comment claiming nested objects "merge
// recursively", the implementation is `{ ...baseValue, ...overrideValue }` —
// a ONE-LEVEL spread. So retry.provider replaces base.retry.provider wholesale
// rather than merging into it. This port matches the code, not the comment.
//
// Rules, in Pi's order:
//   - an absent override key is skipped (base wins)
//   - object-vs-object merges one level, override keys winning
//   - everything else (primitives, arrays, type mismatches) is replaced
func merge(base, override map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, ov := range override {
		if len(ov) == 0 || string(ov) == "null" {
			// Pi skips `undefined`; JSON null is a real value and replaces.
			if string(ov) == "null" {
				out[k] = ov
			}
			continue
		}
		bv, hasBase := base[k]
		if !hasBase {
			out[k] = ov
			continue
		}
		bo, baseIsObj := asObject(bv)
		oo, overIsObj := asObject(ov)
		if baseIsObj && overIsObj {
			spread := make(map[string]json.RawMessage, len(bo)+len(oo))
			for nk, nv := range bo {
				spread[nk] = nv
			}
			for nk, nv := range oo {
				spread[nk] = nv
			}
			if merged, err := marshalStable(spread); err == nil {
				out[k] = merged
				continue
			}
		}
		out[k] = ov
	}
	return out
}

// asObject reports whether raw is a JSON object (not an array or scalar) and
// decodes it if so.
func asObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, false
			}
			return m, true
		default:
			return nil, false
		}
	}
	return nil, false
}
