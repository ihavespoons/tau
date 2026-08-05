package bedrock

import (
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func thinkingFieldsOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	fields, _ := body["additionalModelRequestFields"].(map[string]any)
	return fields
}

// THE POINT: Claude 4.6 and later take an effort level; earlier Claude takes a
// token budget. Sending the wrong one is rejected, so this is the difference
// between thinking working and the turn failing.
func TestAdaptiveModelsGetAnEffortNotABudget(t *testing.T) {
	model := testModel("") // claude-sonnet-5
	body := bodyOf(t, model, userContext("think"), &Options{Reasoning: ai.ThinkingHigh})

	fields := thinkingFieldsOf(t, body)
	thinking, _ := fields["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Fatalf("thinking %v", thinking)
	}
	if _, present := thinking["budget_tokens"]; present {
		t.Errorf("a budget was sent to an adaptive model: %v", thinking)
	}
	output, _ := fields["output_config"].(map[string]any)
	if output["effort"] != "high" {
		t.Errorf("effort %v", output["effort"])
	}
	// Interleaved thinking is a budget-model beta; adaptive models reject it.
	if _, present := fields["anthropic_beta"]; present {
		t.Errorf("anthropic_beta was sent to an adaptive model: %v", fields)
	}
}

func TestBudgetModelsGetABudgetAndTheInterleavedBeta(t *testing.T) {
	model := testModel("")
	model.ID, model.Name = "anthropic.claude-3-7-sonnet-20250219-v1:0", "Claude 3.7 Sonnet"

	body := bodyOf(t, model, userContext("think"), &Options{Reasoning: ai.ThinkingMedium})

	fields := thinkingFieldsOf(t, body)
	thinking, _ := fields["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8192) {
		t.Errorf("thinking %v", thinking)
	}
	betas, _ := fields["anthropic_beta"].([]any)
	if len(betas) != 1 || betas[0] != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic_beta %v", fields["anthropic_beta"])
	}
}

// xhigh is a real level only on some models; on the rest it has to be clamped
// down rather than sent through and rejected.
func TestXhighIsClampedWhereItIsNotNative(t *testing.T) {
	native := testModel("") // sonnet-5 supports xhigh
	body := bodyOf(t, native, userContext("think"), &Options{Reasoning: ai.ThinkingXHigh})
	output, _ := thinkingFieldsOf(t, body)["output_config"].(map[string]any)
	if output["effort"] != "xhigh" {
		t.Errorf("native xhigh effort %v", output["effort"])
	}

	older := testModel("")
	older.ID, older.Name = "anthropic.claude-sonnet-4-6", "Claude Sonnet 4.6"
	body = bodyOf(t, older, userContext("think"), &Options{Reasoning: ai.ThinkingXHigh})
	output, _ = thinkingFieldsOf(t, body)["output_config"].(map[string]any)
	if output["effort"] != "high" {
		t.Errorf("clamped effort %v, want high", output["effort"])
	}
}

// A model with no reasoning must not be sent thinking fields at all.
func TestNoThinkingFieldsWithoutReasoning(t *testing.T) {
	model := testModel("")
	body := bodyOf(t, model, userContext("hi"), &Options{})
	if fields := thinkingFieldsOf(t, body); fields != nil {
		t.Errorf("thinking fields sent with reasoning off: %v", fields)
	}

	nonReasoning := testModel("")
	nonReasoning.Reasoning = false
	body = bodyOf(t, nonReasoning, userContext("hi"), &Options{Reasoning: ai.ThinkingHigh})
	if fields := thinkingFieldsOf(t, body); fields != nil {
		t.Errorf("thinking fields sent to a non-reasoning model: %v", fields)
	}
}

// Converse has no portable thinking parameter, so the fields are Claude-only.
func TestNonClaudeModelsGetNoThinkingFields(t *testing.T) {
	model := testModel("")
	model.ID, model.Name = "deepseek.r1-v1:0", "DeepSeek R1"

	body := bodyOf(t, model, userContext("think"), &Options{Reasoning: ai.ThinkingHigh})
	if fields := thinkingFieldsOf(t, body); fields != nil {
		t.Errorf("thinking fields sent to a non-Claude model: %v", fields)
	}
}

// THE POINT: GovCloud's Converse schema rejects thinking.display, so a request
// that includes it fails outright for every GovCloud user.
func TestGovCloudOmitsTheThinkingDisplay(t *testing.T) {
	model := testModel("")
	body := bodyOf(t, model, userContext("think"), &Options{
		Reasoning: ai.ThinkingHigh,
		Region:    "us-gov-west-1",
	})
	thinking, _ := thinkingFieldsOf(t, body)["thinking"].(map[string]any)
	if _, present := thinking["display"]; present {
		t.Errorf("display was sent to GovCloud: %v", thinking)
	}

	// A GovCloud model id is enough on its own, without a region.
	govModel := testModel("")
	govModel.ID = "us-gov.anthropic.claude-sonnet-5"
	body = bodyOf(t, govModel, userContext("think"), &Options{Reasoning: ai.ThinkingHigh})
	thinking, _ = thinkingFieldsOf(t, body)["thinking"].(map[string]any)
	if _, present := thinking["display"]; present {
		t.Errorf("display was sent to a GovCloud model id: %v", thinking)
	}
}

// Commercial regions default to summarized reasoning.
func TestThinkingDisplayDefaultsToSummarized(t *testing.T) {
	body := bodyOf(t, testModel(""), userContext("think"), &Options{Reasoning: ai.ThinkingHigh})
	thinking, _ := thinkingFieldsOf(t, body)["thinking"].(map[string]any)
	if thinking["display"] != "summarized" {
		t.Errorf("display %v", thinking["display"])
	}

	body = bodyOf(t, testModel(""), userContext("think"), &Options{
		Reasoning: ai.ThinkingHigh, ThinkingDisplay: ThinkingDisplayOmitted,
	})
	thinking, _ = thinkingFieldsOf(t, body)["thinking"].(map[string]any)
	if thinking["display"] != "omitted" {
		t.Errorf("display %v", thinking["display"])
	}
}

// An application inference profile's ARN names neither the vendor nor the
// model, so caching cannot be detected and has to be opt-in.
func TestCachingCanBeForcedForInferenceProfiles(t *testing.T) {
	model := testModel("")
	model.ID = "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc"
	model.Name = "Prod Profile"

	env := testEnv()
	if supportsPromptCaching(model, env) {
		t.Fatal("an opaque ARN must not be assumed cacheable")
	}
	env["AWS_BEDROCK_FORCE_CACHE"] = "1"
	if !supportsPromptCaching(model, env) {
		t.Error("AWS_BEDROCK_FORCE_CACHE=1 must enable cache points")
	}
}

// The catalog names models with dots and spaces; the predicates fold those to
// dashes so one pattern matches every spelling.
func TestModelPredicatesMatchAcrossSeparators(t *testing.T) {
	cases := []struct {
		id, name string
		adaptive bool
		caching  bool
	}{
		{"anthropic.claude-sonnet-5", "Claude Sonnet 5", true, true},
		{"eu.anthropic.claude-opus-5", "Claude Opus 5 (EU)", true, true},
		{"anthropic.claude-3-7-sonnet-20250219-v1:0", "Claude 3.7 Sonnet", false, true},
		{"anthropic.claude-3-5-haiku-20241022-v1:0", "Claude 3.5 Haiku", false, true},
		{"anthropic.claude-3-opus-20240229-v1:0", "Claude 3 Opus", false, false},
		{"amazon.nova-pro-v1:0", "Nova Pro", false, false},
		{"anthropic.claude-sonnet-4-6", "Claude Sonnet 4.6", true, true},
	}
	for _, tc := range cases {
		m := &ai.Model{ID: tc.id, Name: tc.name}
		if got := supportsAdaptiveThinking(m); got != tc.adaptive {
			t.Errorf("supportsAdaptiveThinking(%s) = %v, want %v", tc.id, got, tc.adaptive)
		}
		if got := supportsPromptCaching(m, nil); got != tc.caching {
			t.Errorf("supportsPromptCaching(%s) = %v, want %v", tc.id, got, tc.caching)
		}
	}
}

// Claude on Bedrock requires an output cap. Leaving it off is a request error,
// so the model's own maximum stands in when the caller sets none.
func TestClaudeAlwaysGetsAMaxTokens(t *testing.T) {
	body := bodyOf(t, testModel(""), userContext("hi"), &Options{})
	inference, _ := body["inferenceConfig"].(map[string]any)
	if inference["maxTokens"] != float64(8192) {
		t.Errorf("maxTokens %v, want the model cap", inference["maxTokens"])
	}
}

// Everything else on Bedrock has its own default, and forcing one over it
// truncates answers the user did not ask to truncate.
func TestNonClaudeModelsGetNoDefaultMaxTokens(t *testing.T) {
	model := testModel("")
	model.ID, model.Name = "amazon.nova-pro-v1:0", "Nova Pro"

	body := bodyOf(t, model, userContext("hi"), &Options{})
	inference, _ := body["inferenceConfig"].(map[string]any)
	if v, present := inference["maxTokens"]; present {
		t.Errorf("maxTokens %v was forced onto a non-Claude model", v)
	}
}

// StreamSimple has to leave room for an answer: a budget equal to the whole cap
// means the model thinks until it runs out and returns nothing.
func TestSimpleThinkingLeavesRoomForAnAnswer(t *testing.T) {
	model := testModel("")
	model.ID, model.Name = "anthropic.claude-3-7-sonnet-20250219-v1:0", "Claude 3.7 Sonnet"
	model.MaxTokens = 4096

	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))
	model.BaseURL = url

	_, msg := collect(t, StreamSimple(t.Context(), model, userContext("think"), &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{Env: testEnv()},
		Reasoning:     ai.ThinkingHigh,
	}))
	if msg.StopReason == ai.StopError {
		t.Fatalf("request failed: %s", msg.ErrorMessage)
	}

	inference, _ := cap.Body["inferenceConfig"].(map[string]any)
	maxTokens, _ := inference["maxTokens"].(float64)
	fields, _ := cap.Body["additionalModelRequestFields"].(map[string]any)
	thinking, _ := fields["thinking"].(map[string]any)
	budget, _ := thinking["budget_tokens"].(float64)

	if budget <= 0 {
		t.Fatalf("no thinking budget was sent: %v", thinking)
	}
	if budget > maxTokens-1024 {
		t.Errorf("budget %v leaves under 1024 tokens of the %v cap for the answer", budget, maxTokens)
	}
}

// Retention defaults to short, and the legacy variable Pi reads still works.
func TestCacheRetentionDefaults(t *testing.T) {
	if got := resolveCacheRetention("", nil); got != ai.CacheShort {
		t.Errorf("default retention %q", got)
	}
	if got := resolveCacheRetention(ai.CacheNone, nil); got != ai.CacheNone {
		t.Errorf("explicit none was overridden: %q", got)
	}
	if got := resolveCacheRetention("", map[string]string{"PI_CACHE_RETENTION": "long"}); got != ai.CacheLong {
		t.Errorf("PI_CACHE_RETENTION=long gave %q", got)
	}
	if got := resolveCacheRetention("", map[string]string{"TAU_CACHE_RETENTION": "long"}); got != ai.CacheLong {
		t.Errorf("TAU_CACHE_RETENTION=long gave %q", got)
	}
}
