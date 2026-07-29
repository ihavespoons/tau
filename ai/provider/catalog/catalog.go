// Package catalog holds tau's compiled model data: what every known provider
// offers, generated from vendored upstream snapshots by cmd/tau-genmodels.
//
// Nothing in here is written by hand. To change a model's data, change the
// generator's correction tables and regenerate — an edit here is erased by the
// next run, and the fidelity test that guards the catalog would not see it.
//
//go:generate go run ../../../cmd/tau-genmodels
package catalog

import "github.com/ihavespoons/tau/ai"

// These constructors exist because the generated files need to write pointer
// values inline, and Go has no literal syntax for the address of a constant.
func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }
func intptr(i int) *int       { return &i } //nolint:unused // used by generated compat flags

// Models returns a provider's compiled catalog, or nil if tau has no built-in
// data for it. The slice is a copy: a caller that adjusts a model must not
// change what every other session sees.
func Models(providerID string) []ai.Model {
	models, ok := Catalogs[providerID]
	if !ok {
		return nil
	}
	return append([]ai.Model(nil), models...)
}

// ProviderIDs lists every provider with compiled data.
func ProviderIDs() []string {
	ids := make([]string, 0, len(Catalogs))
	for id := range Catalogs {
		ids = append(ids, id)
	}
	return ids
}
