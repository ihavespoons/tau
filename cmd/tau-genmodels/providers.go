package main

import (
	"sort"

	"github.com/ihavespoons/tau/ai"
)

// providerSpec describes how one provider's catalog is derived from a
// models.dev provider entry.
//
// Nearly every provider is the same transformation with a different id, wire
// API, and endpoint — so they are a table rather than a function each. The two
// hooks cover what actually differs: which models to leave out, and which
// fields to correct afterwards.
type providerSpec struct {
	// Source is the models.dev provider key. Empty means the provider is not
	// derived from models.dev at all.
	Source string
	// SourceAlts are further keys to try when Source is absent. models.dev has
	// renamed providers more than once, and a rename would otherwise drop the
	// whole catalog silently.
	SourceAlts []string
	// ID is tau's provider id, and the name of the generated catalog.
	ID string
	// Name is the display name.
	Name string
	// Api is the wire API every model of this provider speaks.
	Api ai.Api
	// BaseURL is the endpoint, unless PerModelBaseURL overrides it.
	BaseURL string

	// Skip drops a model before it reaches the catalog.
	Skip func(id string, m modelsDevModel) bool
	// Tweak corrects a built model in place.
	Tweak func(id string, m modelsDevModel, out *ai.Model)
	// PerModelBaseURL overrides BaseURL for models that need their own.
	PerModelBaseURL func(id string) string
	// Extra are models the provider serves that models.dev does not describe.
	// They still go through the correction passes.
	Extra []ai.Model
}

// build turns the source provider's models into a sorted catalog.
//
// Only tool-capable models are included: tau is a coding agent, and a model
// that cannot call a tool cannot do the job, so listing it would only offer
// the user a way to break their session.
func (s providerSpec) build(cat modelsDevCatalog, opts *reasoningIndex) []ai.Model {
	src, ok := cat[s.Source]
	for _, alt := range s.SourceAlts {
		if ok {
			break
		}
		src, ok = cat[alt]
	}
	if !ok {
		return nil
	}

	var out []ai.Model
	for id, m := range src.Models {
		if !m.ToolCall {
			continue
		}
		if s.Skip != nil && s.Skip(id, m) {
			continue
		}

		baseURL := s.BaseURL
		if s.PerModelBaseURL != nil {
			if u := s.PerModelBaseURL(id); u != "" {
				baseURL = u
			}
		}

		model := ai.Model{
			ID:            id,
			Name:          m.displayName(id),
			Api:           s.Api,
			Provider:      ai.ProviderId(s.ID),
			BaseURL:       baseURL,
			Reasoning:     m.Reasoning,
			Input:         m.inputModalities(),
			Cost:          ai.ModelCost{ModelCostRates: m.rates()},
			ContextWindow: m.contextWindow(),
			MaxTokens:     m.maxTokens(),
		}
		if s.Tweak != nil {
			s.Tweak(id, m, &model)
		}
		opts.record(s.ID, id, m)
		out = append(out, model)
	}

	// An extra model is a stand-in for something upstream has not listed yet.
	// When upstream catches up, the derived entry is the better one — it has
	// current pricing and limits — so the stand-in steps aside rather than
	// producing a duplicate id.
	derived := make(map[string]bool, len(out))
	for _, m := range out {
		derived[m.ID] = true
	}
	for _, m := range s.Extra {
		if !derived[m.ID] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// reasoningIndex remembers each model's models.dev reasoning options so the
// thinking-level map can be applied after compat detection has run — the map
// only applies to models whose wire API takes a direct effort value, and that
// is not known until the model is fully built.
type reasoningIndex struct {
	byKey map[string][]reasoningOption
}

func newReasoningIndex() *reasoningIndex {
	return &reasoningIndex{byKey: map[string][]reasoningOption{}}
}

func (r *reasoningIndex) record(provider, id string, m modelsDevModel) {
	if m.ReasoningOptions != nil {
		r.byKey[provider+":"+id] = m.ReasoningOptions
	}
}

func (r *reasoningIndex) get(provider, id string) []reasoningOption {
	return r.byKey[provider+":"+id]
}

// boolptr and helpers keep the compat literals in the tables below readable.
func boolptr(b bool) *bool { return &b }

// withCompat returns a tweak that sets compat flags on every model.
func withCompat(apply func(c *ai.CompatFlags)) func(string, modelsDevModel, *ai.Model) {
	return func(_ string, _ modelsDevModel, out *ai.Model) {
		if out.Compat == nil {
			out.Compat = &ai.CompatFlags{}
		}
		apply(out.Compat)
	}
}
