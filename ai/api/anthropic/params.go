package anthropic

// buildParams and message/tool conversion — port of Pi's buildParams,
// convertMessages, convertToolResult, convertTools, convertContentBlocks.

import (
	"encoding/json"
	"fmt"

	"github.com/ihavespoons/tau/ai"
)

// params marshals to Anthropic's MessageCreateParams (streaming).
type params struct {
	Model        string           `json:"model"`
	Messages     []map[string]any `json:"messages"`
	MaxTokens    int              `json:"max_tokens"`
	Stream       bool             `json:"stream"`
	System       []map[string]any `json:"system,omitempty"`
	Temperature  *float64         `json:"temperature,omitempty"`
	Tools        []map[string]any `json:"tools,omitempty"`
	Thinking     map[string]any   `json:"thinking,omitempty"`
	OutputConfig map[string]any   `json:"output_config,omitempty"`
	Metadata     map[string]any   `json:"metadata,omitempty"`
	ToolChoice   map[string]any   `json:"tool_choice,omitempty"`
}

// convertContentBlocks converts text+image content: text-only becomes one
// concatenated string, mixed content becomes a block array with a placeholder
// text block when only images are present.
func convertContentBlocks(content ai.ContentList) any {
	hasImages := false
	for _, c := range content {
		if _, ok := c.(ai.ImageContent); ok {
			hasImages = true
			break
		}
	}
	if !hasImages {
		text := ""
		for i, c := range content {
			if t, ok := c.(ai.TextContent); ok {
				if i > 0 {
					text += "\n"
				}
				text += t.Text
			}
		}
		return sanitizeSurrogates(text)
	}
	blocks := []map[string]any{}
	hasText := false
	for _, c := range content {
		switch b := c.(type) {
		case ai.TextContent:
			hasText = true
			blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(b.Text)})
		case ai.ImageContent:
			blocks = append(blocks, map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "base64", "media_type": b.MimeType, "data": b.Data},
			})
		}
	}
	if !hasText {
		blocks = append([]map[string]any{{"type": "text", "text": "(see attached image)"}}, blocks...)
	}
	return blocks
}

func buildParams(model *ai.Model, c ai.Context, oauth bool, opts *Options) (*params, error) {
	cc := cacheControlFor(model, opts.CacheRetention, opts.Env)
	comp := resolveCompat(model)
	transformed := transformMessages(c.Messages, model, normalizeToolCallID)

	normalizeName := func(s string) string { return s }
	if oauth {
		normalizeName = toClaudeCodeName
	}
	immediate, deferred := splitDeferredTools(ai.Context{SystemPrompt: c.SystemPrompt, Messages: transformed, Tools: c.Tools}, comp.supportsToolReferences, normalizeName)
	if len(immediate) == 0 && len(deferred) > 0 {
		immediate = deferred
		deferred = nil
	}
	deferredNames := map[string]bool{}
	for _, t := range deferred {
		deferredNames[normalizeName(t.Name)] = true
	}

	p := &params{
		Model:     model.ID,
		MaxTokens: orDefault(opts.MaxTokens, model.MaxTokens),
		Stream:    true,
	}
	msgs, err := convertMessages(transformed, oauth, cc, comp.allowEmptySignature, deferredNames, normalizeName)
	if err != nil {
		return nil, err
	}
	p.Messages = msgs

	// OAuth requires the Claude Code identity as the first system block.
	if oauth {
		block := map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."}
		if cc != nil {
			block["cache_control"] = cc
		}
		p.System = []map[string]any{block}
		if c.SystemPrompt != "" {
			userBlock := map[string]any{"type": "text", "text": sanitizeSurrogates(c.SystemPrompt)}
			if cc != nil {
				userBlock["cache_control"] = cc
			}
			p.System = append(p.System, userBlock)
		}
	} else if c.SystemPrompt != "" {
		block := map[string]any{"type": "text", "text": sanitizeSurrogates(c.SystemPrompt)}
		if cc != nil {
			block["cache_control"] = cc
		}
		p.System = []map[string]any{block}
	}

	// Temperature is incompatible with extended thinking and unsupported on
	// some models.
	thinkingEnabled := opts.ThinkingEnabled != nil && *opts.ThinkingEnabled
	if opts.Temperature != nil && !thinkingEnabled && comp.supportsTemperature {
		p.Temperature = opts.Temperature
	}

	if len(immediate) > 0 || len(deferred) > 0 {
		var toolCC map[string]any
		if comp.supportsCacheControlOnTools {
			toolCC = cc
		}
		tools, terr := convertTools(immediate, oauth, comp.supportsEagerToolInputStreaming, comp.supportsStrictTools, toolCC, false)
		if terr != nil {
			return nil, terr
		}
		deferredTools, terr := convertTools(deferred, oauth, comp.supportsEagerToolInputStreaming, comp.supportsStrictTools, nil, true)
		if terr != nil {
			return nil, terr
		}
		p.Tools = append(tools, deferredTools...)
	}

	if model.Reasoning {
		if thinkingEnabled {
			display := opts.ThinkingDisplay
			if display == "" {
				display = "summarized"
			}
			if comp.forceAdaptiveThinking {
				p.Thinking = map[string]any{"type": "adaptive", "display": display}
				if opts.Effort != "" {
					p.OutputConfig = map[string]any{"effort": opts.Effort}
				}
			} else {
				budget := opts.ThinkingBudgetTokens
				if budget == 0 {
					budget = 1024
				}
				p.Thinking = map[string]any{"type": "enabled", "budget_tokens": budget, "display": display}
			}
		} else if opts.ThinkingEnabled != nil && !*opts.ThinkingEnabled {
			// Only send disabled when "off" is not explicitly unsupported.
			if v, present := model.ThinkingLevelMap["off"]; !present || v != nil {
				p.Thinking = map[string]any{"type": "disabled"}
			}
		}
	}

	if opts.Metadata != nil {
		if userID, ok := opts.Metadata["user_id"].(string); ok {
			p.Metadata = map[string]any{"user_id": userID}
		}
	}

	if opts.ToolChoice != nil {
		switch tc := opts.ToolChoice.(type) {
		case string:
			p.ToolChoice = map[string]any{"type": tc}
		case map[string]any:
			p.ToolChoice = tc
		default:
			return nil, fmt.Errorf("anthropic: invalid toolChoice %T", opts.ToolChoice)
		}
	}

	return p, nil
}

func convertToolResult(msg ai.ToolResultMessage, oauth bool, deferredNames map[string]bool, loadedNames map[string]bool, normalizeName func(string) string) (toolResult map[string]any, siblingContent []map[string]any) {
	references := []map[string]any{}
	for _, name := range msg.AddedToolNames {
		normalized := normalizeName(name)
		if !deferredNames[normalized] || loadedNames[normalized] {
			continue
		}
		loadedNames[normalized] = true
		refName := name
		if oauth {
			refName = toClaudeCodeName(name)
		}
		references = append(references, map[string]any{"type": "tool_reference", "tool_name": refName})
	}
	converted := convertContentBlocks(msg.Content)
	content := converted
	if len(references) > 0 {
		content = references
	}
	result := map[string]any{
		"type":        "tool_result",
		"tool_use_id": msg.ToolCallID,
		"content":     content,
		"is_error":    msg.IsError,
	}
	if len(references) == 0 {
		return result, nil
	}
	// Displaced reference-bearing results follow every tool_result block.
	if s, ok := converted.(string); ok {
		return result, []map[string]any{{"type": "text", "text": s}}
	}
	return result, converted.([]map[string]any)
}

func convertMessages(transformed ai.MessageList, oauth bool, cc map[string]any, allowEmptySignature bool, deferredNames map[string]bool, normalizeName func(string) string) ([]map[string]any, error) {
	out := []map[string]any{}
	loadedNames := map[string]bool{}

	for i := 0; i < len(transformed); i++ {
		switch msg := transformed[i].(type) {
		case ai.UserMessage:
			if msg.Content.Blocks == nil {
				if trimSpace(msg.Content.Text) == "" {
					continue
				}
				out = append(out, map[string]any{"role": "user", "content": sanitizeSurrogates(msg.Content.Text)})
				continue
			}
			blocks := []map[string]any{}
			for _, item := range msg.Content.Blocks {
				switch b := item.(type) {
				case ai.TextContent:
					if trimSpace(b.Text) == "" {
						continue
					}
					blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(b.Text)})
				case ai.ImageContent:
					blocks = append(blocks, map[string]any{
						"type":   "image",
						"source": map[string]any{"type": "base64", "media_type": b.MimeType, "data": b.Data},
					})
				}
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": blocks})

		case ai.AssistantMessage:
			blocks := []map[string]any{}
			for _, block := range msg.Content {
				switch b := block.(type) {
				case ai.TextContent:
					if trimSpace(b.Text) == "" {
						continue
					}
					blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(b.Text)})
				case ai.ThinkingContent:
					if b.Redacted {
						blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": b.ThinkingSignature})
						continue
					}
					hasSig := trimSpace(b.ThinkingSignature) != ""
					if trimSpace(b.Thinking) == "" && !hasSig {
						continue
					}
					if !hasSig {
						if allowEmptySignature {
							blocks = append(blocks, map[string]any{"type": "thinking", "thinking": sanitizeSurrogates(b.Thinking), "signature": ""})
						} else {
							blocks = append(blocks, map[string]any{"type": "text", "text": sanitizeSurrogates(b.Thinking)})
						}
					} else {
						blocks = append(blocks, map[string]any{"type": "thinking", "thinking": sanitizeSurrogates(b.Thinking), "signature": b.ThinkingSignature})
					}
				case ai.ToolCall:
					name := b.Name
					if oauth {
						name = toClaudeCodeName(name)
					}
					args := b.Arguments
					if args == nil {
						args = map[string]any{}
					}
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": b.ID, "name": name, "input": args})
				}
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})

		case ai.ToolResultMessage:
			// Group consecutive toolResult messages into one user turn.
			toolResults := []map[string]any{}
			siblings := []map[string]any{}
			j := i
			for j < len(transformed) {
				tr, ok := transformed[j].(ai.ToolResultMessage)
				if !ok {
					break
				}
				result, sibling := convertToolResult(tr, oauth, deferredNames, loadedNames, normalizeName)
				toolResults = append(toolResults, result)
				siblings = append(siblings, sibling...)
				j++
			}
			i = j - 1
			out = append(out, map[string]any{"role": "user", "content": append(toolResults, siblings...)})
		}
	}

	// cache_control on the last user message caches conversation history.
	if cc != nil && len(out) > 0 {
		last := out[len(out)-1]
		if last["role"] == "user" {
			switch content := last["content"].(type) {
			case []map[string]any:
				if len(content) > 0 {
					lastBlock := content[len(content)-1]
					switch lastBlock["type"] {
					case "text", "image", "tool_result":
						lastBlock["cache_control"] = cc
					}
				}
			case string:
				last["content"] = []map[string]any{{"type": "text", "text": content, "cache_control": cc}}
			}
		}
	}

	return out, nil
}

// resolveJSONSchemaStrict ports resolveJsonSchemaStrictSampling.
func resolveJSONSchemaStrict(tool ai.Tool, supportsStrict bool) (bool, error) {
	cs := tool.ConstrainedSampling
	if cs == nil || cs.Disabled || cs.Type != "json_schema" {
		return false, nil
	}
	if supportsStrict {
		return true, nil
	}
	if cs.Strict == "require" {
		return false, fmt.Errorf("Tool %q requires JSON-schema constrained sampling, but strict tools are unsupported.", tool.Name) //nolint:staticcheck // Pi error-message parity
	}
	return false, nil
}

func schemaToMap(tool ai.Tool) (map[string]any, error) {
	if tool.Parameters == nil {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(tool.Parameters)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func convertTools(tools []ai.Tool, oauth bool, supportsEager, supportsStrict bool, cc map[string]any, deferLoading bool) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(tools))
	for idx, tool := range tools {
		strict, err := resolveJSONSchemaStrict(tool, supportsStrict)
		if err != nil {
			return nil, err
		}
		schema, err := schemaToMap(tool)
		if err != nil {
			return nil, err
		}
		properties := schema["properties"]
		if properties == nil {
			properties = map[string]any{}
		}
		required := schema["required"]
		if required == nil {
			required = []any{}
		}
		var inputSchema map[string]any
		if strict {
			inputSchema = map[string]any{}
			for k, v := range schema {
				inputSchema[k] = v
			}
		} else {
			inputSchema = map[string]any{}
		}
		inputSchema["type"] = "object"
		inputSchema["properties"] = properties
		inputSchema["required"] = required

		name := tool.Name
		if oauth {
			name = toClaudeCodeName(name)
		}
		t := map[string]any{
			"name":         name,
			"description":  tool.Description,
			"input_schema": inputSchema,
		}
		if supportsEager {
			t["eager_input_streaming"] = true
		}
		if strict {
			t["strict"] = true
		}
		if deferLoading {
			t["defer_loading"] = true
		}
		if cc != nil && idx == len(tools)-1 {
			t["cache_control"] = cc
		}
		out = append(out, t)
	}
	return out, nil
}
