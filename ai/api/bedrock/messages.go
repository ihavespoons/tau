package bedrock

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// emptyTextPlaceholder stands in for content Bedrock would reject. The Converse
// API refuses empty text blocks and empty content arrays outright, so a turn
// that legitimately produced nothing still has to say something.
const emptyTextPlaceholder = "<empty>"

// maxToolCallIDLength is Bedrock's limit on a toolUseId.
const maxToolCallIDLength = 64

// normalizeToolCallID makes a foreign tool-call id acceptable to Bedrock, which
// takes only word characters and dashes.
func normalizeToolCallID(id string, _ ai.AssistantMessage) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > maxToolCallIDLength {
		out = out[:maxToolCallIDLength]
	}
	return out
}

// nonBlankText returns a text block, or nil when the text is only whitespace.
func nonBlankText(text string) *types.ContentBlockMemberText {
	sanitized := apishared.SanitizeSurrogates(text)
	if apishared.TrimSpace(sanitized) == "" {
		return nil
	}
	return &types.ContentBlockMemberText{Value: sanitized}
}

func requiredText(text string) types.ContentBlock {
	if block := nonBlankText(text); block != nil {
		return block
	}
	return &types.ContentBlockMemberText{Value: emptyTextPlaceholder}
}

// imageBlock converts a base64 image to the raw bytes Bedrock wants.
func imageBlock(mimeType, data string) (*types.ContentBlockMemberImage, error) {
	var format types.ImageFormat
	switch mimeType {
	case "image/jpeg", "image/jpg":
		format = types.ImageFormatJpeg
	case "image/png":
		format = types.ImageFormatPng
	case "image/gif":
		format = types.ImageFormatGif
	case "image/webp":
		format = types.ImageFormatWebp
	default:
		return nil, fmt.Errorf("unknown image type: %s", mimeType)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("decoding %s image: %w", mimeType, err)
	}
	return &types.ContentBlockMemberImage{
		Value: types.ImageBlock{Format: format, Source: &types.ImageSourceMemberBytes{Value: raw}},
	}, nil
}

// toolResultContent converts a tool result's blocks, never returning an empty
// list.
func toolResultContent(content ai.ContentList) ([]types.ToolResultContentBlock, error) {
	var out []types.ToolResultContentBlock
	for _, block := range content {
		switch b := block.(type) {
		case ai.ImageContent:
			img, err := imageBlock(b.MimeType, b.Data)
			if err != nil {
				return nil, err
			}
			out = append(out, &types.ToolResultContentBlockMemberImage{Value: img.Value})
		case ai.TextContent:
			if text := nonBlankText(b.Text); text != nil {
				out = append(out, &types.ToolResultContentBlockMemberText{Value: text.Value})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, &types.ToolResultContentBlockMemberText{Value: emptyTextPlaceholder})
	}
	return out, nil
}

// cachePointBlock builds a cache breakpoint for the requested retention.
func cachePointBlock(retention ai.CacheRetention) types.CachePointBlock {
	block := types.CachePointBlock{Type: types.CachePointTypeDefault}
	if retention == ai.CacheLong {
		block.Ttl = types.CacheTTLOneHour
	}
	return block
}

// buildSystemPrompt returns the system blocks, with a cache breakpoint after
// the prompt when the model can cache.
func buildSystemPrompt(prompt string, model *ai.Model, retention ai.CacheRetention, env map[string]string) []types.SystemContentBlock {
	if prompt == "" {
		return nil
	}
	blocks := []types.SystemContentBlock{
		&types.SystemContentBlockMemberText{Value: apishared.SanitizeSurrogates(prompt)},
	}
	if retention != ai.CacheNone && supportsPromptCaching(model, env) {
		blocks = append(blocks, &types.SystemContentBlockMemberCachePoint{Value: cachePointBlock(retention)})
	}
	return blocks
}

// convertMessages maps the transcript onto Converse messages.
func convertMessages(c ai.Context, model *ai.Model, retention ai.CacheRetention, env map[string]string) ([]types.Message, error) {
	transformed := apishared.TransformMessages(c.Messages, model, normalizeToolCallID)
	var result []types.Message

	for i := 0; i < len(transformed); i++ {
		switch m := transformed[i].(type) {
		case ai.UserMessage:
			content, err := convertUserContent(m)
			if err != nil {
				return nil, err
			}
			result = append(result, types.Message{Role: types.ConversationRoleUser, Content: content})

		case ai.AssistantMessage:
			content, err := convertAssistantContent(m, model)
			if err != nil {
				return nil, err
			}
			// Bedrock rejects a message with no content blocks. An aborted turn
			// can legitimately produce one, so it is dropped rather than sent.
			if len(content) == 0 {
				continue
			}
			result = append(result, types.Message{Role: types.ConversationRoleAssistant, Content: content})

		case ai.ToolResultMessage:
			// Bedrock requires every result from one round in a single user
			// message, so consecutive results are collected here rather than
			// each becoming a turn of its own.
			var blocks []types.ContentBlock
			j := i
			for ; j < len(transformed); j++ {
				tr, ok := transformed[j].(ai.ToolResultMessage)
				if !ok {
					break
				}
				content, err := toolResultContent(tr.Content)
				if err != nil {
					return nil, err
				}
				status := types.ToolResultStatusSuccess
				if tr.IsError {
					status = types.ToolResultStatusError
				}
				blocks = append(blocks, &types.ContentBlockMemberToolResult{
					Value: types.ToolResultBlock{
						ToolUseId: aws.String(tr.ToolCallID),
						Content:   content,
						Status:    status,
					},
				})
			}
			i = j - 1
			result = append(result, types.Message{Role: types.ConversationRoleUser, Content: blocks})
		}
	}

	// The trailing cache breakpoint covers the whole conversation prefix, so it
	// belongs on the last message and only when that message is a user turn.
	if retention != ai.CacheNone && supportsPromptCaching(model, env) && len(result) > 0 {
		last := &result[len(result)-1]
		if last.Role == types.ConversationRoleUser {
			last.Content = append(last.Content,
				&types.ContentBlockMemberCachePoint{Value: cachePointBlock(retention)})
		}
	}

	return result, nil
}

func convertUserContent(m ai.UserMessage) ([]types.ContentBlock, error) {
	if m.Content.Blocks == nil {
		return []types.ContentBlock{requiredText(m.Content.Text)}, nil
	}
	var content []types.ContentBlock
	for _, block := range m.Content.Blocks {
		switch b := block.(type) {
		case ai.TextContent:
			if text := nonBlankText(b.Text); text != nil {
				content = append(content, text)
			}
		case ai.ImageContent:
			img, err := imageBlock(b.MimeType, b.Data)
			if err != nil {
				return nil, err
			}
			content = append(content, img)
		}
	}
	if len(content) == 0 {
		content = append(content, &types.ContentBlockMemberText{Value: emptyTextPlaceholder})
	}
	return content, nil
}

func convertAssistantContent(m ai.AssistantMessage, model *ai.Model) ([]types.ContentBlock, error) {
	var content []types.ContentBlock
	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.TextContent:
			if text := nonBlankText(b.Text); text != nil {
				content = append(content, text)
			}

		case ai.ToolCall:
			args := b.Arguments
			if args == nil {
				args = map[string]any{}
			}
			content = append(content, &types.ContentBlockMemberToolUse{
				Value: types.ToolUseBlock{
					ToolUseId: aws.String(b.ID),
					Name:      aws.String(b.Name),
					Input:     document.NewLazyDocument(args),
				},
			})

		case ai.ThinkingContent:
			thinking := apishared.SanitizeSurrogates(b.Thinking)
			if apishared.TrimSpace(thinking) == "" {
				continue
			}
			if !supportsThinkingSignature(model) {
				// Every non-Claude model on Bedrock rejects the signature field
				// outright, so the reasoning is replayed without one.
				content = append(content, &types.ContentBlockMemberReasoningContent{
					Value: &types.ReasoningContentBlockMemberReasoningText{
						Value: types.ReasoningTextBlock{Text: aws.String(thinking)},
					},
				})
				continue
			}
			if apishared.TrimSpace(b.ThinkingSignature) == "" {
				// Signatures arrive after the thinking deltas. A turn that was
				// interrupted, or one restored from a session file written by
				// another tool, can carry thinking with no signature — and
				// Bedrock rejects that. Replaying it as plain text keeps the
				// reasoning in the transcript instead of losing the turn.
				content = append(content, &types.ContentBlockMemberText{Value: thinking})
				continue
			}
			content = append(content, &types.ContentBlockMemberReasoningContent{
				Value: &types.ReasoningContentBlockMemberReasoningText{
					Value: types.ReasoningTextBlock{
						Text:      aws.String(thinking),
						Signature: aws.String(b.ThinkingSignature),
					},
				},
			})
		}
	}
	return content, nil
}

// convertToolConfig maps tau tools onto a Converse tool configuration.
func convertToolConfig(tools []ai.Tool, choice ToolChoice, supportsStrictMode bool) (*types.ToolConfiguration, error) {
	if len(tools) == 0 || choice.Type == ToolChoiceNone {
		return nil, nil
	}

	converted := make([]types.Tool, 0, len(tools))
	for _, t := range tools {
		strict, err := apishared.ResolveJSONSchemaStrictSampling(t, supportsStrictMode)
		if err != nil {
			return nil, err
		}
		spec := types.ToolSpecification{
			Name:        aws.String(t.Name),
			Description: aws.String(t.Description),
			InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(t.Parameters)},
		}
		if strict {
			spec.Strict = aws.Bool(true)
		}
		converted = append(converted, &types.ToolMemberToolSpec{Value: spec})
	}

	cfg := &types.ToolConfiguration{Tools: converted}
	switch choice.Type {
	case ToolChoiceAuto:
		cfg.ToolChoice = &types.ToolChoiceMemberAuto{}
	case ToolChoiceAny:
		cfg.ToolChoice = &types.ToolChoiceMemberAny{}
	case ToolChoiceTool:
		cfg.ToolChoice = &types.ToolChoiceMemberTool{
			Value: types.SpecificToolChoice{Name: aws.String(choice.Name)},
		}
	}
	return cfg, nil
}
