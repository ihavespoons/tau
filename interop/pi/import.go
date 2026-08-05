package pi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ihavespoons/tau/session"
)

// ImportOptions selects what an import copies and where it puts it.
type ImportOptions struct {
	// Source is the Pi agent directory to read.
	Source string
	// AgentDir is tau's agent directory to write into.
	AgentDir string
	// SessionsDir is tau's session root. Empty means <AgentDir>/sessions.
	SessionsDir string

	// What to bring across. All default to off; the caller decides, because
	// credentials in particular should never be copied by accident.
	Sessions  bool
	Settings  bool
	Auth      bool
	Models    bool
	Resources bool

	// Overwrite replaces destination files that already exist. Without it an
	// existing file is left alone and reported as skipped — a second import
	// must not be able to undo work done in tau since the first.
	Overwrite bool

	// DryRun reports what would happen without writing anything.
	DryRun bool
}

// Report is what an import did, or would do.
type Report struct {
	// Copied names each item brought across.
	Copied []string
	// Skipped names each item left alone, with the reason.
	Skipped []string
	// Migrated counts sessions upgraded from an older format on the way in.
	Migrated int
	// Problems are failures that did not stop the rest of the import.
	Problems []string
	// DryRun records that nothing was actually written.
	DryRun bool
}

// Import copies a Pi installation into tau's directories.
//
// The source is never modified. Failures on individual items are collected
// rather than aborting: a single unreadable session should not cost the user
// the other four hundred.
func Import(opts ImportOptions) (*Report, error) {
	if opts.Source == "" || opts.AgentDir == "" {
		return nil, errors.New("import needs both a source and a destination")
	}
	if opts.Source == opts.AgentDir {
		return nil, errors.New("the source and destination are the same directory")
	}
	sessionsDir := opts.SessionsDir
	if sessionsDir == "" {
		sessionsDir = filepath.Join(opts.AgentDir, "sessions")
	}

	report := &Report{DryRun: opts.DryRun}

	if !opts.DryRun {
		if err := os.MkdirAll(opts.AgentDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", opts.AgentDir, err)
		}
	}

	if opts.Settings {
		importFile(report, opts, filepath.Join(opts.Source, "settings.json"),
			filepath.Join(opts.AgentDir, "settings.json"), 0o644)
	}
	if opts.Models {
		importFile(report, opts, filepath.Join(opts.Source, "models.json"),
			filepath.Join(opts.AgentDir, "models.json"), 0o644)
		importFile(report, opts, filepath.Join(opts.Source, "models-store.json"),
			filepath.Join(opts.AgentDir, "models-store.json"), 0o644)
	}
	if opts.Auth {
		// 0600, always. The source may be laxer than that — tau's own store
		// writes 0600 and an imported file must not be the exception.
		importFile(report, opts, filepath.Join(opts.Source, "auth.json"),
			filepath.Join(opts.AgentDir, "auth.json"), 0o600)
	}
	if opts.Resources {
		for _, dir := range resourceDirs {
			importDir(report, opts, filepath.Join(opts.Source, dir), filepath.Join(opts.AgentDir, dir))
		}
	}
	if opts.Sessions {
		importSessions(report, opts, filepath.Join(opts.Source, "sessions"), sessionsDir)
	}

	return report, nil
}

// importFile copies one file, honouring Overwrite.
func importFile(report *Report, opts ImportOptions, src, dst string, mode os.FileMode) {
	name := filepath.Base(src)
	if !fileExists(src) {
		return
	}
	if fileExists(dst) && !opts.Overwrite {
		report.Skipped = append(report.Skipped, name+" (already in tau)")
		return
	}
	if opts.DryRun {
		report.Copied = append(report.Copied, name)
		return
	}
	if err := copyFile(src, dst, mode); err != nil {
		report.Problems = append(report.Problems, name+": "+err.Error())
		return
	}
	report.Copied = append(report.Copied, name)
}

// importDir copies a resource directory file by file, so an existing tau
// resource of the same name is preserved unless Overwrite says otherwise.
func importDir(report *Report, opts ImportOptions, src, dst string) {
	if !dirExists(src) {
		return
	}
	base := filepath.Base(src)
	copied, skipped := 0, 0

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			report.Problems = append(report.Problems, path+": "+err.Error())
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return nil
		}
		target := filepath.Join(dst, rel)
		if fileExists(target) && !opts.Overwrite {
			skipped++
			return nil
		}
		if opts.DryRun {
			copied++
			return nil
		}
		if err := copyFile(path, target, 0o644); err != nil {
			report.Problems = append(report.Problems, rel+": "+err.Error())
			return nil
		}
		copied++
		return nil
	})
	if err != nil {
		report.Problems = append(report.Problems, base+": "+err.Error())
	}

	if copied > 0 {
		report.Copied = append(report.Copied, fmt.Sprintf("%s (%d files)", base, copied))
	}
	if skipped > 0 {
		report.Skipped = append(report.Skipped, fmt.Sprintf("%s (%d already in tau)", base, skipped))
	}
}

// importSessions copies each session file, migrating older formats on the way.
//
// The destination path is the source's relative path unchanged: tau's cwd
// encoding is byte-identical to Pi's, so a session imported from
// sessions/--Users-ben-Code-tau--/x.jsonl lands where tau would have put it.
func importSessions(report *Report, opts ImportOptions, srcRoot, dstRoot string) {
	if !dirExists(srcRoot) {
		return
	}

	copied := 0
	skipped := 0

	err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			report.Problems = append(report.Problems, path+": "+err.Error())
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		rel, relErr := filepath.Rel(srcRoot, path)
		if relErr != nil {
			return nil
		}
		target := filepath.Join(dstRoot, rel)

		if fileExists(target) && !opts.Overwrite {
			skipped++
			return nil
		}

		lines, err := readLines(path)
		if err != nil {
			report.Problems = append(report.Problems, rel+": "+err.Error())
			return nil
		}
		migrated, changed, err := session.MigrateLines(lines)
		if err != nil {
			report.Problems = append(report.Problems, rel+": "+err.Error())
			return nil
		}
		if changed {
			report.Migrated++
		}

		if opts.DryRun {
			copied++
			return nil
		}
		if err := writeLines(target, migrated); err != nil {
			report.Problems = append(report.Problems, rel+": "+err.Error())
			return nil
		}
		copied++
		return nil
	})
	if err != nil {
		report.Problems = append(report.Problems, err.Error())
	}

	if copied > 0 {
		report.Copied = append(report.Copied, fmt.Sprintf("sessions (%d files)", copied))
	}
	if skipped > 0 {
		report.Skipped = append(report.Skipped, fmt.Sprintf("sessions (%d already in tau)", skipped))
	}
}

func readLines(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		out = append(out, []byte(trimmed))
	}
	return out, nil
}

func writeLines(path string, lines [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, line := range lines {
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// copyFile writes src to dst with the given mode, creating parents.
func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

// Describe renders a report for a human.
func (r *Report) Describe() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("Dry run — nothing was written.\n")
	}
	if len(r.Copied) == 0 && len(r.Skipped) == 0 && len(r.Problems) == 0 {
		b.WriteString("Nothing to import.\n")
		return b.String()
	}
	for _, c := range r.Copied {
		fmt.Fprintf(&b, "  imported  %s\n", c)
	}
	if r.Migrated > 0 {
		fmt.Fprintf(&b, "  migrated  %d sessions to v%d\n", r.Migrated, session.Version)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "  skipped   %s\n", s)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(&b, "  ! %s\n", p)
	}
	return b.String()
}
