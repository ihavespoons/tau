package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/ihavespoons/tau/ai"
)

// modelsDevCatalog is the shape of models.dev's api.json: provider id →
// provider, each with its own model map.
type modelsDevCatalog map[string]modelsDevProvider

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Env    []string                  `json:"env"`
	Doc    string                    `json:"doc"`
	Models map[string]modelsDevModel `json:"models"`
}

// modelsDevModel is one model as models.dev describes it. Fields tau does not
// consume are omitted; the ones here are the ones that reach a catalog entry.
type modelsDevModel struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Family           string              `json:"family"`
	Attachment       bool                `json:"attachment"`
	Reasoning        bool                `json:"reasoning"`
	ReasoningOptions []reasoningOption   `json:"reasoning_options"`
	ToolCall         bool                `json:"tool_call"`
	StructuredOutput bool                `json:"structured_output"`
	Temperature      bool                `json:"temperature"`
	OpenWeights      bool                `json:"open_weights"`
	ReleaseDate      string              `json:"release_date"`
	Modalities       modelsDevModalities `json:"modalities"`
	Limit            modelsDevLimit      `json:"limit"`
	Cost             modelsDevCost       `json:"cost"`
}

type modelsDevModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type modelsDevLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// modelsDevCost holds pointers because absent and zero are different facts: a
// provider that publishes no cache rate is not a provider whose cache is free,
// and at least one correction (Mistral's) turns on exactly that distinction.
type modelsDevCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

func costOrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// rates is the model's pricing as the catalog records it.
func (m modelsDevModel) rates() ai.ModelCostRates {
	return ai.ModelCostRates{
		Input:      costOrZero(m.Cost.Input),
		Output:     costOrZero(m.Cost.Output),
		CacheRead:  costOrZero(m.Cost.CacheRead),
		CacheWrite: costOrZero(m.Cost.CacheWrite),
	}
}

// roundCost trims the floating-point noise a derived rate picks up, so
// 0.04 * 0.1 reads as 0.004 rather than 0.004000000000000001.
func roundCost(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// reasoningOption describes how a model exposes its thinking control.
//
// Values may be JSON null, which is why Values is a slice of pointers: a null
// means "a level models.dev knows of but cannot name", and it has to survive
// decoding distinctly from the empty string.
type reasoningOption struct {
	Type   string    `json:"type"` // "toggle" | "effort" | "budget_tokens"
	Values []*string `json:"values,omitempty"`
	Min    *int      `json:"min,omitempty"`
	Max    *int      `json:"max,omitempty"`
}

// supportsImage reports whether the model takes image input, which is the only
// modality distinction tau's catalog records.
func (m modelsDevModel) supportsImage() bool {
	for _, in := range m.Modalities.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// inputModalities is the catalog's input list, which is either text alone or
// text and image.
func (m modelsDevModel) inputModalities() []string {
	if m.supportsImage() {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

// contextWindow and maxTokens fall back to 4096, matching Pi: a model with no
// declared limit is assumed small rather than unbounded, so a missing value
// cannot cause an over-long request.
func (m modelsDevModel) contextWindow() int {
	if m.Limit.Context > 0 {
		return m.Limit.Context
	}
	return 4096
}

func (m modelsDevModel) maxTokens() int {
	if m.Limit.Output > 0 {
		return m.Limit.Output
	}
	return 4096
}

func (m modelsDevModel) displayName(id string) string {
	if m.Name != "" {
		return m.Name
	}
	return id
}

// loadModelsDev reads the vendored snapshot.
func loadModelsDev(path string) (modelsDevCatalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cat modelsDevCatalog
	if err := json.Unmarshal(b, &cat); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cat, nil
}
