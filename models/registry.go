package models

import (
	"fmt"
	"sort"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/provider"
)

// Registry is the single place the coding layer asks for a model. It composes
// the compiled provider catalog with models.json: overrides adjust built-ins,
// extra models are appended, and unknown provider ids become custom providers.
type Registry struct {
	providers []*provider.Provider
	byID      map[string]*provider.Provider
	models    []ai.Model
	// customAPIKeys records apiKey values declared in models.json so the auth
	// layer can consult them for providers tau has no built-in flow for.
	customAPIKeys map[string]string
}

// NewRegistry composes built-in providers with a models.json config. A nil
// config yields the built-ins unchanged.
func NewRegistry(builtins []*provider.Provider, cfg *Config) (*Registry, error) {
	r := &Registry{
		byID:          map[string]*provider.Provider{},
		customAPIKeys: map[string]string{},
	}

	for _, p := range builtins {
		cp := *p
		cp.Models = append([]ai.Model{}, p.Models...)
		r.providers = append(r.providers, &cp)
		r.byID[cp.ID] = &cp
	}

	if cfg != nil {
		ids := make([]string, 0, len(cfg.Providers))
		for id := range cfg.Providers {
			ids = append(ids, id)
		}
		sort.Strings(ids) // deterministic composition order
		for _, id := range ids {
			if err := r.applyProvider(id, cfg.Providers[id]); err != nil {
				return nil, err
			}
		}
	}

	r.rebuild()
	return r, nil
}

// applyProvider layers one models.json provider entry onto the registry.
func (r *Registry) applyProvider(id string, def ProviderDef) error {
	existing, isBuiltin := r.byID[id]

	if def.APIKey != nil {
		r.customAPIKeys[id] = *def.APIKey
	}

	if !isBuiltin {
		// A provider tau has no built-in for. It needs enough to reach an
		// endpoint; without a baseUrl there is nothing to call.
		if def.BaseURL == nil || *def.BaseURL == "" {
			return fmt.Errorf("models: provider %q is not built in and has no \"baseUrl\"", id)
		}
		api := ""
		if def.Api != nil {
			api = *def.Api
		}
		name := id
		if def.Name != nil {
			name = *def.Name
		}
		p := &provider.Provider{
			ID: id, Name: name, Api: api, BaseURL: *def.BaseURL,
		}
		for _, md := range def.Models {
			p.Models = append(p.Models, buildModel(md, id, *def.BaseURL, api, def))
		}
		r.providers = append(r.providers, p)
		r.byID[id] = p
		return nil
	}

	if def.BaseURL != nil {
		existing.BaseURL = *def.BaseURL
		for i := range existing.Models {
			existing.Models[i].BaseURL = *def.BaseURL
		}
	}
	if def.Name != nil {
		existing.Name = *def.Name
	}

	// Provider-level compat and headers apply to every model it serves.
	if def.Compat != nil || len(def.Headers) > 0 {
		for i := range existing.Models {
			if def.Compat != nil {
				existing.Models[i].Compat = mergeCompat(existing.Models[i].Compat, def.Compat)
			}
			if len(def.Headers) > 0 {
				merged := map[string]string{}
				for k, v := range existing.Models[i].Headers {
					merged[k] = v
				}
				for k, v := range def.Headers {
					merged[k] = v
				}
				existing.Models[i].Headers = merged
			}
		}
	}

	for modelID, override := range def.ModelOverrides {
		found := false
		for i := range existing.Models {
			if existing.Models[i].ID == modelID {
				existing.Models[i] = applyOverride(existing.Models[i], override)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("models: provider %q has no built-in model %q to override", id, modelID)
		}
	}

	// Extra models: a matching id replaces the built-in outright.
	for _, md := range def.Models {
		base := existing.BaseURL
		if md.BaseURL != nil {
			base = *md.BaseURL
		}
		api := existing.Api
		if md.Api != nil {
			api = *md.Api
		}
		built := buildModel(md, id, base, api, def)
		replaced := false
		for i := range existing.Models {
			if existing.Models[i].ID == md.ID {
				existing.Models[i] = built
				replaced = true
				break
			}
		}
		if !replaced {
			existing.Models = append(existing.Models, built)
		}
	}
	return nil
}

// buildModel materializes a models.json model definition.
func buildModel(md ModelDef, providerID, baseURL, api string, def ProviderDef) ai.Model {
	m := ai.Model{
		ID:               md.ID,
		Name:             md.ID,
		Api:              api,
		Provider:         providerID,
		BaseURL:          baseURL,
		Input:            []string{"text"},
		ThinkingLevelMap: md.ThinkingLevelMap,
	}
	if md.Name != nil {
		m.Name = *md.Name
	}
	if md.Api != nil {
		m.Api = *md.Api
	}
	if md.BaseURL != nil {
		m.BaseURL = *md.BaseURL
	}
	if md.Reasoning != nil {
		m.Reasoning = *md.Reasoning
	}
	if len(md.Input) > 0 {
		m.Input = append([]string{}, md.Input...)
	}
	if md.Cost != nil {
		m.Cost = applyCost(ai.ModelCost{}, *md.Cost)
	}
	if md.ContextWindow != nil {
		m.ContextWindow = *md.ContextWindow
	}
	if md.MaxTokens != nil {
		m.MaxTokens = *md.MaxTokens
	}

	headers := map[string]string{}
	for k, v := range def.Headers {
		headers[k] = v
	}
	for k, v := range md.Headers {
		headers[k] = v
	}
	if len(headers) > 0 {
		m.Headers = headers
	}

	m.Compat = mergeCompat(def.Compat, md.Compat)
	return m
}

func (r *Registry) rebuild() {
	r.models = nil
	for _, p := range r.providers {
		r.models = append(r.models, p.Models...)
	}
}

// Providers returns every provider in composition order.
func (r *Registry) Providers() []*provider.Provider { return r.providers }

// Provider returns a provider by id, or nil.
func (r *Registry) Provider(id string) *provider.Provider { return r.byID[id] }

// Models returns every model across all providers.
func (r *Registry) Models() []ai.Model { return append([]ai.Model{}, r.models...) }

// CustomAPIKey returns an apiKey declared in models.json for a provider.
func (r *Registry) CustomAPIKey(providerID string) (string, bool) {
	v, ok := r.customAPIKeys[providerID]
	return v, ok
}

// Resolve looks up a model spec: "id", "provider/id", or either with a
// ":level" thinking suffix. It is strict — an unrecognized colon suffix is
// treated as part of the id rather than silently resolving a different model.
func (r *Registry) Resolve(spec string) (Match, error) {
	res := ParseSpec(spec, r.models, true)
	if res.Model == nil {
		return Match{}, fmt.Errorf("models: no model matches %q", spec)
	}
	return res, nil
}

// Scoped expands cycle-set patterns against the registry.
func (r *Registry) Scoped(patterns []string) ([]Match, []Diagnostic) {
	return Scoped(patterns, r.models)
}

// ProviderFor returns the provider serving a model.
func (r *Registry) ProviderFor(m *ai.Model) *provider.Provider {
	if m == nil {
		return nil
	}
	return r.byID[m.Provider]
}
