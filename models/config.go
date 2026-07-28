// Package models is tau's runtime model registry: the compiled provider
// catalog layered with user-supplied models.json (custom providers, extra
// models, and overrides of built-ins), plus Pi's model-reference grammar.
package models

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ihavespoons/tau/ai"
)

// ModelDef defines a model in models.json. Only ID is required; everything
// else falls back to the provider or the built-in it shadows.
type ModelDef struct {
	ID               string              `json:"id"`
	Name             *string             `json:"name,omitempty"`
	Api              *string             `json:"api,omitempty"`
	BaseURL          *string             `json:"baseUrl,omitempty"`
	Reasoning        *bool               `json:"reasoning,omitempty"`
	ThinkingLevelMap ai.ThinkingLevelMap `json:"thinkingLevelMap,omitempty"`
	Input            []string            `json:"input,omitempty"`
	Cost             *CostDef            `json:"cost,omitempty"`
	ContextWindow    *int                `json:"contextWindow,omitempty"`
	MaxTokens        *int                `json:"maxTokens,omitempty"`
	Headers          map[string]string   `json:"headers,omitempty"`
	Compat           *ai.CompatFlags     `json:"compat,omitempty"`
}

// CostDef is a partial cost specification; absent rates keep the base value.
type CostDef struct {
	Input      *float64           `json:"input,omitempty"`
	Output     *float64           `json:"output,omitempty"`
	CacheRead  *float64           `json:"cacheRead,omitempty"`
	CacheWrite *float64           `json:"cacheWrite,omitempty"`
	Tiers      []ai.ModelCostTier `json:"tiers,omitempty"`
}

// ModelOverride adjusts a built-in model. It cannot change identity (id, api,
// baseUrl) — for that, define a new model.
type ModelOverride struct {
	Name             *string             `json:"name,omitempty"`
	Reasoning        *bool               `json:"reasoning,omitempty"`
	ThinkingLevelMap ai.ThinkingLevelMap `json:"thinkingLevelMap,omitempty"`
	Input            []string            `json:"input,omitempty"`
	Cost             *CostDef            `json:"cost,omitempty"`
	ContextWindow    *int                `json:"contextWindow,omitempty"`
	MaxTokens        *int                `json:"maxTokens,omitempty"`
	Headers          map[string]string   `json:"headers,omitempty"`
	Compat           *ai.CompatFlags     `json:"compat,omitempty"`
}

// ProviderDef is one entry under models.json's "providers" map.
type ProviderDef struct {
	Name           *string                  `json:"name,omitempty"`
	BaseURL        *string                  `json:"baseUrl,omitempty"`
	APIKey         *string                  `json:"apiKey,omitempty"`
	Api            *string                  `json:"api,omitempty"`
	OAuth          *string                  `json:"oauth,omitempty"`
	Headers        map[string]string        `json:"headers,omitempty"`
	Compat         *ai.CompatFlags          `json:"compat,omitempty"`
	AuthHeader     *bool                    `json:"authHeader,omitempty"`
	Models         []ModelDef               `json:"models,omitempty"`
	ModelOverrides map[string]ModelOverride `json:"modelOverrides,omitempty"`
}

// Config is one immutable load of models.json.
type Config struct {
	Providers map[string]ProviderDef `json:"providers"`
}

// LoadConfig reads models.json. A missing file yields an empty config; a
// malformed one is an error naming the file, since silently ignoring a
// broken config would hide the user's mistake.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		return &Config{Providers: map[string]ProviderDef{}}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Providers: map[string]ProviderDef{}}, nil
		}
		return nil, fmt.Errorf("models: read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return &Config{Providers: map[string]ProviderDef{}}, nil
	}

	var cfg Config
	if err := json.Unmarshal(stripJSONComments(b), &cfg); err != nil {
		return nil, fmt.Errorf("models: parse %s: %w", path, err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderDef{}
	}
	for id, p := range cfg.Providers {
		for i, m := range p.Models {
			if strings.TrimSpace(m.ID) == "" {
				return nil, fmt.Errorf("models: %s: providers.%s.models[%d] is missing \"id\"", path, id, i)
			}
		}
	}
	return &cfg, nil
}

// stripJSONComments removes // and /* */ comments outside string literals,
// matching Pi's stripJsonComments so hand-edited configs stay readable.
func stripJSONComments(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString, inLine, inBlock, escaped := false, false, false, false

	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out = append(out, c)
			}
		case inBlock:
			if c == '*' && i+1 < len(b) && b[i+1] == '/' {
				inBlock = false
				i++
			}
		case inString:
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			inBlock = true
			i++
		default:
			out = append(out, c)
		}
	}
	return out
}

// applyOverride layers an override onto a built-in model, field by field.
//
// Ported from provider-composer.ts:100-121: each scalar uses the override when
// present; thinkingLevelMap and headers MERGE into the base rather than
// replacing it; cost merges per-rate with tiers replaced wholesale.
func applyOverride(base ai.Model, o ModelOverride) ai.Model {
	out := base
	if o.Name != nil {
		out.Name = *o.Name
	}
	if o.Reasoning != nil {
		out.Reasoning = *o.Reasoning
	}
	if len(o.ThinkingLevelMap) > 0 {
		merged := ai.ThinkingLevelMap{}
		for k, v := range base.ThinkingLevelMap {
			merged[k] = v
		}
		for k, v := range o.ThinkingLevelMap {
			merged[k] = v
		}
		out.ThinkingLevelMap = merged
	}
	if len(o.Input) > 0 {
		out.Input = append([]string{}, o.Input...)
	}
	if o.Cost != nil {
		out.Cost = applyCost(base.Cost, *o.Cost)
	}
	if o.ContextWindow != nil {
		out.ContextWindow = *o.ContextWindow
	}
	if o.MaxTokens != nil {
		out.MaxTokens = *o.MaxTokens
	}
	if len(o.Headers) > 0 {
		merged := map[string]string{}
		for k, v := range base.Headers {
			merged[k] = v
		}
		for k, v := range o.Headers {
			merged[k] = v
		}
		out.Headers = merged
	}
	if o.Compat != nil {
		out.Compat = mergeCompat(base.Compat, o.Compat)
	}
	return out
}

func applyCost(base ai.ModelCost, c CostDef) ai.ModelCost {
	out := base
	if c.Input != nil {
		out.Input = *c.Input
	}
	if c.Output != nil {
		out.Output = *c.Output
	}
	if c.CacheRead != nil {
		out.CacheRead = *c.CacheRead
	}
	if c.CacheWrite != nil {
		out.CacheWrite = *c.CacheWrite
	}
	if c.Tiers != nil {
		out.Tiers = append([]ai.ModelCostTier{}, c.Tiers...)
	}
	return out
}

// mergeCompat overlays non-nil override fields onto the base flags. Pi spreads
// one level (provider-composer.ts:80-98); since CompatFlags is a flat struct of
// pointers, a nil field means "not specified" and keeps the base value.
func mergeCompat(base, override *ai.CompatFlags) *ai.CompatFlags {
	if override == nil {
		return base
	}
	if base == nil {
		cp := *override
		return &cp
	}

	// Round-trip through JSON so every pointer field is handled without
	// enumerating all ~30 of them — a hand-written merge would silently miss
	// fields added later.
	baseRaw, err := json.Marshal(base)
	if err != nil {
		cp := *override
		return &cp
	}
	overrideRaw, err := json.Marshal(override)
	if err != nil {
		return base
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(baseRaw, &merged); err != nil {
		return base
	}
	var ov map[string]json.RawMessage
	if err := json.Unmarshal(overrideRaw, &ov); err != nil {
		return base
	}
	for k, v := range ov {
		merged[k] = v
	}
	out := &ai.CompatFlags{}
	remarshaled, err := json.Marshal(merged)
	if err != nil {
		return base
	}
	if err := json.Unmarshal(remarshaled, out); err != nil {
		return base
	}
	return out
}
