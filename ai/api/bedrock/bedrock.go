// Package bedrock implements the bedrock-converse-stream wire API — the tau
// port of Pi's packages/ai/src/api/bedrock-converse-stream.ts (snapshot
// v0.82.1).
//
// Unlike every other wire in tau this one does not speak HTTP directly. Bedrock
// signs with SigV4, frames responses as AWS EventStream binary messages, and
// resolves credentials through shared config files, SSO caches and instance
// metadata. That is delegated to aws-sdk-go-v2; what stays here is the mapping
// between tau's message model and Converse, which is where the behaviour that
// matters lives.
package bedrock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithy "github.com/aws/smithy-go"
	awshttp "github.com/aws/smithy-go/transport/http"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// ToolChoiceType selects how the model may use tools.
type ToolChoiceType string

const (
	ToolChoiceAuto ToolChoiceType = "auto"
	ToolChoiceAny  ToolChoiceType = "any"
	ToolChoiceNone ToolChoiceType = "none"
	ToolChoiceTool ToolChoiceType = "tool"
)

// ToolChoice is a tool-choice setting; Name applies only to ToolChoiceTool.
type ToolChoice struct {
	Type ToolChoiceType
	Name string
}

// ThinkingDisplay controls how Claude's reasoning comes back.
type ThinkingDisplay string

const (
	// ThinkingDisplaySummarized returns summarized reasoning text.
	ThinkingDisplaySummarized ThinkingDisplay = "summarized"
	// ThinkingDisplayOmitted redacts the reasoning but still returns the
	// signature, so multi-turn continuity survives without paying for the text.
	ThinkingDisplayOmitted ThinkingDisplay = "omitted"
)

// Options are the bedrock-converse-stream provider options.
type Options struct {
	ai.StreamOptions

	// Region overrides the AWS region. Empty falls through to the environment
	// and then the SDK's own resolution.
	Region string
	// Profile selects a shared-config profile.
	Profile string
	// BearerToken authenticates with a Bedrock API key instead of SigV4.
	BearerToken string

	// ToolChoice constrains tool use.
	ToolChoice ToolChoice
	// Reasoning is the requested thinking level; empty means off.
	Reasoning ai.ThinkingLevel
	// ThinkingBudgets overrides the per-level token budgets on budget-based
	// Claude models.
	ThinkingBudgets *ai.ThinkingBudgets
	// InterleavedThinking enables thinking between tool calls on budget-based
	// Claude models. Nil means enabled.
	InterleavedThinking *bool
	// ThinkingDisplay controls whether reasoning text is returned. Empty means
	// summarized.
	ThinkingDisplay ThinkingDisplay

	// RequestMetadata is attached to the inference request for AWS cost
	// allocation tagging.
	RequestMetadata map[string]string
}

// Stream runs one turn against Bedrock.
//
// Like every wire API in tau it never returns an error: failures arrive as a
// terminal error event carrying an AssistantMessage whose StopReason says what
// went wrong.
func Stream(ctx context.Context, model *ai.Model, c ai.Context, opts *Options) *ai.MessageStream {
	if opts == nil {
		opts = &Options{}
	}
	stream := ai.NewMessageStream()
	go run(ctx, stream, model, c, opts)
	return stream
}

// StreamSimple is Stream with normalized cross-provider options.
func StreamSimple(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	base := &Options{StreamOptions: opts.StreamOptions}

	if opts.Reasoning == "" {
		return Stream(ctx, model, c, base)
	}

	base.Reasoning = opts.Reasoning
	base.ThinkingBudgets = opts.ThinkingBudgets

	// Only budget-based Claude has to have a thinking allowance carved out of
	// the output cap. Effort-based models take a level, and everything else on
	// Bedrock ignores the fields entirely.
	if !isAnthropicClaudeModel(model) || supportsAdaptiveThinking(model) {
		return Stream(ctx, model, c, base)
	}

	maxTokens, thinkingBudget := apishared.AdjustMaxTokensForThinking(
		opts.MaxTokens, model.MaxTokens, opts.Reasoning, opts.ThinkingBudgets)
	maxTokens = apishared.ClampMaxTokensToContext(model, c, maxTokens)
	base.MaxTokens = maxTokens

	// The budget must leave room for an actual answer, or the model spends the
	// whole cap thinking and returns nothing.
	budgets := ai.ThinkingBudgets{}
	if opts.ThinkingBudgets != nil {
		budgets = *opts.ThinkingBudgets
	}
	capped := min(thinkingBudget, max(0, maxTokens-1024))
	setBudget(&budgets, apishared.ClampReasoning(opts.Reasoning), capped)
	base.ThinkingBudgets = &budgets

	return Stream(ctx, model, c, base)
}

func setBudget(budgets *ai.ThinkingBudgets, level ai.ThinkingLevel, value int) {
	switch level {
	case ai.ThinkingMinimal:
		budgets.Minimal = &value
	case ai.ThinkingLow:
		budgets.Low = &value
	case ai.ThinkingMedium:
		budgets.Medium = &value
	case ai.ThinkingHigh:
		budgets.High = &value
	}
}

func newOutput(model *ai.Model) *ai.AssistantMessage {
	return &ai.AssistantMessage{
		Content:    ai.ContentList{},
		Api:        model.Api,
		Provider:   model.Provider,
		Model:      model.ID,
		StopReason: ai.StopStop,
		Timestamp:  time.Now().UnixMilli(),
	}
}

func run(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *Options) {
	out := newOutput(model)

	defer func() {
		if r := recover(); r != nil {
			fail(ctx, stream, out, fmt.Errorf("bedrock: %v", r))
		}
	}()

	input, err := buildInput(model, c, opts)
	if err != nil {
		fail(ctx, stream, out, err)
		return
	}

	if opts.OnPayload != nil {
		replaced, err := opts.OnPayload(input, model)
		if err != nil {
			fail(ctx, stream, out, err)
			return
		}
		if replaced != nil {
			next, ok := replaced.(*bedrockruntime.ConverseStreamInput)
			if !ok {
				fail(ctx, stream, out, fmt.Errorf(
					"bedrock: onPayload returned %T, want *bedrockruntime.ConverseStreamInput", replaced))
				return
			}
			input = next
		}
	}

	client, err := buildClient(ctx, model, opts)
	if err != nil {
		fail(ctx, stream, out, err)
		return
	}

	resp, err := client.ConverseStream(ctx, input)
	if err != nil {
		fail(ctx, stream, out, err)
		return
	}
	events := resp.GetStream()
	defer func() { _ = events.Close() }()

	if opts.OnResponse != nil {
		if err := opts.OnResponse(responseMetadata(resp), model); err != nil {
			fail(ctx, stream, out, err)
			return
		}
	}

	if err := consume(ctx, events, stream, out, model); err != nil {
		fail(ctx, stream, out, err)
		return
	}

	stream.Push(ai.Event{Type: ai.EventDone, Reason: out.StopReason, Message: out})
}

// responseMetadata reports what the SDK exposes of the HTTP response.
//
// A streaming call that gets this far has already succeeded, and the SDK keeps
// no status code on the output — so the status is reported as 200 and the AWS
// request id, which is what a support ticket actually needs, is surfaced as the
// header the service itself would have sent.
func responseMetadata(resp *bedrockruntime.ConverseStreamOutput) ai.ProviderResponse {
	out := ai.ProviderResponse{Status: 200}
	if resp == nil {
		return out
	}
	if id, ok := awsmiddleware.GetRequestIDMetadata(resp.ResultMetadata); ok && id != "" {
		out.Headers = http.Header{"X-Amzn-Requestid": {id}}
	}
	return out
}

func buildInput(model *ai.Model, c ai.Context, opts *Options) (*bedrockruntime.ConverseStreamInput, error) {
	retention := resolveCacheRetention(opts.CacheRetention, opts.Env)

	messages, err := convertMessages(c, model, retention, opts.Env)
	if err != nil {
		return nil, err
	}

	strictMode := model.Compat != nil && model.Compat.SupportsStrictMode != nil && *model.Compat.SupportsStrictMode
	toolConfig, err := convertToolConfig(c.Tools, opts.ToolChoice, strictMode)
	if err != nil {
		return nil, err
	}

	input := &bedrockruntime.ConverseStreamInput{
		ModelId:    aws.String(model.ID),
		Messages:   messages,
		System:     buildSystemPrompt(c.SystemPrompt, model, retention, opts.Env),
		ToolConfig: toolConfig,
	}

	inference := &types.InferenceConfiguration{}
	// Claude on Bedrock requires an output cap; the rest of the catalog treats
	// it as optional and has its own defaults.
	maxTokens := opts.MaxTokens
	if maxTokens == 0 && isAnthropicClaudeModel(model) {
		maxTokens = model.MaxTokens
	}
	if maxTokens > 0 {
		inference.MaxTokens = aws.Int32(int32(maxTokens)) //nolint:gosec // catalog caps are far below int32
	}
	if opts.Temperature != nil {
		t := float32(*opts.Temperature)
		inference.Temperature = &t
	}
	input.InferenceConfig = inference

	if fields := buildAdditionalModelRequestFields(model, opts); fields != nil {
		input.AdditionalModelRequestFields = document.NewLazyDocument(fields)
	}
	if len(opts.RequestMetadata) > 0 {
		input.RequestMetadata = opts.RequestMetadata
	}

	return input, nil
}

func fail(ctx context.Context, stream *ai.MessageStream, out *ai.AssistantMessage, err error) {
	reason := ai.StopError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		reason = ai.StopAborted
	}
	out.StopReason = reason
	stream.Push(ai.Event{Type: ai.EventError, Reason: reason, Error: ai.ErrorMessage(out, reason, formatError(err))})
}

// errorPrefixes give each Bedrock exception a stable human-readable prefix.
//
// The prefixes are load-bearing, not decoration: the retry and
// context-overflow logic above this layer matches on phrases like "server
// error" and "service unavailable", so using the raw SDK exception names would
// silently stop those requests from being retried.
var errorPrefixes = map[string]string{
	"InternalServerException":     "Internal server error",
	"ModelStreamErrorException":   "Model stream error",
	"ValidationException":         "Validation error",
	"ThrottlingException":         "Throttling error",
	"ServiceUnavailableException": "Service unavailable",
}

// dataRetentionDocsURL explains the error some accounts hit when their
// configured retention mode is not offered for a model.
const dataRetentionDocsURL = "https://docs.aws.amazon.com/bedrock/latest/userguide/data-retention.html"

// formatError turns an SDK error into the message a user sees.
func formatError(err error) string {
	if err == nil {
		return ""
	}
	core := err.Error()

	// A gateway or proxy in front of Bedrock can answer with a bare HTTP error
	// the SDK cannot map to a modelled exception. Without the status and body
	// the whole failure collapses to something like "UnknownError", which says
	// nothing about the 403 that actually happened.
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() > 0 {
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorMessage() == "" {
			core = fmt.Sprintf("%d: %s", respErr.HTTPStatusCode(), core)
		}
	}

	hint := ""
	if strings.Contains(strings.ToLower(core), "data retention mode") {
		hint = " See " + dataRetentionDocsURL + " for supported data retention modes."
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		prefix, ok := errorPrefixes[apiErr.ErrorCode()]
		if !ok {
			prefix = apiErr.ErrorCode()
		}
		if prefix != "" {
			return prefix + ": " + core + hint
		}
	}
	return core + hint
}
