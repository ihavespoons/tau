// Package slashcmd is tau's slash-command engine: parsing, a registry, and
// the built-in command set. Port of Pi's core/slash-commands.ts plus the
// dispatch surface the TUI and extensions both drive.
package slashcmd

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Source identifies where a command came from.
type Source string

const (
	SourceBuiltin   Source = "builtin"
	SourceExtension Source = "extension"
	SourcePrompt    Source = "prompt"
	SourceSkill     Source = "skill"
)

// Info is a command's metadata, as shown in help and autocomplete.
type Info struct {
	Name         string
	Description  string
	ArgumentHint string
	Source       Source
	// SourceInfo locates the definition (a file path, an extension name).
	SourceInfo string
}

// Item is an autocomplete suggestion for a command's arguments.
type Item struct {
	Value string
	Label string
}

// Result is the outcome of running a command.
type Result struct {
	// Output is shown to the user.
	Output string
	// Prompt, when set, is submitted to the agent as a user message. This is
	// how /skill: and prompt templates work.
	Prompt string
	// Quit requests that the session end.
	Quit bool
}

// Command is an executable slash command.
type Command interface {
	Info() Info
	Run(ctx context.Context, args string) (Result, error)
}

// Completer is an optional interface for argument autocomplete.
type Completer interface {
	Complete(prefix string) []Item
}

// ErrNotImplemented marks a command that exists and is advertised but whose
// behavior needs a surface tau does not have yet (most need the TUI).
var ErrNotImplemented = fmt.Errorf("not implemented in this phase")

// Registry holds the available commands.
//
// It is not safe for concurrent mutation; register everything during setup,
// then read.
type Registry struct {
	order []string
	byName map[string]Command
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Command{}}
}

// Register adds a command. A duplicate name is suffixed (:1, :2, …) rather
// than replacing the incumbent, so an extension cannot silently shadow a
// built-in.
func (r *Registry) Register(c Command) string {
	name := c.Info().Name
	final := name
	for i := 1; ; i++ {
		if _, taken := r.byName[final]; !taken {
			break
		}
		final = fmt.Sprintf("%s:%d", name, i)
	}
	r.byName[final] = c
	r.order = append(r.order, final)
	return final
}

// Lookup finds a command by name.
func (r *Registry) Lookup(name string) (Command, bool) {
	c, ok := r.byName[name]
	return c, ok
}

// List returns command metadata in registration order.
func (r *Registry) List() []Info {
	out := make([]Info, 0, len(r.order))
	for _, n := range r.order {
		info := r.byName[n].Info()
		info.Name = n // reflect any duplicate suffix
		out = append(out, info)
	}
	return out
}

// Names returns registered names, sorted.
func (r *Registry) Names() []string {
	out := append([]string{}, r.order...)
	sort.Strings(out)
	return out
}

// Complete returns argument suggestions for a command.
func (r *Registry) Complete(name, prefix string) []Item {
	c, ok := r.byName[name]
	if !ok {
		return nil
	}
	if comp, ok := c.(Completer); ok {
		return comp.Complete(prefix)
	}
	return nil
}

// commandPattern matches "/name" optionally followed by whitespace and an
// argument tail.
//
// The name character class excludes "/", and the tail must start with
// whitespace or not exist at all — so "/usr/bin/env" fails to match entirely
// rather than parsing as command "usr" with arguments "/bin/env".
var commandPattern = regexp.MustCompile(`^/([A-Za-z0-9_:.-]+)(?:\s+([\s\S]*))?$`)

// Parsed is a parsed command line.
type Parsed struct {
	Name string
	Args string
	// SkillName is set when the input was /skill:<name>.
	SkillName string
}

// Parse splits a slash-command line.
//
// It returns false for text that merely begins with a slash — a path such as
// /usr/bin/env is not a command, because a command name cannot contain a
// slash.
func Parse(text string) (Parsed, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") || trimmed == "/" {
		return Parsed{}, false
	}
	m := commandPattern.FindStringSubmatch(trimmed)
	if m == nil {
		return Parsed{}, false
	}
	p := Parsed{Name: m[1], Args: strings.TrimSpace(m[2])}
	if rest, ok := strings.CutPrefix(p.Name, "skill:"); ok && rest != "" {
		p.SkillName = rest
	}
	return p, true
}

// funcCommand adapts a function to Command.
type funcCommand struct {
	info Info
	run  func(ctx context.Context, args string) (Result, error)
	comp func(prefix string) []Item
}

func (c *funcCommand) Info() Info { return c.info }

func (c *funcCommand) Run(ctx context.Context, args string) (Result, error) {
	if c.run == nil {
		return Result{}, fmt.Errorf("/%s: %w", c.info.Name, ErrNotImplemented)
	}
	return c.run(ctx, args)
}

func (c *funcCommand) Complete(prefix string) []Item {
	if c.comp == nil {
		return nil
	}
	return c.comp(prefix)
}

// New builds a command from a function.
func New(info Info, run func(ctx context.Context, args string) (Result, error)) Command {
	if info.Source == "" {
		info.Source = SourceExtension
	}
	return &funcCommand{info: info, run: run}
}

// NewWithCompleter builds a command that offers argument autocomplete.
func NewWithCompleter(info Info, run func(ctx context.Context, args string) (Result, error), comp func(prefix string) []Item) Command {
	if info.Source == "" {
		info.Source = SourceExtension
	}
	return &funcCommand{info: info, run: run, comp: comp}
}
