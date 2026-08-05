package ai

import "testing"

func errored(message string) *AssistantMessage {
	return &AssistantMessage{StopReason: StopError, ErrorMessage: message}
}

// Every one of these is a real message from a real provider. The point of the
// catalogue is that a turn rejected for length becomes a compaction rather than
// a failure, and that only works for providers whose wording is recognized.
func TestEachProvidersOverflowMessageIsRecognized(t *testing.T) {
	messages := map[string]string{
		"anthropic":      "prompt is too long: 213462 tokens > 200000 maximum",
		"anthropic 413":  `413 {"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`,
		"bedrock":        "Input is too long for requested model.",
		"openai":         "Your input exceeds the context window of this model",
		"litellm":        "Requested token count exceeds the model's maximum context length of 131072 tokens",
		"openai compat":  "Input length (265330) exceeds model's maximum context length (262144).",
		"gemini":         "The input token count (1196265) exceeds the maximum number of tokens allowed (1048575)",
		"xai":            "This model's maximum prompt length is 131072 but the request contains 537812 tokens",
		"groq":           "Please reduce the length of the messages or completion",
		"openrouter":     "This endpoint's maximum context length is 65536 tokens. However, you requested about 90000 tokens",
		"poolside":       "Input length 90000 exceeds the maximum allowed input length of 65536 tokens.",
		"together":       "The input (90000 tokens) is longer than the model's context length (65536 tokens).",
		"llama.cpp":      "the request exceeds the available context size, try increasing it",
		"lm studio":      "tokens to keep from the initial prompt is greater than the context length",
		"copilot":        "prompt token count of 90000 exceeds the limit of 65536",
		"minimax":        "invalid params, context window exceeds limit",
		"kimi":           "Your request exceeded model token limit: 65536 (requested: 90000)",
		"ds4":            "Prompt has 90000 tokens, but the configured context size is 65536 tokens",
		"cerebras":       "400 status code (no body)",
		"mistral":        "Prompt contains 90000 tokens ... too large for model with 65536 maximum context length",
		"ollama":         "prompt too long; exceeded max context length by 400 tokens",
		"qwen":           "Range of input length should be [1, 65536]",
		"generic":        "context_length_exceeded",
		"generic tokens": "too many tokens",
	}
	for provider, message := range messages {
		if !IsContextOverflow(errored(message), 0) {
			t.Errorf("%s: %q was not recognized as overflow", provider, message)
		}
	}
}

// Compacting in response to a rate limit destroys history to fix a problem
// that would have cleared on its own — and Bedrock's throttling message
// contains the words the generic pattern looks for.
func TestThrottlingIsNotMistakenForOverflow(t *testing.T) {
	messages := []string{
		"Throttling error: Too many tokens, please wait before trying again.",
		"Service unavailable: too many tokens",
		"Rate limit exceeded, please retry",
		"429 too many requests",
	}
	for _, message := range messages {
		if IsContextOverflow(errored(message), 0) {
			t.Errorf("%q was treated as overflow", message)
		}
	}
}

func TestAnOrdinaryFailureIsNotOverflow(t *testing.T) {
	for _, message := range []string{"invalid api key", "connection reset by peer", "500 internal server error"} {
		if IsContextOverflow(errored(message), 200000) {
			t.Errorf("%q was treated as overflow", message)
		}
	}
}

// Some providers accept an oversized request, quietly drop what did not fit,
// and report success. The reported input is the only evidence.
func TestASuccessfulTurnOverTheWindowIsOverflow(t *testing.T) {
	m := &AssistantMessage{StopReason: StopStop, Usage: Usage{Input: 150000, CacheRead: 60000}}
	if !IsContextOverflow(m, 200000) {
		t.Error("210k of input into a 200k window is overflow, however it was reported")
	}
	if IsContextOverflow(m, 0) {
		t.Error("with no known window there is nothing to compare against")
	}
}

func TestASuccessfulTurnInsideTheWindowIsNotOverflow(t *testing.T) {
	m := &AssistantMessage{StopReason: StopStop, Usage: Usage{Input: 100, CacheRead: 50}}
	if IsContextOverflow(m, 200000) {
		t.Error("a small turn is not overflow")
	}
}

// A length stop that produced nothing is not a long answer, it is no answer:
// the server truncated the input to exactly fill the window.
func TestALengthStopWithNoOutputIsOverflow(t *testing.T) {
	m := &AssistantMessage{StopReason: StopLength, Usage: Usage{Input: 199000, Output: 0}}
	if !IsContextOverflow(m, 200000) {
		t.Error("a full window and zero output means the input was truncated")
	}

	// A genuinely long answer stops for length too, and must not be confused
	// with it — compacting would throw away history to fix nothing.
	produced := &AssistantMessage{StopReason: StopLength, Usage: Usage{Input: 199000, Output: 4096}}
	if IsContextOverflow(produced, 200000) {
		t.Error("a length stop that produced output is just a long answer")
	}
}

func TestNoMessageIsNotOverflow(t *testing.T) {
	if IsContextOverflow(nil, 200000) {
		t.Error("nil is not overflow")
	}
}
