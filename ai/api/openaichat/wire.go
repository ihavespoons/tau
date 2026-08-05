package openaichat

import "encoding/json"

// Response wire types. Only the fields tau reads are declared; providers add
// their own freely and unknown keys are ignored.

type chunkPayload struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usagePayload `json:"usage"`
}

type chunkChoice struct {
	Delta        *deltaPayload `json:"delta"`
	FinishReason string        `json:"finish_reason"`
	// Usage here is Moonshot's non-standard placement.
	Usage *usagePayload `json:"usage"`
}

type deltaPayload struct {
	Content   string           `json:"content"`
	ToolCalls []*toolCallDelta `json:"tool_calls"`

	// Reasoning has three spellings in the wild: llama.cpp uses
	// reasoning_content, most OpenAI-compatible gateways use reasoning, and a
	// few use reasoning_text.
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
	ReasoningText    string `json:"reasoning_text"`

	// ReasoningDetails carries OpenRouter's encrypted reasoning payloads,
	// which must be replayed verbatim alongside the tool call they annotate.
	ReasoningDetails []json.RawMessage `json:"reasoning_details"`
}

// firstReasoning returns the field name and text of the first non-empty
// reasoning field. Taking only the first matters: some gateways populate two
// of them with identical text, and streaming both would double the thinking.
func (d *deltaPayload) firstReasoning() (string, string) {
	switch {
	case d.ReasoningContent != "":
		return "reasoning_content", d.ReasoningContent
	case d.Reasoning != "":
		return "reasoning", d.Reasoning
	case d.ReasoningText != "":
		return "reasoning_text", d.ReasoningText
	}
	return "", ""
}

type toolCallDelta struct {
	// Index is a pointer because 0 is a real index and absence is meaningful.
	Index    *int               `json:"index"`
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function *toolCallDeltaFunc `json:"function"`
	// Custom carries a grammar tool's output, which streams as raw text rather
	// than as JSON arguments.
	Custom *toolCallDeltaCust `json:"custom"`
}

type toolCallDeltaFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCallDeltaCust struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

// name returns whichever of the two shapes carried it.
func (t *toolCallDelta) name() string {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name
	}
	if t.Custom != nil {
		return t.Custom.Name
	}
	return ""
}

type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// PromptCacheHitTokens is DeepSeek's spelling of a cache read.
	PromptCacheHitTokens    int             `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails     *promptDetails  `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionDets `json:"completion_tokens_details"`
}

type promptDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type completionDets struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}
