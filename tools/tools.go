// Package tools implements tau's built-in agent tools, ported from Pi's
// coding-agent tool set. Each tool executes against an env.Env, so the same
// tool works locally, in a sandbox, or over a remote transport.
package tools

import (
	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
)

// CodingTools is the default tool set for a coding agent: read, bash, edit,
// write — the same four Pi's createCodingTools ships.
func CodingTools(e env.Env) []agent.Tool {
	return []agent.Tool{Read(e), Bash(e), Edits(e), Write(e)}
}
