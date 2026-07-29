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
	// Rename folds an upstream id onto a different one, returning the new id
	// and optionally a new display name. Upstream sometimes publishes several
	// versioned aliases for one served model.
	Rename func(id string) (string, string)
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
		// A provider may be entirely hand-written. Its Extra models are still
		// the catalog, so an absent source is not an empty result.
		if s.Source == "" {
			return append([]ai.Model(nil), s.Extra...)
		}
		return nil
	}

	var out []ai.Model
	// renamed records which ids arrived via an alias, so a collision with the
	// canonical entry resolves in favour of the real one.
	renamed := map[string]bool{}

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

		modelID, renamedTo := id, ""
		if s.Rename != nil {
			modelID, renamedTo = s.Rename(id)
		}

		model := ai.Model{
			ID:            modelID,
			Name:          m.displayName(modelID),
			Api:           s.Api,
			Provider:      ai.ProviderId(s.ID),
			BaseURL:       baseURL,
			Reasoning:     m.Reasoning,
			Input:         m.inputModalities(),
			Cost:          ai.ModelCost{ModelCostRates: m.rates()},
			ContextWindow: m.contextWindow(),
			MaxTokens:     m.maxTokens(),
		}
		if renamedTo != "" {
			model.Name = renamedTo
		}
		if s.Tweak != nil {
			s.Tweak(id, m, &model)
		}
		opts.record(s.ID, modelID, m)

		if modelID != id {
			renamed[modelID] = true
		}
		out = append(out, model)
	}

	out = dedupeRenamed(out, renamed)

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

// dedupeRenamed resolves ids that two entries now share because an alias was
// folded onto a canonical name.
//
// The canonical entry wins. Upstream publishes both when it is mid-rename, and
// the aliased copy is the one whose metadata lags — so preferring it would
// pick the staler of two descriptions of the same model.
func dedupeRenamed(models []ai.Model, renamed map[string]bool) []ai.Model {
	if len(renamed) == 0 {
		return models
	}

	seen := map[string]int{}
	var out []ai.Model
	for _, m := range models {
		idx, ok := seen[m.ID]
		if !ok {
			seen[m.ID] = len(out)
			out = append(out, m)
			continue
		}
		// A collision: keep whichever did not come from an alias. If both did,
		// the first wins, which is stable because ids are sorted afterwards.
		if !renamed[m.ID] {
			out[idx] = m
		}
	}
	return out
}
