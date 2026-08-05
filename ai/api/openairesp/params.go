package openairesp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// request is the responses body.
type request struct {
	Model  string `json:"model"`
	Input  []item `json:"input"`
	Stream bool   `json:"stream"`

	// Store is always false: tau keeps its own transcript, and letting the
	// endpoint keep one too would make a session's history depend on server
	// state tau cannot see or delete.
	Store bool `json:"store"`

	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	ServiceTier     string   `json:"service_tier,omitempty"`

	PromptCacheKey       string             `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string             `json:"prompt_cache_retention,omitempty"`
	PromptCacheOptions   *promptCacheOption `json:"prompt_cache_options,omitempty"`

	Tools      []tool `json:"tools,omitempty"`
	ToolChoice any    `json:"tool_choice,omitempty"`

	Reasoning *reasoning `json:"reasoning,omitempty"`
	Include   []string   `json:"include,omitempty"`
}

type promptCacheOption struct {
	Mode string `json:"mode"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type tool struct {
	Type        string             `json:"type"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Parameters  *jsonschema.Schema `json:"parameters,omitempty"`
	Strict      *bool              `json:"strict,omitempty"`
	// Format declares a custom tool's grammar. A tool has a schema or a
	// format, never both.
	Format *toolFormat `json:"format,omitempty"`
	// DeferLoading marks a tool the model may search for rather than one
	// offered up front.
	DeferLoading bool `json:"defer_loading,omitempty"`
}

type toolFormat struct {
	Type string `json:"type"`
	// Syntax is "lark" or "regex".
	Syntax     string `json:"syntax"`
	Definition string `json:"definition"`
}

// minOutputTokens is the floor the endpoint enforces; a smaller value is
// rejected outright rather than clamped.
const minOutputTokens = 16

// buildRequest assembles the payload for one turn.
//
// grammar maps a tool name to the argument carrying its raw output, for the
// tools declared with a grammar. It is resolved by the caller because the
// streaming side needs the same answer.
//
// It can fail: a tool may require constrained sampling this host cannot honour,
// and sending the request anyway would let the model answer in a shape the tool
// has been told it will never receive.
func buildRequest(model *ai.Model, c ai.Context, opts *Options, cm compat, grammar map[string]string) (request, error) {
	retention := resolveCacheRetention(opts.CacheRetention, opts.Env)

	immediate, deferredTools := apishared.SplitDeferredTools(c, cm.SupportsToolSearch, nil)
	deferred := make(map[string]ai.Tool, len(deferredTools))
	for _, t := range deferredTools {
		deferred[t.Name] = t
	}

	input, err := convertMessages(model, c, cm, deferred, grammar)
	if err != nil {
		return request{}, err
	}

	req := request{
		Model:  model.ID,
		Input:  input,
		Stream: true,
		Store:  false,
	}

	if retention != ai.CacheNone {
		req.PromptCacheKey = clampPromptCacheKey(opts.SessionID)
	}
	if retention == ai.CacheLong && cm.SupportsLongCacheRetention {
		req.PromptCacheRetention = "24h"
	}
	// Explicit mode is how caching is turned OFF on the models that charge for
	// writes; on the rest the parameter is rejected, which is why it is gated.
	if retention == ai.CacheNone && cm.SupportsExplicitPromptCacheMode {
		req.PromptCacheOptions = &promptCacheOption{Mode: "explicit"}
	}

	if opts.MaxTokens > 0 {
		req.MaxOutputTokens = max(opts.MaxTokens, minOutputTokens)
	}
	if opts.Temperature != nil {
		req.Temperature = opts.Temperature
	}
	if opts.ServiceTier != "" {
		req.ServiceTier = opts.ServiceTier
	}
	if len(immediate) > 0 {
		tools, err := convertTools(immediate, cm, false)
		if err != nil {
			return request{}, err
		}
		req.Tools = tools
	}
	if opts.ToolChoice != nil {
		req.ToolChoice = opts.ToolChoice
	}

	applyReasoning(&req, model, opts)
	return req, nil
}

// applyReasoning sets the thinking request and asks for the encrypted payload
// that makes the next turn able to continue the same reasoning.
func applyReasoning(req *request, model *ai.Model, opts *Options) {
	if !model.Reasoning {
		return
	}

	if opts.Reasoning != "" || opts.ReasoningSummary != "" {
		effort := "medium"
		if opts.Reasoning != "" {
			effort = string(opts.Reasoning)
			if mapped, ok := mappedEffort(model, opts.Reasoning); ok {
				effort = mapped
			}
		}
		summary := opts.ReasoningSummary
		if summary == "" {
			summary = "auto"
		}
		req.Reasoning = &reasoning{Effort: effort, Summary: summary}
		// Without this the reasoning items come back with no payload, and a
		// replayed turn loses the model's train of thought.
		req.Include = []string{"reasoning.encrypted_content"}
		return
	}

	// Thinking off. Copilot rejects the parameter entirely, and a model whose
	// map marks "off" unsupported cannot express it.
	if model.Provider == "github-copilot" || !offSupported(model) {
		return
	}
	effort := "none"
	if v, ok := offValue(model); ok {
		effort = v
	}
	req.Reasoning = &reasoning{Effort: effort}
}

// mappedEffort translates a thinking level through the model's own map. A
// missing entry means the level passes through under its own name; an explicit
// nil means the model cannot express it.
func mappedEffort(model *ai.Model, level ai.ThinkingLevel) (string, bool) {
	if model.ThinkingLevelMap == nil {
		return string(level), level != ""
	}
	mapped, present := model.ThinkingLevelMap[ai.ModelThinkingLevel(level)]
	if !present {
		return string(level), level != ""
	}
	if mapped == nil {
		return "", false
	}
	return *mapped, true
}

// offSupported reports whether the model allows thinking to be turned off.
func offSupported(model *ai.Model) bool {
	if model.ThinkingLevelMap == nil {
		return true
	}
	mapped, present := model.ThinkingLevelMap[ai.ThinkingOff]
	if !present {
		return true
	}
	return mapped != nil
}

// offValue is what to send for "off", when the model names it.
func offValue(model *ai.Model) (string, bool) {
	if model.ThinkingLevelMap == nil {
		return "", false
	}
	mapped, present := model.ThinkingLevelMap[ai.ThinkingOff]
	if !present || mapped == nil {
		return "", false
	}
	return *mapped, true
}

func convertTools(tools []ai.Tool, cm compat, deferLoading bool) ([]tool, error) {
	out := make([]tool, 0, len(tools))
	for _, t := range tools {
		// A grammar tool is declared as a different KIND of tool: no JSON
		// schema at all, because the grammar is the schema.
		grammar, err := apishared.ResolveGrammarConstrainedSampling(t, cm.SupportsOpenAIGrammarTools)
		if err != nil {
			return nil, err
		}
		if grammar != nil {
			out = append(out, tool{
				Type: "custom", Name: t.Name, Description: t.Description,
				Format: &toolFormat{
					Type: "grammar", Syntax: grammar.Format, Definition: grammar.Definition,
				},
				DeferLoading: deferLoading,
			})
			continue
		}

		strict, err := apishared.ResolveJSONSchemaStrictSampling(t, cm.SupportsStrictMode)
		if err != nil {
			return nil, err
		}
		converted := tool{
			Type:         "function",
			Name:         t.Name,
			Description:  t.Description,
			Parameters:   t.Parameters,
			DeferLoading: deferLoading,
		}
		// `strict` is not universally understood, and a host that does not know
		// it rejects the request rather than ignoring the field. Where it IS
		// understood the field is always sent, false by default, so a tool that
		// asked for schema enforcement is the only one that gets it.
		if cm.SupportsStrictMode {
			converted.Strict = &strict
		}
		out = append(out, converted)
	}
	return out, nil
}

// maxPromptCacheKey is OpenAI's documented limit.
const maxPromptCacheKey = 64

func clampPromptCacheKey(sessionID string) string {
	if len(sessionID) <= maxPromptCacheKey {
		return sessionID
	}
	return sessionID[:maxPromptCacheKey]
}
