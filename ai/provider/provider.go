// Package provider assembles wire APIs and model catalogs into named
// providers — the tau analogue of pi-ai's providers/ + models.ts collection.
package provider

import (
	"fmt"

	"github.com/ihavespoons/tau/ai"
)

// Provider is one LLM provider: identity, model catalog, and stream functions.
type Provider struct {
	ID      ai.ProviderId
	Name    string
	Api     ai.Api
	BaseURL string
	// EnvKeys are environment variables consulted for API keys, in order.
	EnvKeys []string
	Models  []ai.Model

	Stream       ai.StreamFunc
	StreamSimple ai.SimpleStreamFunc
}

// Model returns the provider's model with the given id, or nil.
func (p *Provider) Model(id string) *ai.Model {
	for i := range p.Models {
		if p.Models[i].ID == id {
			return &p.Models[i]
		}
	}
	return nil
}

// Registry is an ordered collection of providers.
type Registry struct {
	order     []string
	providers map[string]*Provider
}

// NewRegistry builds a registry from providers in order.
func NewRegistry(providers ...*Provider) *Registry {
	r := &Registry{providers: map[string]*Provider{}}
	for _, p := range providers {
		r.Register(p)
	}
	return r
}

// Register adds or replaces a provider.
func (r *Registry) Register(p *Provider) {
	if _, exists := r.providers[p.ID]; !exists {
		r.order = append(r.order, p.ID)
	}
	r.providers[p.ID] = p
}

// Get returns a provider by id, or nil.
func (r *Registry) Get(id ai.ProviderId) *Provider { return r.providers[id] }

// Providers returns all providers in registration order.
func (r *Registry) Providers() []*Provider {
	out := make([]*Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.providers[id])
	}
	return out
}

// FindModel resolves "provider/model" or (provider, model) pairs.
func (r *Registry) FindModel(providerID ai.ProviderId, modelID string) (*Provider, *ai.Model, error) {
	p := r.Get(providerID)
	if p == nil {
		return nil, nil, fmt.Errorf("provider: unknown provider %q", providerID)
	}
	m := p.Model(modelID)
	if m == nil {
		return nil, nil, fmt.Errorf("provider: unknown model %q for provider %q", modelID, providerID)
	}
	return p, m, nil
}
