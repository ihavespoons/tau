package apishared

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ihavespoons/tau/ai"
)

// Port of Pi's api/constrained-sampling.ts: both halves of constrained
// sampling, shared because the OpenAI chat-completions and responses wires
// implement the same feature with different envelopes, and Bedrock wants the
// JSON-schema half on its own.

// ResolveJSONSchemaStrictSampling reports whether a tool should be declared
// with strict schema enforcement.
//
// A tool that merely prefers constrained sampling degrades quietly on a model
// that cannot do it. One that requires it must not: silently dropping the
// constraint would let the model return arguments the tool has already been
// told it will never receive.
func ResolveJSONSchemaStrictSampling(tool ai.Tool, supportsStrictMode bool) (bool, error) {
	cfg := tool.ConstrainedSampling
	if cfg == nil || cfg.Disabled || cfg.Type != "json_schema" {
		return false, nil
	}
	if supportsStrictMode {
		return true, nil
	}
	if cfg.Strict == "require" {
		return false, fmt.Errorf(
			"tool %q requires JSON-schema constrained sampling, but strict tools are unsupported", tool.Name)
	}
	return false, nil
}

// GrammarSampling is a resolved grammar constraint: the grammar itself, and the
// single tool argument the model's output becomes.
//
// A grammar tool is not called with JSON. The model emits raw text matching the
// grammar — a regex, a SQL statement, a diff — and the provider hands it back
// as an opaque string. InputProperty is where that string is put so the rest of
// tau, which only knows tools with JSON arguments, sees an ordinary tool call.
type GrammarSampling struct {
	// Format is "lark" or "regex".
	Format string
	// Definition is the grammar text.
	Definition string
	// InputProperty is the tool's single required string parameter.
	InputProperty string
}

// ResolveGrammarConstrainedSampling returns the grammar a tool should be
// declared with, or nil if it wants none.
//
// A provider that cannot do grammar tools gets nil rather than an error: the
// tool still works as an ordinary JSON-schema tool, which is a real degradation
// but a working one. An unusable REQUEST — a grammar config with no variant tau
// can send, or a parameter schema the input property cannot be inferred from —
// is an error, because there is nothing sensible to fall back to.
func ResolveGrammarConstrainedSampling(tool ai.Tool, supportsGrammarTools bool) (*GrammarSampling, error) {
	cfg := tool.ConstrainedSampling
	if cfg == nil || cfg.Disabled || cfg.Type != "grammar" {
		return nil, nil
	}
	if !supportsGrammarTools {
		return nil, nil
	}

	lark := strings.TrimSpace(cfg.Variants[ai.GrammarOpenAILark])
	regex := strings.TrimSpace(cfg.Variants[ai.GrammarOpenAIRegex])
	format, definition := "lark", cfg.Variants[ai.GrammarOpenAILark]
	if lark == "" {
		format, definition = "regex", cfg.Variants[ai.GrammarOpenAIRegex]
	}
	if lark == "" && regex == "" {
		return nil, fmt.Errorf(
			"tool %q cannot use grammar constrained sampling: no supported grammar variant was provided", tool.Name)
	}

	property, err := grammarInputProperty(tool)
	if err != nil {
		return nil, fmt.Errorf("tool %q cannot use grammar constrained sampling: %w", tool.Name, err)
	}
	return &GrammarSampling{Format: format, Definition: definition, InputProperty: property}, nil
}

// grammarInputProperty infers which parameter receives the model's output.
//
// It is inferred rather than declared because a grammar tool has exactly one
// thing it can accept: the text the grammar produced. A schema with two
// required properties has no answer to "which one is the output", and guessing
// would put the model's work in the wrong field.
func grammarInputProperty(tool ai.Tool) (string, error) {
	schema := tool.Parameters
	if schema == nil || schema.Type != "object" {
		return "", fmt.Errorf("grammar constrained sampling requires an object parameter schema")
	}
	if len(schema.Required) != 1 {
		return "", fmt.Errorf("grammar constrained sampling requires exactly one required string property")
	}
	property := schema.Required[0]
	spec, ok := schema.Properties[property]
	if !ok || spec == nil {
		return "", fmt.Errorf("grammar constrained sampling requires a properties entry for %s", property)
	}
	if spec.Type != "string" {
		return "", fmt.Errorf("grammar constrained sampling property %s must have type string", property)
	}
	return property, nil
}

// GrammarToolInputProperties maps tool name to input property for every tool in
// the turn that will be declared as a grammar tool.
//
// The map is built once per request and consulted everywhere a tool call is
// converted, because by then the tool definition is gone: a replayed assistant
// message from a session file carries only the name and the arguments, and
// nothing in it says the call was a grammar call.
func GrammarToolInputProperties(tools []ai.Tool, supportsGrammarTools bool) (map[string]string, error) {
	if !supportsGrammarTools {
		return nil, nil
	}
	var out map[string]string
	for _, t := range tools {
		grammar, err := ResolveGrammarConstrainedSampling(t, true)
		if err != nil {
			return nil, err
		}
		if grammar == nil {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[t.Name] = grammar.InputProperty
	}
	return out, nil
}

// GrammarToolInput extracts the raw text to replay for a grammar tool call.
//
// The provider wants the string the model produced, not a JSON object. If the
// argument is missing or is not a string the call cannot be replayed at all,
// and failing here beats sending "undefined" as a SQL statement.
func GrammarToolInput(toolName string, arguments map[string]any, inputProperty string) (string, error) {
	value, ok := arguments[inputProperty].(string)
	if !ok {
		return "", fmt.Errorf("grammar tool call %q requires argument %q to be a string", toolName, inputProperty)
	}
	return value, nil
}

// GrammarInputBuffer turns a grammar tool's streamed plain text back into the
// JSON-argument deltas the rest of tau expects.
//
// Everything downstream — the TUI's tool renderer, the JSON mode, extensions
// watching toolcall_delta — was written against tool arguments that arrive as
// JSON text. A grammar tool streams a bare string instead. Rather than teach
// every consumer a second shape, the string is re-wrapped as it arrives:
// `{"query":"SELECT` … ` 1"}`. The result parses with the same partial-JSON
// salvage as any other call.
type GrammarInputBuffer struct {
	input   string
	started bool
	closed  bool
}

// Input is the text accumulated so far.
func (b *GrammarInputBuffer) Input() string { return b.input }

// AppendJSONDelta advances the buffer to nextInput and returns the JSON
// fragment representing what was added. An empty fragment means there was
// nothing new to emit.
//
// close writes the closing quote and brace. The provider sends the complete
// input again on its done event, so repeating an already-closed value is
// tolerated; anything else after close, or an input that does not extend what
// came before, means the stream contradicted itself and is reported rather than
// patched over — the arguments would otherwise be silently wrong.
func (b *GrammarInputBuffer) AppendJSONDelta(inputProperty, nextInput string, close bool) (string, error) {
	if b.closed {
		if close && nextInput == b.input {
			return "", nil
		}
		return "", fmt.Errorf("grammar tool input for property %q changed after it was closed", inputProperty)
	}
	if !strings.HasPrefix(nextInput, b.input) {
		return "", fmt.Errorf("grammar tool input for property %q changed non-monotonically", inputProperty)
	}

	added := nextInput[len(b.input):]
	if !close && added == "" {
		return "", nil
	}

	var delta strings.Builder
	if !b.started {
		name, err := encodeJSONString(inputProperty)
		if err != nil {
			return "", err
		}
		delta.WriteString("{" + name + `:"`)
		b.started = true
	}
	escaped, err := encodeJSONString(added)
	if err != nil {
		return "", err
	}
	// Without the surrounding quotes: this is a fragment of a string literal
	// that is still open.
	delta.WriteString(strings.TrimSuffix(strings.TrimPrefix(escaped, `"`), `"`))
	b.input = nextInput

	if close {
		delta.WriteString(`"}`)
		b.closed = true
	}
	return delta.String(), nil
}

// encodeJSONString quotes a string the way JSON.stringify does.
//
// HTML escaping is off deliberately. Go's default turns < > & into < and
// friends, which is valid JSON but different bytes — and a grammar tool's input
// is precisely the kind of payload full of them (a regex, a diff, HTML). The
// deltas would then disagree with what Pi produces for the same call.
func encodeJSONString(s string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", err
	}
	// Encode appends a newline.
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
