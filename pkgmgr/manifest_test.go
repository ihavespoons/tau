package pkgmgr

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// writeTree materializes a map of slash-separated relative paths to contents.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func relSorted(t *testing.T, root string, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatalf("%s not under %s", p, root)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

func TestReadManifest(t *testing.T) {
	t.Run("tau key", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			"package.json": `{"name":"p","tau":{"skills":["a"],"themes":[]}}`,
		})
		m := ReadManifest(root)
		if m == nil {
			t.Fatal("no manifest")
		}
		if !reflect.DeepEqual(m.Skills, []string{"a"}) {
			t.Errorf("skills = %v", m.Skills)
		}
		if entries, declared := m.Entries(TypeThemes); !declared || len(entries) != 0 {
			t.Errorf("themes: entries=%v declared=%v, want declared-and-empty", entries, declared)
		}
		if _, declared := m.Entries(TypePrompts); declared {
			t.Error("prompts should not be declared")
		}
	})

	// A package written for Pi must work unmodified — that is the whole reason
	// the layout matches.
	t.Run("pi key accepted", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			"package.json": `{"name":"p","pi":{"extensions":["index.ts"]}}`,
		})
		m := ReadManifest(root)
		if m == nil || !reflect.DeepEqual(m.Extensions, []string{"index.ts"}) {
			t.Fatalf("manifest = %+v", m)
		}
	})

	t.Run("tau wins over pi", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			"package.json": `{"name":"p","tau":{"skills":["t"]},"pi":{"skills":["p"]}}`,
		})
		if m := ReadManifest(root); !reflect.DeepEqual(m.Skills, []string{"t"}) {
			t.Errorf("skills = %v", m.Skills)
		}
	})

	// A stray comma must not cost the user the whole package.
	t.Run("malformed json is not an error", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"package.json": `{"name":"p",}`})
		if m := ReadManifest(root); m != nil {
			t.Errorf("manifest = %+v, want nil", m)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if m := ReadManifest(t.TempDir()); m != nil {
			t.Errorf("manifest = %+v, want nil", m)
		}
	})
}

func TestPackageIdent(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"package.json": `{"name":"pkg","version":"1.2.3"}`})
	name, version := PackageIdent(root)
	if name != "pkg" || version != "1.2.3" {
		t.Errorf("got %q %q", name, version)
	}
	if name, version := PackageIdent(t.TempDir()); name != "" || version != "" {
		t.Errorf("got %q %q for a package with no package.json", name, version)
	}
}

func TestCollectFilesByConvention(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"skills/deploy/SKILL.md":         "skill",
		"skills/deploy/reference.md":     "not a skill of its own",
		"skills/loose.md":                "single-file skill",
		"skills/.hidden/SKILL.md":        "ignored",
		"skills/node_modules/x/SKILL.md": "ignored",
		"prompts/a.md":                   "prompt",
		"prompts/nested/b.md":            "prompt",
		"prompts/notes.txt":              "not a prompt",
		"themes/dark.json":               "{}",
		"extensions/one.ts":              "//",
		"extensions/two/index.ts":        "//",
		"extensions/three/src/deep.ts":   "// not an entry point",
	})

	cases := []struct {
		t    ResourceType
		want []string
	}{
		{TypeSkills, []string{"skills/deploy/SKILL.md", "skills/loose.md"}},
		{TypePrompts, []string{"prompts/a.md", "prompts/nested/b.md"}},
		{TypeThemes, []string{"themes/dark.json"}},
		{TypeExtensions, []string{"extensions/one.ts", "extensions/two/index.ts"}},
	}
	for _, c := range cases {
		t.Run(string(c.t), func(t *testing.T) {
			all, enabled := CollectFiles(root, c.t)
			if got := relSorted(t, root, all); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for _, p := range all {
				if !enabled[p] {
					t.Errorf("%s not enabled by default", p)
				}
			}
		})
	}
}

// A directory holding SKILL.md is one skill; its own bundled markdown must not
// register as further skills.
func TestSkillDirectoryStopsDescent(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"skills/deploy/SKILL.md":       "skill",
		"skills/deploy/inner/SKILL.md": "must not be found",
		"skills/group/nested/SKILL.md": "a skill two levels down",
	})
	all, _ := CollectFiles(root, TypeSkills)
	want := []string{"skills/deploy/SKILL.md", "skills/group/nested/SKILL.md"}
	if got := relSorted(t, root, all); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtensionEntryPointsFromManifest(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"extensions/ext/package.json": `{"name":"ext","tau":{"extensions":["src/main.ts","missing.ts"]}}`,
		"extensions/ext/src/main.ts":  "//",
		"extensions/ext/index.ts":     "// should lose to the manifest",
	})
	all, _ := CollectFiles(root, TypeExtensions)
	want := []string{"extensions/ext/src/main.ts"}
	if got := relSorted(t, root, all); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCollectFilesFromManifestEntries(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":            `{"name":"p","tau":{"skills":["bundled","extra/solo.md"],"prompts":["p/**/*.md"]}}`,
		"bundled/a/SKILL.md":      "skill",
		"bundled/b/SKILL.md":      "skill",
		"extra/solo.md":           "skill",
		"skills/ignored/SKILL.md": "the manifest wins over convention",
		"p/one.md":                "prompt",
		"p/deep/two.md":           "prompt",
		"p/three.txt":             "not markdown",
	})

	all, enabled := CollectFiles(root, TypeSkills)
	want := []string{"bundled/a/SKILL.md", "bundled/b/SKILL.md", "extra/solo.md"}
	if got := relSorted(t, root, all); !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
	for _, p := range all {
		if !enabled[p] {
			t.Errorf("%s not enabled", p)
		}
	}

	all, _ = CollectFiles(root, TypePrompts)
	wantPrompts := []string{"p/deep/two.md", "p/one.md"}
	if got := relSorted(t, root, all); !reflect.DeepEqual(got, wantPrompts) {
		t.Errorf("prompts = %v, want %v", got, wantPrompts)
	}
}

// A package can ship something switched off, so that a user opts in rather than
// out.
func TestManifestOverridesDisableByDefault(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":     `{"name":"p","tau":{"skills":["s","-s/risky"]}}`,
		"s/safe/SKILL.md":  "skill",
		"s/risky/SKILL.md": "skill",
	})
	all, enabled := CollectFiles(root, TypeSkills)
	if len(all) != 2 {
		t.Fatalf("all = %v", relSorted(t, root, all))
	}
	if !enabled[filepath.Join(root, "s", "safe", "SKILL.md")] {
		t.Error("safe should be enabled")
	}
	if enabled[filepath.Join(root, "s", "risky", "SKILL.md")] {
		t.Error("risky should be disabled by the package's own override")
	}
}

func TestManifestEmptyListShipsNothing(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":      `{"name":"p","tau":{"skills":[]}}`,
		"skills/a/SKILL.md": "skill",
	})
	all, enabled := CollectFiles(root, TypeSkills)
	if len(all) != 0 || len(enabled) != 0 {
		t.Errorf("all=%v enabled=%v, want empty", relSorted(t, root, all), enabled)
	}
}

// An entry naming a type that is not declared still falls back to convention,
// so declaring skills does not silently disable themes.
func TestUndeclaredTypeFallsBackToConvention(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":     `{"name":"p","tau":{"skills":["s"]}}`,
		"s/a/SKILL.md":     "skill",
		"themes/dark.json": "{}",
	})
	all, _ := CollectFiles(root, TypeThemes)
	if got := relSorted(t, root, all); !reflect.DeepEqual(got, []string{"themes/dark.json"}) {
		t.Errorf("themes = %v", got)
	}
}
