package extension

import (
	"sync/atomic"

	"github.com/ihavespoons/tau/ai"
)

// Mode is the host's UI mode, so extensions can degrade gracefully.
type Mode string

const (
	ModeTUI   Mode = "tui"
	ModeRPC   Mode = "rpc"
	ModeJSON  Mode = "json"
	ModePrint Mode = "print"
)

// Context is what handlers receive alongside their event. It is a live view
// of the session, guarded against use after the session is replaced.
type Context struct {
	runner *Runner
	// generation stamps the session this context belongs to. A mismatch with
	// the runner's current generation means the context is stale.
	generation uint64

	// systemPrompt is overridden during before_agent_start dispatch so
	// handlers observe the chained value.
	systemPrompt *string
}

// stale reports whether the context outlived its session.
func (c *Context) stale() bool {
	return c.runner == nil || atomic.LoadUint64(&c.runner.generation) != c.generation
}

// Err returns ErrStale if this context belongs to a replaced session.
func (c *Context) Err() error {
	if c.stale() {
		return ErrStale
	}
	return nil
}

// Mode reports the host's UI mode.
func (c *Context) Mode() Mode {
	if c.stale() {
		return ""
	}
	return c.runner.mode
}

// HasUI reports whether an interactive UI is attached.
func (c *Context) HasUI() bool { return c.Mode() == ModeTUI }

// Cwd returns the working directory.
func (c *Context) Cwd() string {
	if c.stale() {
		return ""
	}
	return c.runner.cwd
}

// IsProjectTrusted reports whether project-scoped resources were loaded.
func (c *Context) IsProjectTrusted() bool {
	if c.stale() {
		return false
	}
	return c.runner.trusted
}

// Model returns the active model, or nil when the context is stale or no
// runtime is bound.
func (c *Context) Model() *ai.Model {
	if c.stale() || c.runner.runtime == nil {
		return nil
	}
	return c.runner.runtime.Model()
}

// ThinkingLevel returns the active reasoning level.
func (c *Context) ThinkingLevel() ai.ModelThinkingLevel {
	if c.stale() || c.runner.runtime == nil {
		return ""
	}
	return c.runner.runtime.ThinkingLevel()
}

// ActiveToolNames lists the tools currently available to the model.
func (c *Context) ActiveToolNames() []string {
	if c.stale() || c.runner.runtime == nil {
		return nil
	}
	return c.runner.runtime.ActiveToolNames()
}

// SystemPrompt returns the system prompt. During before_agent_start it
// reflects edits made by earlier handlers in the chain.
func (c *Context) SystemPrompt() string {
	if c.stale() {
		return ""
	}
	if c.systemPrompt != nil {
		return *c.systemPrompt
	}
	return c.runner.systemPrompt
}

// CommandContext extends Context with the session-control operations that are
// only legal from inside a command handler.
type CommandContext struct {
	*Context
}

// Result reports whether an operation was cancelled by an extension.
type Result struct {
	Cancelled bool
	Reason    string
}
