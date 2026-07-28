package ai

// ModelCostRates are $/million-token rates.
type ModelCostRates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// ModelCostTier applies its rates when total input tokens exceed the threshold.
type ModelCostTier struct {
	ModelCostRates
	InputTokensAbove int `json:"inputTokensAbove"`
}

// ModelCost is base rates plus optional request-wide pricing tiers.
type ModelCost struct {
	ModelCostRates
	Tiers []ModelCostTier `json:"tiers,omitempty"`
}

// Model describes one model on one provider.
type Model struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Api       Api        `json:"api"`
	Provider  ProviderId `json:"provider"`
	BaseURL   string     `json:"baseUrl"`
	Reasoning bool       `json:"reasoning"`
	// ThinkingLevelMap maps pi thinking levels to provider/model-specific
	// values. Missing keys use provider defaults; a nil value marks a level
	// unsupported.
	ThinkingLevelMap ThinkingLevelMap  `json:"thinkingLevelMap,omitempty"`
	Input            []string          `json:"input"` // "text", "image"
	Cost             ModelCost         `json:"cost"`
	ContextWindow    int               `json:"contextWindow"`
	MaxTokens        int               `json:"maxTokens"`
	Headers          map[string]string `json:"headers,omitempty"`
	// Compat overrides API quirk auto-detection. Nil fields auto-detect from
	// BaseURL inside each wire-API package.
	Compat *CompatFlags `json:"compat,omitempty"`
}

// SupportsImageInput reports whether the model accepts image content.
func (m *Model) SupportsImageInput() bool {
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// CalculateCost computes and stores usage.Cost from the model's rates,
// picking the highest matching input-token tier for the whole request.
// Anthropic 1h cache writes are billed at 2x base input (Pi parity).
func CalculateCost(m *Model, usage *Usage) Cost {
	inputTokens := usage.Input + usage.CacheRead + usage.CacheWrite
	rates := m.Cost.ModelCostRates
	matched := -1
	for _, tier := range m.Cost.Tiers {
		if inputTokens > tier.InputTokensAbove && tier.InputTokensAbove > matched {
			rates = tier.ModelCostRates
			matched = tier.InputTokensAbove
		}
	}
	longWrite := 0
	if usage.CacheWrite1h != nil {
		longWrite = *usage.CacheWrite1h
	}
	shortWrite := usage.CacheWrite - longWrite
	usage.Cost.Input = rates.Input / 1e6 * float64(usage.Input)
	usage.Cost.Output = rates.Output / 1e6 * float64(usage.Output)
	usage.Cost.CacheRead = rates.CacheRead / 1e6 * float64(usage.CacheRead)
	usage.Cost.CacheWrite = (rates.CacheWrite*float64(shortWrite) + rates.Input*2*float64(longWrite)) / 1e6
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	return usage.Cost
}

var extendedThinkingLevels = []ModelThinkingLevel{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// SupportedThinkingLevels returns the thinking levels the model supports,
// mirroring Pi's getSupportedThinkingLevels: non-reasoning models support only
// "off"; xhigh/max require an explicit map entry; a null map entry disables a
// level.
func SupportedThinkingLevels(m *Model) []ModelThinkingLevel {
	if !m.Reasoning {
		return []ModelThinkingLevel{"off"}
	}
	var out []ModelThinkingLevel
	for _, level := range extendedThinkingLevels {
		mapped, present := m.ThinkingLevelMap[level]
		if present && mapped == nil {
			continue
		}
		if (level == "xhigh" || level == "max") && !present {
			continue
		}
		out = append(out, level)
	}
	return out
}

// ClampThinkingLevel returns the nearest supported level: the level itself,
// else the next higher supported level, else the next lower, else the first
// supported (or "off").
func ClampThinkingLevel(m *Model, level ModelThinkingLevel) ModelThinkingLevel {
	available := SupportedThinkingLevels(m)
	has := func(l ModelThinkingLevel) bool {
		for _, a := range available {
			if a == l {
				return true
			}
		}
		return false
	}
	if has(level) {
		return level
	}
	idx := -1
	for i, l := range extendedThinkingLevels {
		if l == level {
			idx = i
			break
		}
	}
	if idx == -1 {
		if len(available) > 0 {
			return available[0]
		}
		return "off"
	}
	for i := idx; i < len(extendedThinkingLevels); i++ {
		if has(extendedThinkingLevels[i]) {
			return extendedThinkingLevels[i]
		}
	}
	for i := idx - 1; i >= 0; i-- {
		if has(extendedThinkingLevels[i]) {
			return extendedThinkingLevels[i]
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return "off"
}

// ModelsEqual compares models by id and provider.
func ModelsEqual(a, b *Model) bool {
	if a == nil || b == nil {
		return false
	}
	return a.ID == b.ID && a.Provider == b.Provider
}
