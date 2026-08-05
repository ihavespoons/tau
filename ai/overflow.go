package ai

import "regexp"

// Context-overflow detection. Port of Pi's utils/overflow.ts.
//
// There is no standard for "your request was too long". Every provider says it
// differently, several do not say it at all, and two report success while
// silently truncating. Detecting it anyway is what lets tau compact and retry
// instead of handing the user a failed turn — so the patterns are a catalogue
// of observed error text, kept as one list rather than scattered per wire.

// overflowPatterns match the error text providers return when the input does
// not fit. Each is annotated with the provider whose message it was written
// against; a message quoted in the comment is one that was actually seen.
var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt is too long`),                                                                        // Anthropic: "prompt is too long: 213462 tokens > 200000 maximum"
	regexp.MustCompile(`(?i)request_too_large`),                                                                         // Anthropic, HTTP 413 on request byte size
	regexp.MustCompile(`(?i)input is too long for requested model`),                                                     // Amazon Bedrock
	regexp.MustCompile(`(?i)exceeds the context window`),                                                                // OpenAI, both wires
	regexp.MustCompile(`(?i)exceeds (?:the )?(?:model'?s )?maximum context length(?: of [\d,]+ tokens?|\s*\([\d,]+\))`), // LiteLLM and other OpenAI-compatible proxies
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),                                                    // Google Gemini
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),                                                              // xAI
	regexp.MustCompile(`(?i)reduce the length of the messages`),                                                         // Groq
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),                                                      // OpenRouter, most backends
	regexp.MustCompile(`(?i)exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`),                           // OpenRouter/Poolside
	regexp.MustCompile(`(?i)input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`),            // Together AI
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),                                                                  // GitHub Copilot
	regexp.MustCompile(`(?i)exceeds the available context size`),                                                        // llama.cpp
	regexp.MustCompile(`(?i)greater than the context length`),                                                           // LM Studio
	regexp.MustCompile(`(?i)context window exceeds limit`),                                                              // MiniMax
	regexp.MustCompile(`(?i)exceeded model token limit`),                                                                // Kimi For Coding
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`),                                       // Mistral
	regexp.MustCompile(`(?i)prompt has [\d,]+ tokens?, but the configured context size is [\d,]+ tokens?`),              // DS4
	regexp.MustCompile(`(?i)model_context_window_exceeded`),                                                             // z.ai, when it does report
	regexp.MustCompile(`(?i)prompt too long; exceeded (?:max )?context length`),                                         // Ollama
	regexp.MustCompile(`(?i)range of input length should be`),                                                           // DashScope / Qwen
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),                                                             // generic
	regexp.MustCompile(`(?i)too many tokens`),                                                                           // generic
	regexp.MustCompile(`(?i)token limit exceeded`),                                                                      // generic
	regexp.MustCompile(`(?i)^4(?:00|13)\s*(?:status code)?\s*\(no body\)`),                                              // Cerebras returns no body at all
}

// nonOverflowPatterns veto a match. Bedrock renders throttling as "Too many
// tokens, please wait before trying again", which the generic pattern above
// would read as overflow — and compacting in response to a rate limit destroys
// history to fix a problem that would have cleared on its own.
var nonOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(Throttling error|Service unavailable):`),
	regexp.MustCompile(`(?i)rate limit`),
	regexp.MustCompile(`(?i)too many requests`),
}

// IsContextOverflow reports whether a response means the request did not fit.
//
// contextWindow may be zero when it is unknown, which disables the two
// inferred cases below and leaves only the error text.
func IsContextOverflow(m *AssistantMessage, contextWindow int) bool {
	if m == nil {
		return false
	}

	if m.StopReason == StopError && m.ErrorMessage != "" {
		for _, p := range nonOverflowPatterns {
			if p.MatchString(m.ErrorMessage) {
				return false
			}
		}
		for _, p := range overflowPatterns {
			if p.MatchString(m.ErrorMessage) {
				return true
			}
		}
	}

	// Silent overflow: the request succeeded, but the provider counted more
	// input than the model can hold — it truncated and did not say so.
	if contextWindow > 0 && m.StopReason == StopStop {
		if m.Usage.Input+m.Usage.CacheRead > contextWindow {
			return true
		}
	}

	// Truncate-then-stop: the input was cut down to exactly fill the window,
	// leaving no room to generate. A length stop with no output at all is not
	// a long answer, it is no answer.
	if contextWindow > 0 && m.StopReason == StopLength && m.Usage.Output == 0 {
		if m.Usage.Input+m.Usage.CacheRead >= contextWindow*99/100 {
			return true
		}
	}

	return false
}
