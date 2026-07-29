package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// The catalog is where a port silently goes wrong. A wrong base URL, a missing
// compat flag, or a thinking level the provider does not accept produces a
// model that looks fine in a list and fails on first use — and there are over
// a thousand of them, so nobody is going to notice by reading.
//
// testdata/picatalog holds Pi 0.82.1's own generated catalog, produced by
// running its generator against the same models.dev snapshot tau vendors. It
// is the spec: any difference is either a bug in tau's generator or a
// deliberate divergence, and the second kind belongs in knownDivergences with
// a reason.

// knownDivergences records where tau's catalog intentionally differs from
// Pi's, keyed by "provider/model#field" for one field or "provider/model" for
// a whole model. Every entry needs a reason, and an entry that stops matching
// anything is reported so the list cannot rot into a set of excuses for bugs
// that were fixed years ago.
var knownDivergences = map[string]string{
	"together/MiniMaxAI/MiniMax-M2.7#compat": "Pi keeps two copies of chat-completions detection " +
		"and they have drifted: the generator's copy withholds thinkingFormat for Together's " +
		"reasoning-only models, its runtime copy does not. Since the catalog value only ever " +
		"overrides detection with the same string, both agree on the wire — so tau records what " +
		"detection concludes rather than reproducing the omission.",
}

// usedDivergences records which entries actually suppressed a difference.
var usedDivergences = map[string]bool{}

// notYetGenerated lists providers whose catalogs tau does not build yet. They
// are named rather than skipped silently, so the remaining work is visible in
// the test output instead of being invisible in its absence.
var notYetGenerated = map[string]string{
	"amazon-bedrock":         "needs per-region inference-profile ids",
	"ant-ling":               "hand-written model list, not derived from models.dev",
	"azure-openai-responses": "needs the azure-openai-responses wire",
	"cloudflare-ai-gateway":  "multi-api provider: routes to three wires by model",
	"deepseek":               "hand-written model list, not derived from models.dev",
	"github-copilot":         "needs the authenticated copilot catalog",
	"google-vertex":          "needs the google-vertex wire",
	"kimi-coding":            "hand-written model list, not derived from models.dev",
	"minimax":                "hand-written model list, not derived from models.dev",
	"minimax-cn":             "hand-written model list, not derived from models.dev",
	"nvidia":                 "needs the live NIM id mapping",
	"openai-codex":           "hand-written model list, not derived from models.dev",
	"opencode":               "needs the opencode zen catalog",
	"opencode-go":            "needs the opencode zen catalog",
}

// loadGolden reads Pi's catalog for one provider.
func loadGolden(t *testing.T, providerID string) map[string]ai.Model {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "picatalog", providerID+".json"))
	if err != nil {
		t.Fatalf("reading the golden catalog: %v", err)
	}
	var out map[string]ai.Model
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decoding the golden catalog: %v", err)
	}
	return out
}

func goldenProviderIDs(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "picatalog", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no golden catalogs found: %v", err)
	}
	ids := make([]string, 0, len(paths))
	for _, p := range paths {
		ids = append(ids, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	sort.Strings(ids)
	return ids
}

// TestCatalogMatchesPi is the fidelity gate.
func TestCatalogMatchesPi(t *testing.T) {
	for _, providerID := range goldenProviderIDs(t) {
		t.Run(providerID, func(t *testing.T) {
			if reason, pending := notYetGenerated[providerID]; pending {
				if _, built := Catalogs[providerID]; built {
					t.Fatalf("%s is generated now — remove it from notYetGenerated", providerID)
				}
				t.Skipf("not generated yet: %s", reason)
			}

			golden := loadGolden(t, providerID)
			built := Catalogs[providerID]
			if len(built) == 0 {
				t.Fatalf("no compiled catalog for %s (%d models expected)", providerID, len(golden))
			}

			byID := make(map[string]ai.Model, len(built))
			for _, m := range built {
				byID[m.ID] = m
			}

			for id, want := range golden {
				key := providerID + "/" + id
				if _, ok := knownDivergences[key]; ok {
					usedDivergences[key] = true
					continue
				}
				got, ok := byID[id]
				if !ok {
					t.Errorf("missing model %q", id)
					continue
				}
				for _, d := range diffModel(want, got) {
					fieldKey := key + "#" + d.field
					if _, ok := knownDivergences[fieldKey]; ok {
						usedDivergences[fieldKey] = true
						continue
					}
					t.Errorf("%s: %s", id, d)
				}
			}

			for id := range byID {
				if _, ok := golden[id]; !ok {
					if _, known := knownDivergences[providerID+"/"+id]; !known {
						t.Errorf("extra model %q that Pi does not list", id)
					}
				}
			}
		})
	}
}

// diffModel reports every field that differs, rather than stopping at the
// first: fixing a generator bug usually fixes a whole class of them, and
// seeing the class is what tells you which correction is missing.
// difference is one field that does not match, kept structured so a known
// divergence can be scoped to a single field instead of a whole model.
type difference struct {
	field string
	text  string
}

func (d difference) String() string { return d.text }

func diffModel(want, got ai.Model) []difference {
	var diffs []difference
	cmp := func(field string, w, g any) {
		if !reflect.DeepEqual(w, g) {
			diffs = append(diffs, difference{
				field: field,
				text:  fmt.Sprintf("%s: pi=%v tau=%v", field, w, g),
			})
		}
	}

	cmp("name", want.Name, got.Name)
	cmp("api", want.Api, got.Api)
	cmp("provider", want.Provider, got.Provider)
	cmp("baseUrl", want.BaseURL, got.BaseURL)
	cmp("reasoning", want.Reasoning, got.Reasoning)
	cmp("input", want.Input, got.Input)
	cmp("contextWindow", want.ContextWindow, got.ContextWindow)
	cmp("maxTokens", want.MaxTokens, got.MaxTokens)
	cmp("headers", want.Headers, got.Headers)

	// Cost and compat are compared through JSON so an absent field and a zero
	// one read the same way they do on the wire.
	cmp("cost", jsonOf(want.Cost), jsonOf(got.Cost))
	cmp("compat", jsonOf(want.Compat), jsonOf(got.Compat))
	cmp("thinkingLevelMap", jsonOf(want.ThinkingLevelMap), jsonOf(got.ThinkingLevelMap))
	return diffs
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unmarshalable: %v>", err)
	}
	return string(b)
}

// A divergence that no longer suppresses anything is a stale excuse, and the
// next real difference in that field would be silently swallowed by it.
func TestNoStaleDivergences(t *testing.T) {
	if len(usedDivergences) == 0 {
		t.Skip("run alongside TestCatalogMatchesPi")
	}
	for key := range knownDivergences {
		if !usedDivergences[key] {
			t.Errorf("%q no longer diverges — remove it", key)
		}
	}
}

// Every generated catalog must be reachable through the index, or a provider
// exists in the binary that nothing can select.
func TestEveryCatalogIsIndexed(t *testing.T) {
	for id, models := range Catalogs {
		if len(models) == 0 {
			t.Errorf("%s is indexed with no models", id)
		}
		for _, m := range models {
			if string(m.Provider) != id {
				t.Errorf("%s: model %q claims provider %q", id, m.ID, m.Provider)
			}
		}
	}
}

// Models returns a copy, so a caller adjusting one model cannot change what
// every other session sees.
func TestModelsReturnsACopy(t *testing.T) {
	first := Models("anthropic")
	if len(first) == 0 {
		t.Fatal("no anthropic models")
	}
	first[0].Name = "mutated"

	if second := Models("anthropic"); second[0].Name == "mutated" {
		t.Error("Models handed out the shared backing array")
	}
}
