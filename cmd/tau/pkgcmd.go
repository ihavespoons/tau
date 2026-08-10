package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/pkgmgr"
	"github.com/ihavespoons/tau/settings"
)

// packageOpts are the flags every package subcommand shares.
type packageOpts struct {
	local     bool
	approve   bool
	noApprove bool
}

func (o *packageOpts) register(fs *flag.FlagSet) {
	fs.BoolVar(&o.local, "local", false, "use this project's "+config.DirName+"/settings.json")
	fs.BoolVar(&o.local, "l", false, "shorthand for -local")
	fs.BoolVar(&o.approve, "approve", false, "trust this project's "+config.DirName+" resources")
	fs.BoolVar(&o.noApprove, "no-approve", false, "do not trust this project's "+config.DirName+" resources")
}

// override turns the trust flags into a decision, or nil to resolve normally.
func (o *packageOpts) override() (*bool, error) {
	switch {
	case o.approve && o.noApprove:
		return nil, errors.New("-approve and -no-approve contradict each other")
	case o.approve:
		yes := true
		return &yes, nil
	case o.noApprove:
		no := false
		return &no, nil
	}
	return nil, nil
}

func (o *packageOpts) settingsScope() settings.Scope {
	if o.local {
		return settings.Project
	}
	return settings.Global
}

func (o *packageOpts) pkgScope() pkgmgr.Scope {
	if o.local {
		return pkgmgr.ScopeProject
	}
	return pkgmgr.ScopeUser
}

// packageEnv is the state every package subcommand works against: the settings
// file it will edit and the manager that touches the disk.
type packageEnv struct {
	cwd     string
	set     *settings.Manager
	mgr     *pkgmgr.Manager
	trusted bool
	reason  string
}

// newPackageEnv resolves trust, loads settings and builds a manager.
//
// Trust is decided before anything project-scoped is read or written, because
// installing into a checkout writes into it and running its packages executes
// its code. There is no prompt on this path, so an undecided project is denied
// and the user reaches it deliberately with -approve.
func newPackageEnv(o *packageOpts) (*packageEnv, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolving working directory: %w", err)
	}
	override, err := o.override()
	if err != nil {
		return nil, err
	}
	tr := coding.ResolveTrust(cwd, override)

	set, err := settings.Load(settings.Options{
		Cwd: cwd, AgentDir: config.AgentDir(), ProjectTrusted: tr.Trusted,
	})
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}
	for _, e := range set.Errors() {
		fmt.Fprintln(os.Stderr, "tau: "+e.Error())
	}

	if o.local && !tr.Trusted {
		return nil, fmt.Errorf("project is not trusted (%s)\n"+
			"       pass -approve to install into %s", tr.Reason, config.ProjectDir(cwd))
	}

	return &packageEnv{
		cwd: cwd,
		set: set,
		mgr: pkgmgr.New(pkgmgr.Options{
			AgentDir: config.AgentDir(), Cwd: cwd, ProjectTrusted: tr.Trusted,
		}),
		trusted: tr.Trusted,
		reason:  tr.Reason,
	}, nil
}

// rawPackages returns one scope's package entries exactly as configured.
func (e *packageEnv) rawPackages(scope settings.Scope) ([]json.RawMessage, error) {
	s, err := e.set.Scoped(scope)
	if err != nil {
		return nil, err
	}
	return s.Packages, nil
}

// writePackages persists a package list, removing the key when it empties so
// an unused "packages": [] is not left behind.
func (e *packageEnv) writePackages(ctx context.Context, scope settings.Scope, list []json.RawMessage) error {
	if len(list) == 0 {
		return e.set.Unset(ctx, scope, "packages")
	}
	return e.set.Set(ctx, scope, "packages", list)
}

// sameIdentity reports whether two sources name the same package, ignoring the
// version or ref. Installing npm:pkg@2 over npm:pkg@1 replaces the entry
// rather than configuring the package twice.
func sameIdentity(a, b pkgmgr.Source) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case pkgmgr.KindNPM:
		return a.Name == b.Name
	case pkgmgr.KindGit:
		return a.Host == b.Host && a.Path == b.Path
	case pkgmgr.KindLocal:
		return a.LocalPath == b.LocalPath
	}
	return false
}

// findPackage locates a configured entry for a source, returning its index.
func findPackage(list []json.RawMessage, source string) int {
	want := pkgmgr.ParseSource(source)
	for i, raw := range list {
		entry, err := pkgmgr.ParseEntry(raw)
		if err != nil {
			continue
		}
		if entry.Source == source || sameIdentity(pkgmgr.ParseSource(entry.Source), want) {
			return i
		}
	}
	return -1
}

// setEntrySource rewrites an entry's source in place.
//
// The object form is edited as a map rather than through pkgmgr.Entry so that
// keys tau does not model survive: a user's per-resource filters are the whole
// reason they wrote the object form, and reinstalling must not discard them.
func setEntrySource(raw json.RawMessage, source string) (json.RawMessage, error) {
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(source)
		if err != nil {
			return nil, err
		}
		obj["source"] = encoded
		return json.Marshal(obj)
	}
	return json.Marshal(source)
}

// installCmd is `tau install <source>`: fetch a package and configure it.
func installCmd(args []string) error {
	fs := flag.NewFlagSet("tau install", flag.ContinueOnError)
	var o packageOpts
	o.register(fs)
	fs.Usage = func() {
		fmt.Println("usage: tau install <source> [flags]")
		fmt.Println("\nsources:")
		fmt.Println("  npm:@scope/name[@version]      an npm package")
		fmt.Println("  git:github.com/user/repo       a git repository (also github:/gitlab:/bitbucket:)")
		fmt.Println("  https://github.com/user/repo   a git repository by URL")
		fmt.Println("  ./local/path                   a directory, used where it lies")
		fmt.Println("\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	source := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if source == "" {
		fs.Usage()
		return errors.New("no package source given")
	}

	env, err := newPackageEnv(&o)
	if err != nil {
		return err
	}
	ctx, cancel := ctxWithSignals()
	defer cancel()

	src, path, err := env.mgr.Install(ctx, source, o.pkgScope())
	if err != nil {
		return err
	}

	scope := o.settingsScope()
	list, err := env.rawPackages(scope)
	if err != nil {
		return err
	}
	if i := findPackage(list, source); i >= 0 {
		updated, err := setEntrySource(list[i], source)
		if err != nil {
			return err
		}
		list[i] = updated
	} else {
		encoded, err := json.Marshal(source)
		if err != nil {
			return err
		}
		list = append(list, encoded)
	}
	if err := env.writePackages(ctx, scope, list); err != nil {
		return err
	}

	fmt.Printf("Installed %s\n", source)
	if src.Kind == pkgmgr.KindLocal {
		fmt.Printf("  using %s in place\n", path)
	} else {
		fmt.Printf("  at %s\n", path)
	}
	fmt.Printf("  configured in %s\n", env.set.Path(scope))
	describeProvided(env.mgr, source, o.pkgScope())
	return nil
}

// describeProvided reports what a package contributes, so an install that
// resolves to nothing says so instead of looking like it worked.
func describeProvided(mgr *pkgmgr.Manager, source string, scope pkgmgr.Scope) {
	res := mgr.Resolve([]pkgmgr.Entry{{Source: source}}, scope)
	var parts []string
	for _, t := range pkgmgr.ResourceTypes {
		if n := len(res.Enabled(t)); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, t))
		}
	}
	if len(parts) == 0 {
		fmt.Println("  provides no skills, prompts, themes or extensions")
		return
	}
	fmt.Printf("  provides %s\n", strings.Join(parts, ", "))
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "tau: "+w)
	}
}

// removeCmd is `tau remove <source>`: drop a package from settings and disk.
func removeCmd(args []string) error {
	fs := flag.NewFlagSet("tau remove", flag.ContinueOnError)
	var o packageOpts
	o.register(fs)
	fs.Usage = func() {
		fmt.Println("usage: tau remove <source> [flags]")
		fmt.Println("\nAlias: tau uninstall <source>")
		fmt.Println("\nA local package is unconfigured but never deleted — tau did not put it there.")
		fmt.Println("\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	source := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if source == "" {
		fs.Usage()
		return errors.New("no package source given")
	}

	env, err := newPackageEnv(&o)
	if err != nil {
		return err
	}
	ctx, cancel := ctxWithSignals()
	defer cancel()

	scope := o.settingsScope()
	list, err := env.rawPackages(scope)
	if err != nil {
		return err
	}
	configured := findPackage(list, source)
	if configured >= 0 {
		entry, perr := pkgmgr.ParseEntry(list[configured])
		if perr == nil {
			// Remove what settings actually named: "tau remove npm:pkg" should
			// take out the npm:pkg@1.2.3 the user installed.
			source = entry.Source
		}
		list = append(list[:configured:configured], list[configured+1:]...)
		if err := env.writePackages(ctx, scope, list); err != nil {
			return err
		}
	}

	installed := packageIsInstalled(env.mgr, source, o.pkgScope())
	if configured < 0 && !installed {
		return fmt.Errorf("no package matching %s is installed or configured", source)
	}
	if err := env.mgr.Remove(ctx, source, o.pkgScope()); err != nil {
		return err
	}

	fmt.Printf("Removed %s\n", source)
	if configured >= 0 {
		fmt.Printf("  unconfigured in %s\n", env.set.Path(scope))
	}
	if pkgmgr.ParseSource(source).Kind == pkgmgr.KindLocal {
		fmt.Println("  the directory itself was left alone")
	}
	return nil
}

// packageIsInstalled reports whether a source has anything on disk.
func packageIsInstalled(mgr *pkgmgr.Manager, source string, scope pkgmgr.Scope) bool {
	path, err := mgr.PackagePath(pkgmgr.ParseSource(source), scope)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// updateCmd is `tau update [source]`: refetch installed packages.
func updateCmd(args []string) error {
	fs := flag.NewFlagSet("tau update", flag.ContinueOnError)
	var o packageOpts
	o.register(fs)
	fs.Usage = func() {
		fmt.Println("usage: tau update [source] [flags]")
		fmt.Println("\nWith no source, updates every configured package.")
		fmt.Println("A pinned source is left alone: you asked for that exact version.")
		fmt.Println("\ntau updates itself through however you installed it (brew upgrade tau).")
		fmt.Println("\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	source := strings.TrimSpace(strings.Join(fs.Args(), " "))

	env, err := newPackageEnv(&o)
	if err != nil {
		return err
	}
	ctx, cancel := ctxWithSignals()
	defer cancel()

	if source != "" {
		updated, err := env.mgr.Update(ctx, source, o.pkgScope())
		if err != nil {
			return err
		}
		fmt.Println(updateLine(source, updated))
		return nil
	}

	type target struct {
		source string
		scope  pkgmgr.Scope
	}
	var targets []target
	scopes := []struct {
		settings settings.Scope
		pkg      pkgmgr.Scope
	}{{settings.Global, pkgmgr.ScopeUser}, {settings.Project, pkgmgr.ScopeProject}}
	for _, s := range scopes {
		if s.settings == settings.Project && !env.trusted {
			continue
		}
		raw, err := env.rawPackages(s.settings)
		if err != nil {
			return err
		}
		entries, warnings := pkgmgr.ParseEntries(raw)
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "tau: "+w)
		}
		for _, e := range entries {
			targets = append(targets, target{source: e.Source, scope: s.pkg})
		}
	}
	if len(targets) == 0 {
		fmt.Println("No packages configured.")
		return nil
	}

	var failed int
	for _, t := range targets {
		updated, err := env.mgr.Update(ctx, t.source, t.scope)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "tau: %s: %v\n", t.source, err)
			continue
		}
		fmt.Println(updateLine(t.source, updated))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d packages failed to update", failed, len(targets))
	}
	return nil
}

func updateLine(source string, updated bool) string {
	if updated {
		return "Updated " + source
	}
	return "Pinned  " + source + " (left alone)"
}

// packagesCmd is `tau packages`: report what is configured and installed.
func packagesCmd(args []string) error {
	fs := flag.NewFlagSet("tau packages", flag.ContinueOnError)
	var o packageOpts
	o.register(fs)
	fs.Usage = func() {
		fmt.Println("usage: tau packages [flags]")
		fmt.Println("\nLists configured packages from both scopes, then anything installed")
		fmt.Println("on disk that no longer appears in settings.")
		fmt.Println("\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	env, err := newPackageEnv(&o)
	if err != nil {
		return err
	}

	configured := map[string]bool{}
	printed := false
	for _, s := range []struct {
		settings settings.Scope
		pkg      pkgmgr.Scope
		label    string
	}{
		{settings.Global, pkgmgr.ScopeUser, "User packages"},
		{settings.Project, pkgmgr.ScopeProject, "Project packages"},
	} {
		if s.settings == settings.Project && !env.trusted {
			continue
		}
		raw, err := env.rawPackages(s.settings)
		if err != nil {
			return err
		}
		entries, warnings := pkgmgr.ParseEntries(raw)
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "tau: "+w)
		}
		if len(entries) == 0 {
			continue
		}
		if printed {
			fmt.Println()
		}
		printed = true
		fmt.Printf("%s (%s):\n", s.label, env.set.Path(s.settings))
		for _, e := range entries {
			configured[e.Source] = true
			fmt.Printf("  %s%s\n", e.Source, filterNote(e))
			if path, err := env.mgr.PackagePath(pkgmgr.ParseSource(e.Source), s.pkg); err == nil {
				if info, serr := os.Stat(path); serr == nil && info.IsDir() {
					fmt.Printf("      %s\n", path)
				} else {
					fmt.Println("      not installed — run tau install " + e.Source)
				}
			}
		}
	}

	// Matched by identity, not by string: a package configured as npm:pkg and
	// installed as npm:pkg@1.2.3 is the same package, not an orphan.
	known := rawOf(configured)
	var orphans []pkgmgr.Installed
	for _, scope := range []pkgmgr.Scope{pkgmgr.ScopeUser, pkgmgr.ScopeProject} {
		if scope == pkgmgr.ScopeProject && !env.trusted {
			continue
		}
		installed, err := env.mgr.List(scope)
		if err != nil {
			continue
		}
		for _, p := range installed {
			if findPackage(known, p.Source) < 0 {
				orphans = append(orphans, p)
			}
		}
	}
	if len(orphans) > 0 {
		if printed {
			fmt.Println()
		}
		printed = true
		fmt.Println("Installed but not configured:")
		for _, p := range orphans {
			fmt.Printf("  %s (%s)\n      %s\n", p.Source, p.Scope, p.Path)
		}
	}

	if !printed {
		fmt.Println("No packages installed.")
	}
	if !env.trusted {
		fmt.Println("\nProject packages were not read: " + env.reason)
	}
	return nil
}

// filterNote marks an entry that selects only part of what its package ships.
func filterNote(e pkgmgr.Entry) string {
	if e.Autoload != nil && !*e.Autoload {
		return " (opt-in)"
	}
	for _, t := range pkgmgr.ResourceTypes {
		if _, declared := e.Patterns(t); declared {
			return " (filtered)"
		}
	}
	return ""
}

// rawOf re-encodes configured sources so findPackage can match an installed
// package against them by identity rather than by exact string.
func rawOf(configured map[string]bool) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(configured))
	for source := range configured {
		if encoded, err := json.Marshal(source); err == nil {
			out = append(out, encoded)
		}
	}
	return out
}
