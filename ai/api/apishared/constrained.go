package apishared

import (
	"fmt"

	"github.com/ihavespoons/tau/ai"
)

// Port of the JSON-schema half of Pi's api/constrained-sampling.ts. The grammar
// half belongs to the OpenAI wires and is not ported yet.

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
