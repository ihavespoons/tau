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
//
// Each tool carries the system-prompt snippet and guidelines Pi gives it, so
// prompt.Build can advertise them without a second source of truth.
func CodingTools(e env.Env) []agent.Tool {
	return []agent.Tool{
		promptMeta(Read(e),
			"Read file contents",
			"Use read to examine files instead of cat or sed."),
		// Pi's bash guideline points the model at PI_* environment variables
		// carrying model and session metadata. tau does not export those yet
		// (they land with the environment-variable work in P9), and telling
		// the model to inspect variables that do not exist just buys a wasted
		// tool call — so the guideline stays out until the feature is real.
		promptMeta(Bash(e),
			"Execute bash commands (ls, grep, find, etc.)"),
		promptMeta(Edits(e),
			"Make precise file edits with exact text replacement, including multiple disjoint edits in one call",
			"Use edit for precise changes (edits[].oldText must match exactly)",
			"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
			"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
			"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions."),
		promptMeta(Write(e),
			"Create or overwrite files",
			"Use write only for new files or complete rewrites."),
	}
}
