package coding

import (
	"strings"

	"github.com/ihavespoons/tau/tools"
)

// sessionEnv reports the session metadata bash commands can read, matching
// Pi's PI_* set (bash.ts:171-181) under tau's own prefix.
//
// It is a method rather than a captured snapshot because every value here
// changes while the session runs: /model switches the model, /new replaces
// the session file. The bash tool calls this once per command.
func (s *Session) sessionEnv() []string {
	var out []string
	add := func(key, value string) {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, key+"="+value)
		}
	}

	add("TAU_SESSION_ID", s.sessionID)
	add("TAU_SESSION_FILE", s.Path)
	if m := s.Model; m != nil {
		add("TAU_PROVIDER", string(m.Provider))
		add("TAU_MODEL", m.ID)
	}
	// Agent is nil for the moment between New building the session and New
	// building the agent. Nothing can run a command in that window, but the
	// tool set is already constructed, so the check earns its keep.
	if s.Agent != nil {
		add("TAU_REASONING_LEVEL", string(s.Agent.ThinkingLevel()))
	}
	return out
}

// compile-time check that the method still fits the tool set's expectation.
var _ tools.SessionEnv = (*Session)(nil).sessionEnv
