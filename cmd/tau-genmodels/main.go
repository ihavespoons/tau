// Command tau-genmodels generates tau's compiled model catalogs.
//
// The catalog is data, but it is not raw data: models.dev describes what a
// model is, while tau needs to know how to talk to it, and the gap between
// those is a few hundred hand-verified corrections. Those corrections live
// here, in one place, next to a fidelity test that diffs the result against
// Pi's own generated catalog — so a divergence shows up as a failing test
// rather than as a provider that mysteriously rejects a request.
//
// Usage:
//
//	go run ./cmd/tau-genmodels             # regenerate from the vendored snapshot
//	go run ./cmd/tau-genmodels -refresh    # re-fetch the upstream sources first
//	go run ./cmd/tau-genmodels -report     # print a per-provider model count
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ihavespoons/tau/ai"
)

func main() {
	var (
		dataDir = flag.String("data", "ai/provider/catalog/data", "directory holding the vendored source snapshots")
		outDir  = flag.String("out", "ai/provider/catalog", "directory to write generated catalogs into")
		refresh = flag.Bool("refresh", false, "re-fetch the upstream sources before generating")
		report  = flag.Bool("report", false, "print a per-provider model count instead of writing files")
	)
	flag.Parse()

	if err := run(*dataDir, *outDir, *refresh, *report); err != nil {
		fmt.Fprintln(os.Stderr, "tau-genmodels:", err)
		os.Exit(1)
	}
}

func run(dataDir, outDir string, refresh, report bool) error {
	if refresh {
		if err := refreshSources(dataDir); err != nil {
			return err
		}
	}

	cat, err := loadModelsDev(filepath.Join(dataDir, "modelsdev.json"))
	if err != nil {
		return err
	}

	catalogs, err := buildAll(cat, dataDir)
	if err != nil {
		return err
	}

	if report {
		ids := make([]string, 0, len(catalogs))
		for id := range catalogs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		total := 0
		for _, id := range ids {
			fmt.Printf("%-26s %4d models\n", id, len(catalogs[id]))
			total += len(catalogs[id])
		}
		fmt.Printf("%-26s %4d models across %d providers\n", "TOTAL", total, len(ids))
		return nil
	}

	return emitAll(outDir, catalogs)
}

// buildAll produces every provider catalog, keyed by tau provider id.
func buildAll(cat modelsDevCatalog, dataDir string) (map[string][]ai.Model, error) {
	idx := newReasoningIndex()
	out := map[string][]ai.Model{}

	for _, spec := range specs() {
		models := spec.build(cat, idx)
		if len(models) == 0 {
			continue
		}
		out[spec.ID] = models
	}

	// Corrections run after every provider is built, because several of them
	// depend on the finished model rather than on its models.dev source.
	for _, models := range out {
		for i := range models {
			applyMetadata(&models[i], idx)
		}
	}

	return out, nil
}
