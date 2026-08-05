package models

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func storeAt(t *testing.T) *CatalogStore {
	t.Helper()
	return NewCatalogStore(filepath.Join(t.TempDir(), "models-store.json"))
}

func gatewayModels() []ai.Model {
	return []ai.Model{{
		ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Api: ai.ApiPiMessages,
		Provider: "radius", BaseURL: "https://radius.pi.dev/v1",
		Reasoning: true, Input: []string{"text", "image"},
		ContextWindow: 200000, MaxTokens: 64000,
	}}
}

func TestACachedCatalogRoundTrips(t *testing.T) {
	s := storeAt(t)
	if err := s.Write(context.Background(), "radius", gatewayModels()); err != nil {
		t.Fatal(err)
	}

	entry := s.Read("radius")
	if entry == nil || len(entry.Models) != 1 {
		t.Fatalf("entry %+v", entry)
	}
	got := entry.Models[0]
	if got.ID != "claude-sonnet-4-5" || got.Api != ai.ApiPiMessages || got.BaseURL != "https://radius.pi.dev/v1" {
		t.Errorf("model %+v", got)
	}
	if got.ContextWindow != 200000 || got.MaxTokens != 64000 || !got.Reasoning {
		t.Errorf("model %+v", got)
	}
	// The timestamp is what a staleness policy would key on later.
	if entry.CheckedAt == 0 {
		t.Error("checkedAt was not recorded")
	}
}

// The file is Pi's shape — provider id to {models, checkedAt} — so an imported
// Pi setup starts with a warm cache instead of an empty picker.
func TestTheFileIsKeyedByProvider(t *testing.T) {
	s := storeAt(t)
	if err := s.Write(context.Background(), "radius", gatewayModels()); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]struct {
		Models    []map[string]any `json:"models"`
		CheckedAt int64            `json:"checkedAt"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	entry, ok := file["radius"]
	if !ok || len(entry.Models) != 1 || entry.CheckedAt == 0 {
		t.Fatalf("file %s", raw)
	}
	if entry.Models[0]["id"] != "claude-sonnet-4-5" {
		t.Errorf("model %+v", entry.Models[0])
	}
}

// Writing one provider must not drop another's catalog: two gateways can be
// configured, and a login to one would otherwise log the user out of the other.
func TestWritingOneProviderKeepsTheRest(t *testing.T) {
	s := storeAt(t)
	ctx := context.Background()
	if err := s.Write(ctx, "radius", gatewayModels()); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, "other-gateway", gatewayModels()); err != nil {
		t.Fatal(err)
	}

	if s.Read("radius") == nil {
		t.Error("the first catalog was lost")
	}
	if s.Read("other-gateway") == nil {
		t.Error("the second catalog was lost")
	}
}

func TestDeleteRemovesOneProvider(t *testing.T) {
	s := storeAt(t)
	ctx := context.Background()
	if err := s.Write(ctx, "radius", gatewayModels()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "radius"); err != nil {
		t.Fatal(err)
	}
	if s.Read("radius") != nil {
		t.Error("the catalog survived deletion")
	}
	// Deleting what is not there is not an error: logging out twice is normal.
	if err := s.Delete(ctx, "radius"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// THE POINT: this is a cache, and a cache must never be the reason tau will
// not start. Every provider with a compiled catalog is unaffected by it.
func TestACorruptCacheIsNotFatal(t *testing.T) {
	s := storeAt(t)
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if entry := s.Read("radius"); entry != nil {
		t.Errorf("entry %+v, want none", entry)
	}
	// And a write repairs it rather than failing forever.
	if err := s.Write(context.Background(), "radius", gatewayModels()); err != nil {
		t.Fatal(err)
	}
	if s.Read("radius") == nil {
		t.Error("the write did not replace the corrupt file")
	}
}

func TestAMissingFileReadsAsNoEntry(t *testing.T) {
	if entry := storeAt(t).Read("radius"); entry != nil {
		t.Errorf("entry %+v", entry)
	}
}
