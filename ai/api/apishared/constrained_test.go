package apishared

import (
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
)

func grammarTool(variants map[ai.GrammarFormat]string, required []string, propType string) ai.Tool {
	props := map[string]*jsonschema.Schema{}
	for _, name := range required {
		props[name] = &jsonschema.Schema{Type: propType}
	}
	return ai.Tool{
		Name:        "sql",
		Description: "run a query",
		Parameters: &jsonschema.Schema{
			Type: "object", Properties: props, Required: required,
		},
		ConstrainedSampling: &ai.ConstrainedSampling{Type: "grammar", Variants: variants},
	}
}

// THE POINT: a grammar tool takes no JSON schema — the grammar IS the schema —
// so the one thing that must be inferred is which argument the model's output
// becomes. Get it wrong and the text lands in a field the tool never reads.
func TestGrammarResolutionFindsTheInputProperty(t *testing.T) {
	tool := grammarTool(map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "start: /.*/"}, []string{"query"}, "string")

	got, err := ResolveGrammarConstrainedSampling(tool, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "lark" || got.Definition != "start: /.*/" || got.InputProperty != "query" {
		t.Errorf("grammar %+v", got)
	}
}

// Lark is preferred where both are offered: it describes structure, where a
// regex only describes shape.
func TestLarkIsPreferredOverRegex(t *testing.T) {
	tool := grammarTool(map[ai.GrammarFormat]string{
		ai.GrammarOpenAILark:  "start: /.*/",
		ai.GrammarOpenAIRegex: "^.*$",
	}, []string{"query"}, "string")

	got, err := ResolveGrammarConstrainedSampling(tool, true)
	if err != nil || got.Format != "lark" {
		t.Errorf("grammar %+v, err %v", got, err)
	}
}

func TestRegexIsUsedWhenItIsTheOnlyVariant(t *testing.T) {
	tool := grammarTool(map[ai.GrammarFormat]string{ai.GrammarOpenAIRegex: "^SELECT .*$"}, []string{"query"}, "string")

	got, err := ResolveGrammarConstrainedSampling(tool, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "regex" || got.Definition != "^SELECT .*$" {
		t.Errorf("grammar %+v", got)
	}
}

// A whitespace-only definition is not a grammar; treating it as one would send
// the provider something it rejects, with the tool named as the culprit.
func TestABlankGrammarVariantIsNotUsable(t *testing.T) {
	tool := grammarTool(map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "   "}, []string{"query"}, "string")

	if _, err := ResolveGrammarConstrainedSampling(tool, true); err == nil {
		t.Error("a blank grammar was accepted")
	}
}

func TestAGrammarWithNoSupportedVariantIsAnError(t *testing.T) {
	tool := grammarTool(map[ai.GrammarFormat]string{"anthropic_lark": "start: /.*/"}, []string{"query"}, "string")

	_, err := ResolveGrammarConstrainedSampling(tool, true)
	if err == nil || !strings.Contains(err.Error(), "no supported grammar variant") {
		t.Errorf("error %v", err)
	}
	// And the tool is named: the user has to know WHICH tool to fix.
	if err != nil && !strings.Contains(err.Error(), `"sql"`) {
		t.Errorf("error %q does not name the tool", err)
	}
}

// THE POINT: with two required properties there is no answer to "which one is
// the output", and guessing would put the model's work in the wrong field.
func TestAnAmbiguousSchemaIsRejected(t *testing.T) {
	cases := map[string]ai.Tool{
		"two required properties": grammarTool(
			map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "g"}, []string{"query", "limit"}, "string"),
		"no required property": grammarTool(
			map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "g"}, nil, "string"),
		"the property is not a string": grammarTool(
			map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "g"}, []string{"query"}, "number"),
	}
	for name, tool := range cases {
		if _, err := ResolveGrammarConstrainedSampling(tool, true); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}

	// A required property with no entry in properties at all.
	tool := grammarTool(map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "g"}, []string{"query"}, "string")
	tool.Parameters.Properties = map[string]*jsonschema.Schema{"other": {Type: "string"}}
	if _, err := ResolveGrammarConstrainedSampling(tool, true); err == nil {
		t.Error("a required property with no schema entry was accepted")
	}

	// And a schema that is not an object at all.
	tool = grammarTool(map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "g"}, []string{"query"}, "string")
	tool.Parameters.Type = "string"
	if _, err := ResolveGrammarConstrainedSampling(tool, true); err == nil {
		t.Error("a non-object parameter schema was accepted")
	}
}

// THE POINT: a provider that cannot do grammar tools gets nil rather than an
// error. The tool still works as an ordinary JSON-schema tool — a real
// degradation, but a working one, and refusing would make the tool unusable on
// every provider but one.
func TestAnUnsupportedProviderDegradesRatherThanFailing(t *testing.T) {
	tool := grammarTool(map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "g"}, []string{"query"}, "string")

	got, err := ResolveGrammarConstrainedSampling(tool, false)
	if err != nil || got != nil {
		t.Errorf("grammar %+v, err %v", got, err)
	}
	// Even a config that could never be sent stays quiet: the provider was
	// never going to see it.
	broken := grammarTool(map[ai.GrammarFormat]string{}, []string{"query"}, "string")
	if got, err := ResolveGrammarConstrainedSampling(broken, false); err != nil || got != nil {
		t.Errorf("grammar %+v, err %v", got, err)
	}
}

func TestNonGrammarToolsResolveToNothing(t *testing.T) {
	plain := ai.Tool{Name: "read", Parameters: &jsonschema.Schema{Type: "object"}}
	if got, err := ResolveGrammarConstrainedSampling(plain, true); err != nil || got != nil {
		t.Errorf("grammar %+v, err %v", got, err)
	}

	schemaTool := plain
	schemaTool.ConstrainedSampling = &ai.ConstrainedSampling{Type: "json_schema", Strict: "require"}
	if got, err := ResolveGrammarConstrainedSampling(schemaTool, true); err != nil || got != nil {
		t.Errorf("grammar %+v, err %v", got, err)
	}

	// `false` in Pi's config: constrained sampling explicitly turned off.
	disabled := plain
	disabled.ConstrainedSampling = &ai.ConstrainedSampling{Disabled: true, Type: "grammar"}
	if got, err := ResolveGrammarConstrainedSampling(disabled, true); err != nil || got != nil {
		t.Errorf("grammar %+v, err %v", got, err)
	}
}

func TestGrammarToolInputPropertiesMapsEveryGrammarTool(t *testing.T) {
	tools := []ai.Tool{
		{Name: "read", Parameters: &jsonschema.Schema{Type: "object"}},
		grammarTool(map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "g"}, []string{"query"}, "string"),
	}

	props, err := GrammarToolInputProperties(tools, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props["sql"] != "query" {
		t.Errorf("properties %+v", props)
	}

	// Nothing is mapped where the provider cannot do grammar tools, which is
	// what makes the calls replay as ordinary function calls.
	if props, err := GrammarToolInputProperties(tools, false); err != nil || props != nil {
		t.Errorf("properties %+v, err %v", props, err)
	}
}

// THE POINT: a tool that REQUIRES schema enforcement must not be sent to a
// provider that will not enforce it. One that merely prefers it degrades.
func TestStrictSamplingRequiresWhatItSaysItRequires(t *testing.T) {
	require := ai.Tool{Name: "extract", ConstrainedSampling: &ai.ConstrainedSampling{
		Type: "json_schema", Strict: "require",
	}}
	if _, err := ResolveJSONSchemaStrictSampling(require, false); err == nil {
		t.Error("a required constraint was silently dropped")
	}
	strict, err := ResolveJSONSchemaStrictSampling(require, true)
	if err != nil || !strict {
		t.Errorf("strict %v, err %v", strict, err)
	}

	prefer := ai.Tool{Name: "extract", ConstrainedSampling: &ai.ConstrainedSampling{
		Type: "json_schema", Strict: "prefer",
	}}
	if strict, err := ResolveJSONSchemaStrictSampling(prefer, false); err != nil || strict {
		t.Errorf("strict %v, err %v", strict, err)
	}

	plain := ai.Tool{Name: "read"}
	if strict, err := ResolveJSONSchemaStrictSampling(plain, true); err != nil || strict {
		t.Errorf("a tool that asked for nothing got strict=%v", strict)
	}
}

// THE POINT: everything downstream of a wire — the transcript renderer, JSON
// mode, extensions watching toolcall_delta — was written against arguments that
// arrive as JSON text. A grammar tool streams a bare string, so it is re-wrapped
// as it arrives rather than teaching every consumer a second shape.
func TestTheInputBufferRebuildsJSONArgumentDeltas(t *testing.T) {
	var buf GrammarInputBuffer

	first, err := buf.AppendJSONDelta("query", "SELECT", false)
	if err != nil {
		t.Fatal(err)
	}
	if first != `{"query":"SELECT` {
		t.Errorf("first delta %q", first)
	}

	second, err := buf.AppendJSONDelta("query", "SELECT 1", false)
	if err != nil {
		t.Fatal(err)
	}
	if second != " 1" {
		t.Errorf("second delta %q", second)
	}

	last, err := buf.AppendJSONDelta("query", "SELECT 1", true)
	if err != nil {
		t.Fatal(err)
	}
	if last != `"}` {
		t.Errorf("closing delta %q", last)
	}
	if buf.Input() != "SELECT 1" {
		t.Errorf("input %q", buf.Input())
	}

	// The concatenation is the point: it has to parse.
	if first+second+last != `{"query":"SELECT 1"}` {
		t.Errorf("assembled %q", first+second+last)
	}
}

// A grammar tool's output is exactly the kind of payload full of quotes,
// newlines and backslashes — it is code. Unescaped, the assembled JSON would
// not parse.
func TestTheInputBufferEscapesWhatJSONRequires(t *testing.T) {
	var buf GrammarInputBuffer
	raw := "say \"hi\"\n\tand \\ that"

	delta, err := buf.AppendJSONDelta("code", raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(delta[len(`{"code":"`):], "\n") {
		t.Errorf("a raw newline survived: %q", delta)
	}
	if !strings.Contains(delta, `\"hi\"`) || !strings.Contains(delta, `\\ that`) {
		t.Errorf("delta %q", delta)
	}
}

// THE POINT: Go's JSON encoder escapes < > & by default. A grammar tool's input
// is precisely the kind of text full of them — HTML, a diff, a regex — and the
// deltas would then disagree with what every other client produces for the same
// call, for no gain.
func TestTheInputBufferDoesNotEscapeHTML(t *testing.T) {
	var buf GrammarInputBuffer

	delta, err := buf.AppendJSONDelta("html", `<div class="a"> & </div>`, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(delta, escaped) {
			t.Errorf("HTML was escaped as %s: %q", escaped, delta)
		}
	}
	if !strings.Contains(delta, `<div`) || !strings.Contains(delta, `& `) {
		t.Errorf("delta %q", delta)
	}
}

// A property name is escaped too: it is a JSON key, and tau does not get to
// assume the tool author picked an identifier.
func TestThePropertyNameIsEscaped(t *testing.T) {
	var buf GrammarInputBuffer

	delta, err := buf.AppendJSONDelta(`odd"name`, "x", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(delta, `{"odd\"name":"`) {
		t.Errorf("delta %q", delta)
	}
}

// THE POINT: the buffer emits DIFFERENCES. An input that does not extend what
// came before means the stream contradicted itself, and patching over it would
// produce arguments that are silently wrong.
func TestNonMonotonicInputIsRejected(t *testing.T) {
	var buf GrammarInputBuffer
	if _, err := buf.AppendJSONDelta("query", "SELECT 1", false); err != nil {
		t.Fatal(err)
	}
	_, err := buf.AppendJSONDelta("query", "DROP TABLE", false)
	if err == nil || !strings.Contains(err.Error(), "non-monotonically") {
		t.Errorf("error %v", err)
	}
}

// The provider repeats the complete input on its done event, so closing twice
// with the same value is normal. Closing with a DIFFERENT value is not.
func TestClosingTwiceIsToleratedOnlyForTheSameValue(t *testing.T) {
	var buf GrammarInputBuffer
	if _, err := buf.AppendJSONDelta("query", "SELECT 1", true); err != nil {
		t.Fatal(err)
	}

	delta, err := buf.AppendJSONDelta("query", "SELECT 1", true)
	if err != nil {
		t.Fatalf("repeating the final value must be tolerated: %v", err)
	}
	if delta != "" {
		t.Errorf("a repeated close emitted %q", delta)
	}

	if _, err := buf.AppendJSONDelta("query", "SELECT 2", true); err == nil {
		t.Error("the input changed after it was closed and was accepted")
	}
	if _, err := buf.AppendJSONDelta("query", "SELECT 1 more", false); err == nil {
		t.Error("more input arrived after close and was accepted")
	}
}

// A delta that adds nothing emits nothing: an empty toolcall_delta is noise in
// every consumer.
func TestAnEmptyAdvanceEmitsNothing(t *testing.T) {
	var buf GrammarInputBuffer
	if _, err := buf.AppendJSONDelta("query", "SELECT", false); err != nil {
		t.Fatal(err)
	}
	delta, err := buf.AppendJSONDelta("query", "SELECT", false)
	if err != nil {
		t.Fatal(err)
	}
	if delta != "" {
		t.Errorf("delta %q", delta)
	}
}

// An empty input still has to produce parseable arguments — the model matched
// the grammar with nothing.
func TestAnEmptyInputStillClosesToValidJSON(t *testing.T) {
	var buf GrammarInputBuffer
	delta, err := buf.AppendJSONDelta("query", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if delta != `{"query":""}` {
		t.Errorf("delta %q", delta)
	}
}

// THE POINT: replaying a grammar call means sending the raw text back. If the
// argument is missing or is not a string there is nothing to send, and failing
// beats posting "<nil>" as a SQL statement.
func TestGrammarToolInputRequiresAString(t *testing.T) {
	got, err := GrammarToolInput("sql", map[string]any{"query": "SELECT 1"}, "query")
	if err != nil || got != "SELECT 1" {
		t.Errorf("input %q, err %v", got, err)
	}

	for name, args := range map[string]map[string]any{
		"missing":   {},
		"not text":  {"query": 42},
		"null":      {"query": nil},
		"wrong key": {"sql": "SELECT 1"},
	} {
		if _, err := GrammarToolInput("sql", args, "query"); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}

// THE POINT: the empty-advance guard matters on the FIRST delta. Without it an
// empty opening delta writes `{"query":"` before there is anything to put in
// it — and if the call then ends, the arguments are an unterminated string.
//
// Mutation testing found this: removing the guard changed nothing for a delta
// arriving after the buffer had already opened, which is all the tests had.
func TestAnEmptyFirstDeltaDoesNotOpenTheJSON(t *testing.T) {
	var buf GrammarInputBuffer

	delta, err := buf.AppendJSONDelta("query", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if delta != "" {
		t.Errorf("an empty first delta emitted %q", delta)
	}

	// And the real first delta still opens it exactly once.
	delta, err = buf.AppendJSONDelta("query", "SELECT 1", true)
	if err != nil {
		t.Fatal(err)
	}
	if delta != `{"query":"SELECT 1"}` {
		t.Errorf("delta %q", delta)
	}
}
