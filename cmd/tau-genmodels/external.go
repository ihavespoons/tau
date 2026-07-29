package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/ihavespoons/tau/ai"
)

// Two providers do not appear in models.dev in any usable form: they are
// aggregators whose catalogs change faster than any third party tracks, so
// each publishes its own /models endpoint. Those responses are vendored
// alongside the models.dev snapshot and read here.

const (
	openRouterBaseURL     = "https://openrouter.ai/api/v1"
	vercelGatewayBaseURL  = "https://ai-gateway.vercel.sh"
	openRouterProviderID  = "openrouter"
	vercelGatewayProvider = "vercel-ai-gateway"
)

// openRouterCatalog is OpenRouter's /models response.
type openRouterCatalog struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	ContextLength       int      `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`

	Architecture struct {
		Modality string `json:"modality"`
	} `json:"architecture"`

	// Prices are strings in dollars per token, which is why they are parsed
	// rather than decoded: a float64 would lose the distinction between "0"
	// and absent, and several are given in scientific notation.
	Pricing struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheRead  string `json:"input_cache_read"`
		InputCacheWrite string `json:"input_cache_write"`
	} `json:"pricing"`

	TopProvider struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

func (m openRouterModel) supports(param string) bool {
	for _, p := range m.SupportedParameters {
		if p == param {
			return true
		}
	}
	return false
}

// perMillion converts a dollars-per-token string to dollars per million.
func perMillion(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return roundCost(v * 1e6)
}

func buildOpenRouter(dataDir string) ([]ai.Model, error) {
	var cat openRouterCatalog
	if err := readJSON(filepath.Join(dataDir, "openrouter.json"), &cat); err != nil {
		return nil, err
	}

	var out []ai.Model
	for _, m := range cat.Data {
		if !m.supports("tools") {
			continue
		}

		input := []string{"text"}
		if containsAny(m.Architecture.Modality, "image") {
			input = append(input, "image")
		}

		// The top provider's window is the one a request actually gets; the
		// model-level figure is the best any backend offers.
		contextWindow := m.TopProvider.ContextLength
		if contextWindow == 0 {
			contextWindow = m.ContextLength
		}
		if contextWindow == 0 {
			contextWindow = 4096
		}
		maxTokens := m.TopProvider.MaxCompletionTokens
		if maxTokens == 0 {
			maxTokens = 4096
		}

		out = append(out, ai.Model{
			ID: m.ID, Name: m.Name,
			Api: ai.ApiOpenAICompletions, Provider: openRouterProviderID,
			BaseURL:   openRouterBaseURL,
			Reasoning: m.supports("reasoning"),
			Input:     input,
			Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
				Input:      perMillion(m.Pricing.Prompt),
				Output:     perMillion(m.Pricing.Completion),
				CacheRead:  perMillion(m.Pricing.InputCacheRead),
				CacheWrite: perMillion(m.Pricing.InputCacheWrite),
			}},
			ContextWindow: contextWindow, MaxTokens: maxTokens,
		})
		tweakOpenRouter(&out[len(out)-1])
	}

	out = append(out, openRouterExtraModels...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// vercelGatewayCatalog is the gateway's /models response.
type vercelGatewayCatalog struct {
	Data []vercelGatewayModel `json:"data"`
}

type vercelGatewayModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Tags          []string `json:"tags"`
	ContextWindow int      `json:"context_window"`
	MaxTokens     int      `json:"max_tokens"`

	// Prices may arrive as a JSON string or a number depending on the model,
	// so they are decoded loosely and coerced.
	Pricing struct {
		Input           json.RawMessage `json:"input"`
		Output          json.RawMessage `json:"output"`
		InputCacheRead  json.RawMessage `json:"input_cache_read"`
		InputCacheWrite json.RawMessage `json:"input_cache_write"`
	} `json:"pricing"`
}

func (m vercelGatewayModel) tagged(tag string) bool {
	for _, t := range m.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// looseNumber coerces a value that may be a JSON number or a JSON string.
// Anything unparseable is zero rather than an error: a missing price should
// not stop the whole catalog from generating.
func looseNumber(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func gatewayPerMillion(raw json.RawMessage) float64 { return roundCost(looseNumber(raw) * 1e6) }

func buildVercelGateway(dataDir string) ([]ai.Model, error) {
	var cat vercelGatewayCatalog
	if err := readJSON(filepath.Join(dataDir, "vercelgateway.json"), &cat); err != nil {
		return nil, err
	}

	var out []ai.Model
	for _, m := range cat.Data {
		if !m.tagged("tool-use") {
			continue
		}

		input := []string{"text"}
		if m.tagged("vision") {
			input = append(input, "image")
		}

		name := m.Name
		if name == "" {
			name = m.ID
		}
		contextWindow := m.ContextWindow
		if contextWindow == 0 {
			contextWindow = 4096
		}
		maxTokens := m.MaxTokens
		if maxTokens == 0 {
			maxTokens = 4096
		}

		// The gateway speaks Anthropic's messages wire for every model it
		// serves, whoever built the model.
		out = append(out, ai.Model{
			ID: m.ID, Name: name,
			Api: ai.ApiAnthropicMessages, Provider: vercelGatewayProvider,
			BaseURL:   vercelGatewayBaseURL,
			Reasoning: m.tagged("reasoning"),
			Input:     input,
			Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
				Input:      gatewayPerMillion(m.Pricing.Input),
				Output:     gatewayPerMillion(m.Pricing.Output),
				CacheRead:  gatewayPerMillion(m.Pricing.InputCacheRead),
				CacheWrite: gatewayPerMillion(m.Pricing.InputCacheWrite),
			}},
			ContextWindow: contextWindow, MaxTokens: maxTokens,
		})
		tweakVercelGateway(&out[len(out)-1])
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func readJSON(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}
