package apishared

// Port of Pi's utils/estimate.ts (the subset simple-options needs) and
// api/simple-options.ts.
//
// These lived in the anthropic package until Bedrock needed them too. Bedrock
// runs Claude, so it wants the identical budget arithmetic — and two copies of
// "how many tokens may this turn spend thinking" would drift into two different
// answers for the same model.

import (
	"encoding/json"

	"github.com/ihavespoons/tau/ai"
)

const (
	charsPerToken       = 4
	estimatedImageChars = 4800
	contextSafetyTokens = 4096
	minMaxTokens        = 1
)

func calculateContextTokens(u ai.Usage) int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

func estimateContentChars(content ai.ContentList) int {
	chars := 0
	for _, block := range content {
		switch b := block.(type) {
		case ai.TextContent:
			chars += len(b.Text)
		case ai.ImageContent:
			chars += estimatedImageChars
		}
	}
	return chars
}

func ceilDiv(chars, per int) int { return (chars + per - 1) / per }

func estimateTextTokens(text string) int { return ceilDiv(len(text), charsPerToken) }

func estimateMessageTokens(msg ai.Message) int {
	switch m := msg.(type) {
	case ai.UserMessage:
		if m.Content.Blocks == nil {
			return ceilDiv(len(m.Content.Text), charsPerToken)
		}
		return ceilDiv(estimateContentChars(m.Content.Blocks), charsPerToken)
	case ai.ToolResultMessage:
		return ceilDiv(estimateContentChars(m.Content), charsPerToken)
	case ai.AssistantMessage:
		chars := 0
		for _, block := range m.Content {
			switch b := block.(type) {
			case ai.TextContent:
				chars += len(b.Text)
			case ai.ThinkingContent:
				chars += len(b.Thinking)
			case ai.ToolCall:
				args, err := json.Marshal(b.Arguments)
				if err != nil {
					args = []byte("[unserializable]")
				}
				chars += len(b.Name) + len(args)
			}
		}
		return ceilDiv(chars, charsPerToken)
	default:
		return 0
	}
}

// EstimateContextTokens ports Pi's estimateContextTokens for a full Context:
// anchored on the most recent applicable assistant usage when available.
func EstimateContextTokens(c ai.Context) int {
	lastUsageIndex := -1
	var lastUsage ai.Usage
	latestPrefixTimestamp := int64(-1 << 62)
	for i, msg := range c.Messages {
		if m, ok := msg.(ai.AssistantMessage); ok {
			usageAppliesToPrefix := m.Timestamp >= latestPrefixTimestamp
			if usageAppliesToPrefix && m.StopReason != ai.StopAborted && m.StopReason != ai.StopError && calculateContextTokens(m.Usage) > 0 {
				lastUsage = m.Usage
				lastUsageIndex = i
			}
		}
		if ts := messageTimestamp(msg); ts > latestPrefixTimestamp {
			latestPrefixTimestamp = ts
		}
	}

	if lastUsageIndex >= 0 {
		tokens := calculateContextTokens(lastUsage)
		for i := lastUsageIndex + 1; i < len(c.Messages); i++ {
			tokens += estimateMessageTokens(c.Messages[i])
		}
		addedNames := map[string]bool{}
		for i := lastUsageIndex + 1; i < len(c.Messages); i++ {
			if m, ok := c.Messages[i].(ai.ToolResultMessage); ok {
				for _, n := range m.AddedToolNames {
					addedNames[n] = true
				}
			}
		}
		if len(addedNames) > 0 {
			var added []ai.Tool
			for _, t := range c.Tools {
				if addedNames[t.Name] {
					added = append(added, t)
				}
			}
			tokens += estimateToolsTokens(added)
		}
		return tokens
	}

	tokens := 0
	for _, msg := range c.Messages {
		tokens += estimateMessageTokens(msg)
	}
	if c.SystemPrompt != "" {
		tokens += estimateTextTokens(c.SystemPrompt)
	}
	tokens += estimateToolsTokens(c.Tools)
	return tokens
}

func estimateToolsTokens(tools []ai.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	b, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return estimateTextTokens(string(b))
}

func messageTimestamp(msg ai.Message) int64 {
	switch m := msg.(type) {
	case ai.UserMessage:
		return m.Timestamp
	case ai.AssistantMessage:
		return m.Timestamp
	case ai.ToolResultMessage:
		return m.Timestamp
	default:
		return 0
	}
}

// ClampMaxTokensToContext ports simple-options.ts.
func ClampMaxTokensToContext(model *ai.Model, c ai.Context, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return max(minMaxTokens, maxTokens)
	}
	available := model.ContextWindow - EstimateContextTokens(c) - contextSafetyTokens
	return min(maxTokens, max(minMaxTokens, available))
}

// ClampReasoning maps xhigh/max down to high for budget-based thinking.
func ClampReasoning(level ai.ThinkingLevel) ai.ThinkingLevel {
	if level == ai.ThinkingXHigh || level == ai.ThinkingMax {
		return ai.ThinkingHigh
	}
	return level
}

// AdjustMaxTokensForThinking ports simple-options.ts: fits a thinking budget
// inside the output cap. baseMaxTokens == 0 means "no explicit caller cap".
func AdjustMaxTokensForThinking(baseMaxTokens, modelMaxTokens int, level ai.ThinkingLevel, budgets *ai.ThinkingBudgets) (maxTokens, thinkingBudget int) {
	defaults := map[ai.ThinkingLevel]int{
		ai.ThinkingMinimal: 1024,
		ai.ThinkingLow:     2048,
		ai.ThinkingMedium:  8192,
		ai.ThinkingHigh:    16384,
	}
	if budgets != nil {
		if budgets.Minimal != nil {
			defaults[ai.ThinkingMinimal] = *budgets.Minimal
		}
		if budgets.Low != nil {
			defaults[ai.ThinkingLow] = *budgets.Low
		}
		if budgets.Medium != nil {
			defaults[ai.ThinkingMedium] = *budgets.Medium
		}
		if budgets.High != nil {
			defaults[ai.ThinkingHigh] = *budgets.High
		}
	}
	const minOutputTokens = 1024
	thinkingBudget = defaults[ClampReasoning(level)]
	if baseMaxTokens == 0 {
		maxTokens = modelMaxTokens
	} else {
		maxTokens = min(baseMaxTokens+thinkingBudget, modelMaxTokens)
	}
	if maxTokens <= thinkingBudget {
		thinkingBudget = max(0, maxTokens-minOutputTokens)
	}
	return maxTokens, thinkingBudget
}
