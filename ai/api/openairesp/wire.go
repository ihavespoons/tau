package openairesp

import "encoding/json"

// streamEvent is one SSE event from the responses endpoint.
//
// Every event names an output_index, which is the slot it belongs to. That
// index — not an ordering assumption — is what ties a delta to the item it is
// part of, because the wire interleaves items freely.
type streamEvent struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Delta       string          `json:"delta"`
	Arguments   string          `json:"arguments"`
	Input       string          `json:"input"`
	Item        json.RawMessage `json:"item"`
	Response    *responseBody   `json:"response"`

	// Code and Message carry a mid-stream error event.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// outputItem is an item as the wire describes it. Only the fields tau reads
// are named; the raw bytes are kept separately for verbatim replay.
type outputItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Phase  string `json:"phase"`

	// Reasoning.
	Summary          []summaryPart `json:"summary"`
	Content          []summaryPart `json:"content"`
	EncryptedContent string        `json:"encrypted_content"`

	// Message.
	MessageContent []outputContent `json:"-"`

	// Function call.
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type outputContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// decodeOutputItem reads an item and keeps its raw bytes.
//
// A message and a reasoning item both have a `content` field of different
// shape, so the message form is decoded separately rather than forcing both
// into one struct and losing whichever loses the tag.
func decodeOutputItem(raw json.RawMessage) (outputItem, error) {
	var it outputItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return it, err
	}
	if it.Type == "message" {
		var msg struct {
			Content []outputContent `json:"content"`
		}
		if err := json.Unmarshal(raw, &msg); err == nil {
			it.MessageContent = msg.Content
		}
	}
	return it, nil
}

// responseBody is the terminal response object.
type responseBody struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	ServiceTier string            `json:"service_tier"`
	Usage       *usagePayload     `json:"usage"`
	Output      []json.RawMessage `json:"output"`
	Error       *responseError    `json:"error"`
	Incomplete  *incompleteInfo   `json:"incomplete_details"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type incompleteInfo struct {
	Reason string `json:"reason"`
}

type usagePayload struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	InputTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`

	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}
