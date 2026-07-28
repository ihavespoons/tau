package ai

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// GrammarFormat is an OpenAI grammar variant for constrained sampling.
type GrammarFormat string

const (
	GrammarOpenAILark  GrammarFormat = "openai_lark"
	GrammarOpenAIRegex GrammarFormat = "openai_regex"
)

// ConstrainedSampling is Pi's `false | ConstrainedSamplingConfig`.
// Disabled=true corresponds to the literal `false`.
type ConstrainedSampling struct {
	Disabled bool
	Type     string // "json_schema" | "grammar"
	Strict   string // "prefer" | "require" (json_schema only)
	Variants map[GrammarFormat]string
}

func (c ConstrainedSampling) MarshalJSON() ([]byte, error) {
	if c.Disabled {
		return []byte("false"), nil
	}
	switch c.Type {
	case "grammar":
		return json.Marshal(struct {
			Type     string                   `json:"type"`
			Variants map[GrammarFormat]string `json:"variants"`
		}{c.Type, c.Variants})
	default:
		return json.Marshal(struct {
			Type   string `json:"type"`
			Strict string `json:"strict"`
		}{"json_schema", c.Strict})
	}
}

func (c *ConstrainedSampling) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		*c = ConstrainedSampling{Disabled: !b}
		return nil
	}
	var obj struct {
		Type     string                   `json:"type"`
		Strict   string                   `json:"strict"`
		Variants map[GrammarFormat]string `json:"variants"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*c = ConstrainedSampling{Type: obj.Type, Strict: obj.Strict, Variants: obj.Variants}
	return nil
}

// Tool is an LLM-facing tool definition. Parameters is a JSON Schema.
type Tool struct {
	Name                string               `json:"name"`
	Description         string               `json:"description"`
	Parameters          *jsonschema.Schema   `json:"parameters"`
	ConstrainedSampling *ConstrainedSampling `json:"constrainedSampling,omitempty"`
}
