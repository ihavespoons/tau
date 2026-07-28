package prompt

import (
	"os"
	"path/filepath"
)

// contextFileNames are tried in order within a directory; the first hit wins
// and the rest are ignored (resource-loader.ts:68).
var contextFileNames = []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// LoadContextFiles discovers project instruction files, port of
// loadProjectContextFiles (resource-loader.ts:85-123).
//
// Order is the global agent directory first, then every ancestor of cwd
// ordered outermost-first — so the repo root's AGENTS.md precedes a
// subdirectory's, and the nearest file is last and therefore most salient.
// A path already collected is never added twice.
//
// Unreadable files are skipped rather than failing the build: a project with
// a permission-denied AGENTS.md should still get an agent.
func LoadContextFiles(cwd, agentDir string) []ContextFile {
	var files []ContextFile
	seen := map[string]bool{}

	if agentDir != "" {
		if f, ok := loadContextFileFromDir(agentDir); ok {
			files = append(files, f)
			seen[f.Path] = true
		}
	}

	var ancestors []ContextFile
	dir := cwd
	for dir != "" {
		if f, ok := loadContextFileFromDir(dir); ok && !seen[f.Path] {
			// Prepend: walking up yields nearest-first, but the prompt wants
			// outermost-first.
			ancestors = append([]ContextFile{f}, ancestors...)
			seen[f.Path] = true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return append(files, ancestors...)
}

func loadContextFileFromDir(dir string) (ContextFile, bool) {
	for _, name := range contextFileNames {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return ContextFile{Path: path, Content: string(content)}, true
	}
	return ContextFile{}, false
}
