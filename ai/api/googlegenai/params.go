package googlegenai

import (
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// request is the generateContent body.
type request struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Tools             []toolDeclaration `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type toolDeclaration struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// ParametersJSONSchema takes full JSON Schema — anyOf, const, and the rest.
	ParametersJSONSchema any `json:"parametersJsonSchema,omitempty"`
	// Parameters is the legacy OpenAPI 3.0 field, needed where the API
	// translates it into another vendor's schema format.
	Parameters any `json:"parameters,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
}

type functionCallingConfig struct {
	Mode string `json:"mode"`
}

type generationConfig struct {
	Temperature     *float64        `json:"temperature,omitempty"`
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	// IncludeThoughts asks for thought summaries. Without it the model still
	// reasons, but tau sees none of it.
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
	// ThinkingLevel is Gemini 3's control.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	// ThinkingBudget is Gemini 2's, in tokens. Zero disables thinking, which
	// is why it is a pointer: omitting it and sending zero mean the opposite.
	ThinkingBudget *int `json:"thinkingBudget,omitempty"`
}

// buildRequest assembles the payload for one turn.
func buildRequest(model *ai.Model, c ai.Context, opts *Options) request {
	req := request{Contents: convertMessages(model, c)}

	if c.SystemPrompt != "" {
		req.SystemInstruction = &content{
			Parts: []part{{Text: apishared.SanitizeSurrogates(c.SystemPrompt)}},
		}
	}

	if len(c.Tools) > 0 {
		req.Tools = convertTools(c.Tools, opts.UseLegacyParameters)
		if mode := functionCallingMode(model, c.Tools, opts.ToolChoice); mode != "" {
			req.ToolConfig = &toolConfig{FunctionCallingConfig: functionCallingConfig{Mode: mode}}
		}
	}

	gen := generationConfig{Temperature: opts.Temperature, MaxOutputTokens: opts.MaxTokens}
	gen.ThinkingConfig = buildThinkingConfig(model, opts)
	if gen != (generationConfig{}) {
		req.GenerationConfig = &gen
	}
	return req
}

func convertTools(tools []ai.Tool, legacyParameters bool) []toolDeclaration {
	decls := make([]functionDeclaration, 0, len(tools))
	for _, t := range tools {
		d := functionDeclaration{Name: t.Name, Description: t.Description}
		if legacyParameters {
			d.Parameters = sanitizeForOpenAPI(schemaAsMap(t))
		} else {
			d.ParametersJSONSchema = t.Parameters
		}
		decls = append(decls, d)
	}
	return []toolDeclaration{{FunctionDeclarations: decls}}
}

// functionCallingMode picks how strictly the model must obey the tool schemas.
//
// VALIDATED is Gemini 3's enforcement of required parameters, and is what
// stops a model inventing a call with half its arguments missing. It is only
// available from Gemini 3 on, so asking for it earlier would be rejected.
func functionCallingMode(model *ai.Model, tools []ai.Tool, toolChoice string) string {
	switch toolChoice {
	case "none":
		return "NONE"
	case "any":
		return "ANY"
	}
	if supportsStrictSampling(model.ID) {
		return "VALIDATED"
	}
	if toolChoice == "auto" {
		return "AUTO"
	}
	return ""
}

func supportsStrictSampling(modelID string) bool {
	return geminiMajorVersion(modelID) >= 3
}

var (
	gemini3Pro   = regexp.MustCompile(`gemini-3(?:\.\d+)?-pro`)
	gemini3Flash = regexp.MustCompile(`gemini-3(?:\.\d+)?-flash`)
	gemma4       = regexp.MustCompile(`gemma-?4`)
)

func isGemini3Pro(id string) bool { return gemini3Pro.MatchString(strings.ToLower(id)) }

func isGemini3Flash(id string) bool {
	low := strings.ToLower(id)
	return gemini3Flash.MatchString(low) || low == "gemini-flash-latest" || low == "gemini-flash-lite-latest"
}

func isGemma4(id string) bool { return gemma4.MatchString(strings.ToLower(id)) }

// buildThinkingConfig translates tau's thinking level into whichever control
// this model generation exposes.
func buildThinkingConfig(model *ai.Model, opts *Options) *thinkingConfig {
	if !model.Reasoning {
		return nil
	}
	if opts.Reasoning == "" {
		return disabledThinking(model)
	}

	cfg := &thinkingConfig{IncludeThoughts: true}
	if level := thinkingLevelFor(model, opts.Reasoning); level != "" {
		cfg.ThinkingLevel = level
		return cfg
	}
	budget := thinkingBudget(model, opts.Reasoning, opts.ThinkingBudgets)
	cfg.ThinkingBudget = &budget
	return cfg
}

// disabledThinking is how each generation expresses "do not show me thinking".
//
// Gemini 3 Pro cannot be stopped from thinking at all, and Flash cannot be
// fully disabled either, so those get the lowest level WITHOUT includeThoughts
// — the model still reasons, tau just does not display it. Only Gemini 2
// accepts a genuine zero budget.
func disabledThinking(model *ai.Model) *thinkingConfig {
	switch {
	case isGemini3Pro(model.ID):
		return &thinkingConfig{ThinkingLevel: "LOW"}
	case isGemini3Flash(model.ID), isGemma4(model.ID):
		return &thinkingConfig{ThinkingLevel: "MINIMAL"}
	}
	zero := 0
	return &thinkingConfig{ThinkingBudget: &zero}
}

// thinkingLevelFor maps tau's level onto Google's, for the models that take a
// level. An empty result means this model wants a token budget instead.
func thinkingLevelFor(model *ai.Model, level ai.ThinkingLevel) string {
	if geminiMajorVersion(model.ID) < 3 && !isGemma4(model.ID) {
		return ""
	}
	switch {
	case isGemini3Pro(model.ID):
		// Pro exposes two settings, not four.
		switch level {
		case "minimal", "low":
			return "LOW"
		default:
			return "HIGH"
		}
	case isGemma4(model.ID):
		switch level {
		case "minimal", "low":
			return "MINIMAL"
		default:
			return "HIGH"
		}
	}
	switch level {
	case "minimal":
		return "MINIMAL"
	case "low":
		return "LOW"
	case "medium":
		return "MEDIUM"
	default:
		return "HIGH"
	}
}

// thinkingBudget is the token allowance for a Gemini 2 model.
//
// -1 means "let the model decide", which is the right default for a model tau
// has no measured budgets for: guessing a number would cap reasoning the model
// would otherwise size for the task.
func thinkingBudget(model *ai.Model, level ai.ThinkingLevel, custom *ai.ThinkingBudgets) int {
	if custom != nil {
		if v, ok := customBudget(custom, level); ok {
			return v
		}
	}

	var budgets map[ai.ThinkingLevel]int
	switch {
	case strings.Contains(model.ID, "2.5-pro"):
		budgets = map[ai.ThinkingLevel]int{"minimal": 128, "low": 2048, "medium": 8192, "high": 32768}
	case strings.Contains(model.ID, "2.5-flash-lite"):
		budgets = map[ai.ThinkingLevel]int{"minimal": 512, "low": 2048, "medium": 8192, "high": 24576}
	case strings.Contains(model.ID, "2.5-flash"):
		budgets = map[ai.ThinkingLevel]int{"minimal": 128, "low": 2048, "medium": 8192, "high": 24576}
	default:
		return -1
	}
	if v, ok := budgets[level]; ok {
		return v
	}
	return -1
}

func customBudget(budgets *ai.ThinkingBudgets, level ai.ThinkingLevel) (int, bool) {
	switch level {
	case "minimal":
		return deref(budgets.Minimal)
	case "low":
		return deref(budgets.Low)
	case "medium":
		return deref(budgets.Medium)
	case "high":
		return deref(budgets.High)
	}
	return 0, false
}

func deref(v *int) (int, bool) {
	if v == nil {
		return 0, false
	}
	return *v, true
}
