package models

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/provider"
)

func model(providerID, id, name string) ai.Model {
	return ai.Model{
		ID: id, Name: name, Provider: providerID, Api: "anthropic-messages",
		BaseURL: "https://api.example.com", Input: []string{"text"},
		Cost:          ai.ModelCost{ModelCostRates: ai.ModelCostRates{Input: 1, Output: 2}},
		ContextWindow: 1000, MaxTokens: 100,
	}
}

func catalog() []ai.Model {
	return []ai.Model{
		model("anthropic", "claude-opus-5", "Claude Opus 5"),
		model("anthropic", "claude-sonnet-5", "Claude Sonnet 5"),
		model("anthropic", "claude-sonnet-4-5-20250929", "Claude Sonnet 4.5"),
		model("openai", "gpt-5.5", "GPT 5.5"),
		model("openrouter", "moonshotai/kimi-k2:free", "Kimi K2 Free"),
		model("openrouter", "claude-sonnet-5", "Sonnet via OpenRouter"),
	}
}

func TestFindExact(t *testing.T) {
	c := catalog()
	cases := []struct {
		name     string
		ref      string
		wantID   string
		wantProv string
		wantNil  bool
	}{
		{name: "canonical provider/id", ref: "anthropic/claude-opus-5", wantID: "claude-opus-5", wantProv: "anthropic"},
		{name: "case insensitive", ref: "ANTHROPIC/Claude-Opus-5", wantID: "claude-opus-5", wantProv: "anthropic"},
		{name: "unique bare id", ref: "claude-opus-5", wantID: "claude-opus-5", wantProv: "anthropic"},
		{name: "id with slash", ref: "openrouter/moonshotai/kimi-k2:free", wantID: "moonshotai/kimi-k2:free", wantProv: "openrouter"},
		{name: "ambiguous bare id resolves to nothing", ref: "claude-sonnet-5", wantNil: true},
		{name: "disambiguated by provider", ref: "openrouter/claude-sonnet-5", wantID: "claude-sonnet-5", wantProv: "openrouter"},
		{name: "unknown", ref: "nope", wantNil: true},
		{name: "empty", ref: "", wantNil: true},
		{name: "whitespace trimmed", ref: "  anthropic/claude-opus-5  ", wantID: "claude-opus-5", wantProv: "anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FindExact(tc.ref, c)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %s/%s, want no match", got.Provider, got.ID)
				}
				return
			}
			if got == nil {
				t.Fatal("no match")
			}
			if got.ID != tc.wantID || got.Provider != tc.wantProv {
				t.Errorf("got %s/%s, want %s/%s", got.Provider, got.ID, tc.wantProv, tc.wantID)
			}
		})
	}
}

func TestParseSpecThinkingSuffix(t *testing.T) {
	c := catalog()
	cases := []struct {
		name      string
		spec      string
		strict    bool
		wantID    string
		wantLevel string
		wantNil   bool
		wantWarn  bool
	}{
		{name: "no suffix", spec: "anthropic/claude-opus-5", wantID: "claude-opus-5"},
		{name: "high", spec: "anthropic/claude-opus-5:high", wantID: "claude-opus-5", wantLevel: "high"},
		{name: "off", spec: "claude-opus-5:off", wantID: "claude-opus-5", wantLevel: "off"},
		{name: "max", spec: "gpt-5.5:max", wantID: "gpt-5.5", wantLevel: "max"},
		{
			// The whole spec matches a real model id containing a colon, so
			// no splitting happens — this is why full-match comes first.
			name: "colon in model id is not a thinking suffix",
			spec: "openrouter/moonshotai/kimi-k2:free", wantID: "moonshotai/kimi-k2:free",
		},
		{
			name:   "colon id plus a real thinking suffix",
			spec:   "openrouter/moonshotai/kimi-k2:free:high",
			wantID: "moonshotai/kimi-k2:free", wantLevel: "high",
		},
		{
			name: "unknown suffix lenient warns and keeps the model",
			spec: "anthropic/claude-opus-5:bogus", wantID: "claude-opus-5", wantWarn: true,
		},
		{
			name: "unknown suffix strict refuses",
			spec: "anthropic/claude-opus-5:bogus", strict: true, wantNil: true,
		},
		{name: "unknown model", spec: "nope:high", wantNil: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSpec(tc.spec, c, tc.strict)
			if tc.wantNil {
				if got.Model != nil {
					t.Fatalf("got %s, want no match", got.Model.ID)
				}
				return
			}
			if got.Model == nil {
				t.Fatal("no match")
			}
			if got.Model.ID != tc.wantID {
				t.Errorf("id = %q, want %q", got.Model.ID, tc.wantID)
			}
			if got.ThinkingLevel != tc.wantLevel {
				t.Errorf("level = %q, want %q", got.ThinkingLevel, tc.wantLevel)
			}
			if (got.Warning != "") != tc.wantWarn {
				t.Errorf("warning = %q, wantWarn = %v", got.Warning, tc.wantWarn)
			}
		})
	}
}

func TestSubstringMatchPrefersAliasOverDatedSnapshot(t *testing.T) {
	c := []ai.Model{
		model("anthropic", "claude-sonnet-4-5-20250929", "Sonnet dated"),
		model("anthropic", "claude-sonnet-4-5", "Sonnet alias"),
	}
	got := ParseSpec("sonnet", c, false)
	if got.Model == nil || got.Model.ID != "claude-sonnet-4-5" {
		t.Errorf("got %v, want the alias", got.Model)
	}
}

func TestSubstringMatchFallsBackToLatestDated(t *testing.T) {
	c := []ai.Model{
		model("anthropic", "claude-x-20240101", "old"),
		model("anthropic", "claude-x-20250101", "new"),
	}
	got := ParseSpec("claude-x", c, false)
	if got.Model == nil || got.Model.ID != "claude-x-20250101" {
		t.Errorf("got %v, want the newest dated snapshot", got.Model)
	}
}

func TestIsAlias(t *testing.T) {
	cases := map[string]bool{
		"claude-sonnet-4-5":          true,
		"claude-sonnet-4-5-20250929": false,
		"gpt-4-latest":               true,
		"model-20250101":             false,
		"v1-1234567":                 true, // 7 digits is not a date
	}
	for id, want := range cases {
		if got := isAlias(id); got != want {
			t.Errorf("isAlias(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestScopedGlobbing(t *testing.T) {
	c := catalog()
	cases := []struct {
		name      string
		patterns  []string
		wantIDs   []string
		wantLevel string
		wantDiags int
	}{
		{
			name: "provider wildcard", patterns: []string{"anthropic/*"},
			wantIDs: []string{"claude-opus-5", "claude-sonnet-5", "claude-sonnet-4-5-20250929"},
		},
		{
			name: "bare wildcard spans providers", patterns: []string{"*sonnet*"},
			wantIDs: []string{"claude-sonnet-5", "claude-sonnet-4-5-20250929", "claude-sonnet-5"},
		},
		{
			name: "glob with thinking level", patterns: []string{"anthropic/*:high"},
			wantIDs:   []string{"claude-opus-5", "claude-sonnet-5", "claude-sonnet-4-5-20250929"},
			wantLevel: "high",
		},
		{
			name: "exact plus glob dedupes", patterns: []string{"anthropic/claude-opus-5", "anthropic/*"},
			wantIDs: []string{"claude-opus-5", "claude-sonnet-5", "claude-sonnet-4-5-20250929"},
		},
		{
			name: "no match warns", patterns: []string{"nonexistent/*"},
			wantIDs: nil, wantDiags: 1,
		},
		{
			name: "question mark", patterns: []string{"openai/gpt-5.?"},
			wantIDs: []string{"gpt-5.5"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := Scoped(tc.patterns, c)
			if len(diags) != tc.wantDiags {
				t.Errorf("diagnostics = %v, want %d", diags, tc.wantDiags)
			}
			if len(got) != len(tc.wantIDs) {
				var ids []string
				for _, m := range got {
					ids = append(ids, m.Model.ID)
				}
				t.Fatalf("matched %v, want %v", ids, tc.wantIDs)
			}
			for i, m := range got {
				if m.Model.ID != tc.wantIDs[i] {
					t.Errorf("model[%d] = %q, want %q", i, m.Model.ID, tc.wantIDs[i])
				}
				if tc.wantLevel != "" && m.ThinkingLevel != tc.wantLevel {
					t.Errorf("model[%d] level = %q, want %q", i, m.ThinkingLevel, tc.wantLevel)
				}
			}
		})
	}
}

func builtinProvider() *provider.Provider {
	return &provider.Provider{
		ID: "anthropic", Name: "Anthropic", Api: "anthropic-messages",
		BaseURL: "https://api.anthropic.com",
		Models: []ai.Model{
			model("anthropic", "claude-opus-5", "Claude Opus 5"),
			model("anthropic", "claude-sonnet-5", "Claude Sonnet 5"),
		},
	}
}

func TestRegistryWithoutConfig(t *testing.T) {
	r, err := NewRegistry([]*provider.Provider{builtinProvider()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Models()) != 2 {
		t.Errorf("models = %d", len(r.Models()))
	}
	got, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.ID != "claude-opus-5" {
		t.Errorf("resolved %q", got.Model.ID)
	}
}

func TestRegistryModelOverrideAdjustsBuiltin(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderDef{
		"anthropic": {
			ModelOverrides: map[string]ModelOverride{
				"claude-opus-5": {
					Cost:          &CostDef{Input: f64(99), Output: f64(198)},
					ContextWindow: intp(500000),
					Compat:        &ai.CompatFlags{SupportsTemperature: boolp(false)},
				},
			},
		},
	}}
	r, err := NewRegistry([]*provider.Provider{builtinProvider()}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve("claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.Cost.Input != 99 || got.Model.Cost.Output != 198 {
		t.Errorf("cost = %+v", got.Model.Cost)
	}
	if got.Model.ContextWindow != 500000 {
		t.Errorf("contextWindow = %d", got.Model.ContextWindow)
	}
	if got.Model.Compat == nil || got.Model.Compat.SupportsTemperature == nil || *got.Model.Compat.SupportsTemperature {
		t.Error("compat override not applied")
	}
	// An untouched field keeps its built-in value.
	if got.Model.MaxTokens != 100 {
		t.Errorf("maxTokens = %d, should be untouched", got.Model.MaxTokens)
	}
	// A sibling model is unaffected.
	sonnet, _ := r.Resolve("claude-sonnet-5")
	if sonnet.Model.Cost.Input != 1 {
		t.Errorf("sibling cost changed: %+v", sonnet.Model.Cost)
	}
}

func TestOverrideMergesRatherThanReplacingMaps(t *testing.T) {
	base := builtinProvider()
	base.Models[0].ThinkingLevelMap = ai.ThinkingLevelMap{
		"low":  strp("low"),
		"high": strp("high"),
	}
	base.Models[0].Headers = map[string]string{"x-base": "1", "shared": "base"}

	cfg := &Config{Providers: map[string]ProviderDef{
		"anthropic": {ModelOverrides: map[string]ModelOverride{
			"claude-opus-5": {
				ThinkingLevelMap: ai.ThinkingLevelMap{"high": strp("HIGH"), "max": strp("max")},
				Headers:          map[string]string{"shared": "override", "x-new": "2"},
			},
		}},
	}}
	r, err := NewRegistry([]*provider.Provider{base}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Resolve("claude-opus-5")

	tm := got.Model.ThinkingLevelMap
	if v := tm["low"]; v == nil || *v != "low" {
		t.Error("thinkingLevelMap should merge, keeping the base 'low' entry")
	}
	if v := tm["high"]; v == nil || *v != "HIGH" {
		t.Error("override should win for 'high'")
	}
	if v := tm["max"]; v == nil || *v != "max" {
		t.Error("override should add 'max'")
	}

	h := got.Model.Headers
	if h["x-base"] != "1" || h["shared"] != "override" || h["x-new"] != "2" {
		t.Errorf("headers = %v, want a merge with the override winning", h)
	}
}

func TestRegistryAddsExtraModelToBuiltinProvider(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderDef{
		"anthropic": {Models: []ModelDef{{
			ID: "claude-experimental", ContextWindow: intp(42), Reasoning: boolp(true),
		}}},
	}}
	r, err := NewRegistry([]*provider.Provider{builtinProvider()}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve("claude-experimental")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.ContextWindow != 42 || !got.Model.Reasoning {
		t.Errorf("model = %+v", got.Model)
	}
	// It inherits the provider's endpoint and api.
	if got.Model.BaseURL != "https://api.anthropic.com" || got.Model.Api != "anthropic-messages" {
		t.Errorf("baseURL/api not inherited: %s %s", got.Model.BaseURL, got.Model.Api)
	}
}

func TestRegistryCustomProvider(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderDef{
		"local": {
			BaseURL: strp("http://localhost:8080/v1"),
			Api:     strp("openai-completions"),
			APIKey:  strp("$LOCAL_KEY"),
			Models:  []ModelDef{{ID: "my-model", ContextWindow: intp(8192)}},
		},
	}}
	r, err := NewRegistry([]*provider.Provider{builtinProvider()}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Provider("local")
	if p == nil {
		t.Fatal("custom provider not registered")
	}
	got, err := r.Resolve("local/my-model")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.BaseURL != "http://localhost:8080/v1" || got.Model.Api != "openai-completions" {
		t.Errorf("model = %+v", got.Model)
	}
	if key, ok := r.CustomAPIKey("local"); !ok || key != "$LOCAL_KEY" {
		t.Errorf("apiKey = %q %v", key, ok)
	}
}

func TestRegistryCustomProviderNeedsBaseURL(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderDef{
		"broken": {Models: []ModelDef{{ID: "m"}}},
	}}
	if _, err := NewRegistry([]*provider.Provider{builtinProvider()}, cfg); err == nil {
		t.Error("expected an error for a custom provider with no baseUrl")
	}
}

func TestRegistryOverrideOfUnknownModelErrors(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderDef{
		"anthropic": {ModelOverrides: map[string]ModelOverride{"ghost": {}}},
	}}
	if _, err := NewRegistry([]*provider.Provider{builtinProvider()}, cfg); err == nil {
		t.Error("expected an error overriding a model that does not exist")
	}
}

func TestRegistryProviderBaseURLRewritesModels(t *testing.T) {
	cfg := &Config{Providers: map[string]ProviderDef{
		"anthropic": {BaseURL: strp("http://proxy.internal/v1")},
	}}
	r, err := NewRegistry([]*provider.Provider{builtinProvider()}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r.Models() {
		if m.BaseURL != "http://proxy.internal/v1" {
			t.Errorf("%s baseURL = %q", m.ID, m.BaseURL)
		}
	}
}

func TestResolveUnknownModelErrors(t *testing.T) {
	r, _ := NewRegistry([]*provider.Provider{builtinProvider()}, nil)
	if _, err := r.Resolve("does-not-exist"); err == nil {
		t.Error("expected an error")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is empty", func(t *testing.T) {
		cfg, err := LoadConfig(filepath.Join(dir, "nope.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Providers) != 0 {
			t.Errorf("providers = %v", cfg.Providers)
		}
	})

	t.Run("comments are stripped", func(t *testing.T) {
		path := filepath.Join(dir, "commented.json")
		body := `{
  // a line comment
  "providers": {
    "local": {
      /* block comment */
      "baseUrl": "http://x/v1",
      "models": [{"id": "m"}]
    }
  }
}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Providers) != 1 {
			t.Fatalf("providers = %v", cfg.Providers)
		}
	})

	t.Run("url in a string is not treated as a comment", func(t *testing.T) {
		path := filepath.Join(dir, "url.json")
		body := `{"providers":{"p":{"baseUrl":"https://example.com/v1","models":[{"id":"m"}]}}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := *cfg.Providers["p"].BaseURL; got != "https://example.com/v1" {
			t.Errorf("baseUrl = %q — the // inside the URL was eaten", got)
		}
	})

	t.Run("malformed json errors with the path", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte(`{"providers":`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("model without id errors", func(t *testing.T) {
		path := filepath.Join(dir, "noid.json")
		if err := os.WriteFile(path, []byte(`{"providers":{"p":{"baseUrl":"x","models":[{"name":"m"}]}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Error("expected an error for a model with no id")
		}
	})
}

// A realistic models.json from Pi's docs must load and compose.
func TestRealisticModelsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	body := `{
  "providers": {
    "anthropic": {
      "modelOverrides": {
        "claude-opus-5": {
          "contextWindow": 200000,
          "cost": { "input": 5, "output": 25, "cacheRead": 0.5, "cacheWrite": 6.25 }
        }
      }
    },
    "my-proxy": {
      "name": "Corporate Proxy",
      "baseUrl": "https://llm.corp.internal/v1",
      "api": "openai-completions",
      "apiKey": "$CORP_TOKEN",
      "headers": { "x-team": "platform" },
      "compat": { "supportsStore": false, "maxTokensField": "max_tokens" },
      "models": [
        {
          "id": "corp-gpt",
          "name": "Corp GPT",
          "reasoning": true,
          "thinkingLevelMap": { "low": "low", "high": "high", "max": null },
          "input": ["text", "image"],
          "cost": { "input": 1, "output": 3, "cacheRead": 0.1, "cacheWrite": 1.25 },
          "contextWindow": 128000,
          "maxTokens": 8192
        }
      ]
    }
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry([]*provider.Provider{builtinProvider()}, cfg)
	if err != nil {
		t.Fatal(err)
	}

	opus, err := r.Resolve("anthropic/claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	if opus.Model.ContextWindow != 200000 || opus.Model.Cost.CacheWrite != 6.25 {
		t.Errorf("override not applied: %+v", opus.Model)
	}

	corp, err := r.Resolve("my-proxy/corp-gpt")
	if err != nil {
		t.Fatal(err)
	}
	if !corp.Model.Reasoning || corp.Model.ContextWindow != 128000 {
		t.Errorf("corp model = %+v", corp.Model)
	}
	if !corp.Model.SupportsImageInput() {
		t.Error("corp model should accept images")
	}
	if v, ok := corp.Model.ThinkingLevelMap["max"]; !ok || v != nil {
		t.Error("a null thinkingLevelMap entry should mark the level unsupported")
	}
	if corp.Model.Headers["x-team"] != "platform" {
		t.Errorf("provider headers not inherited: %v", corp.Model.Headers)
	}
	if corp.Model.Compat == nil || corp.Model.Compat.MaxTokensField == nil ||
		*corp.Model.Compat.MaxTokensField != "max_tokens" {
		t.Error("provider compat not inherited")
	}
	// SupportedThinkingLevels honors the null entry.
	levels := ai.SupportedThinkingLevels(corp.Model)
	for _, l := range levels {
		if l == "max" {
			t.Error("'max' should be unsupported: its map entry is null")
		}
	}
}

func f64(v float64) *float64 { return &v }
func intp(v int) *int        { return &v }
func boolp(v bool) *bool     { return &v }
func strp(v string) *string  { return &v }
