package settings

import "encoding/json"

// migrate upgrades legacy settings shapes in place, porting Pi's
// migrateSettings (settings-manager.ts:381-440). Migration runs on every read,
// so an old file keeps working without being rewritten.
func migrate(raw map[string]json.RawMessage) map[string]json.RawMessage {
	if raw == nil {
		return map[string]json.RawMessage{}
	}
	out := cloneRaw(raw)

	// queueMode -> steeringMode
	if v, ok := out["queueMode"]; ok {
		if _, exists := out["steeringMode"]; !exists {
			out["steeringMode"] = v
		}
		delete(out, "queueMode")
	}

	// websockets: bool -> transport: "websocket" | "sse"
	if _, hasTransport := out["transport"]; !hasTransport {
		if v, ok := out["websockets"]; ok {
			var b bool
			if err := json.Unmarshal(v, &b); err == nil {
				if b {
					out["transport"] = json.RawMessage(`"websocket"`)
				} else {
					out["transport"] = json.RawMessage(`"sse"`)
				}
			}
		}
	}
	delete(out, "websockets")

	// skills: {enableSkillCommands, customDirectories} -> array + top-level flag
	if v, ok := out["skills"]; ok {
		if obj, isObj := asObject(v); isObj {
			if esc, has := obj["enableSkillCommands"]; has {
				if _, exists := out["enableSkillCommands"]; !exists {
					out["enableSkillCommands"] = esc
				}
			}
			if dirs, has := obj["customDirectories"]; has && isNonEmptyArray(dirs) {
				out["skills"] = dirs
			} else {
				delete(out, "skills")
			}
		}
	}

	// retry.maxDelayMs -> retry.provider.maxRetryDelayMs
	if v, ok := out["retry"]; ok {
		if retry, isObj := asObject(v); isObj {
			if maxDelay, has := retry["maxDelayMs"]; has {
				provider := map[string]json.RawMessage{}
				if p, hasProvider := retry["provider"]; hasProvider {
					if parsed, isProviderObj := asObject(p); isProviderObj {
						provider = parsed
					}
				}
				if existing, hasKey := provider["maxRetryDelayMs"]; !hasKey || string(existing) == "null" {
					provider["maxRetryDelayMs"] = maxDelay
				}
				if encoded, err := marshalStable(provider); err == nil {
					retry["provider"] = encoded
				}
			}
			delete(retry, "maxDelayMs")
			if encoded, err := marshalStable(retry); err == nil {
				out["retry"] = encoded
			}
		}
	}

	return out
}

func isNonEmptyArray(raw json.RawMessage) bool {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false
	}
	return len(arr) > 0
}
