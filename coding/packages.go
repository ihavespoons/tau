package coding

import (
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/pkgmgr"
	"github.com/ihavespoons/tau/settings"
)

// packagePaths are the resource files contributed by installed packages,
// grouped by type.
type packagePaths struct {
	byType   map[pkgmgr.ResourceType][]string
	warnings []string
}

func (p packagePaths) get(t pkgmgr.ResourceType) []string { return p.byType[t] }

// loadPackages resolves the packages configured in each settings scope into the
// files they contribute.
//
// Scopes are read separately rather than from the merged view because the
// merge cannot say where an entry came from, and scope decides both where the
// package lives on disk and which copy wins a name collision. Project packages
// come first for that reason: the nearer configuration is the more specific
// one.
//
// An untrusted project contributes nothing. Its packages are code — an
// extension entry point is executed, a skill is injected into the system
// prompt — so they sit behind the same gate as everything else in .tau.
func loadPackages(mgr *settings.Manager, cwd string, trusted bool) packagePaths {
	out := packagePaths{byType: map[pkgmgr.ResourceType][]string{}}
	if mgr == nil {
		return out
	}

	pm := pkgmgr.New(pkgmgr.Options{
		AgentDir: config.AgentDir(), Cwd: cwd, ProjectTrusted: trusted,
	})

	for _, sc := range []struct {
		settings settings.Scope
		pkg      pkgmgr.Scope
	}{
		{settings.Project, pkgmgr.ScopeProject},
		{settings.Global, pkgmgr.ScopeUser},
	} {
		if sc.settings == settings.Project && !trusted {
			continue
		}
		s, err := mgr.Scoped(sc.settings)
		if err != nil {
			out.warnings = append(out.warnings, "packages: "+err.Error())
			continue
		}
		entries, warnings := pkgmgr.ParseEntries(s.Packages)
		out.warnings = append(out.warnings, warnings...)
		if len(entries) == 0 {
			continue
		}
		res := pm.Resolve(entries, sc.pkg)
		out.warnings = append(out.warnings, res.Warnings...)
		for _, t := range pkgmgr.ResourceTypes {
			if enabled := res.Enabled(t); len(enabled) > 0 {
				out.byType[t] = append(out.byType[t], enabled...)
			}
		}
	}
	return out
}
