package coding

import (
	"os"

	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/settings"
	"github.com/ihavespoons/tau/trust"
)

// resolveTrust decides whether project-scoped resources (.tau/settings.json,
// .tau/skills, .tau/extensions) may load for this directory.
//
// The default comes from GLOBAL settings only. Reading it from the merged
// view would let an untrusted project authorize itself by writing
// "defaultProjectTrust": "always" into its own .tau/settings.json.
//
// Without a UI to prompt with, an undecided project is denied: trust fails
// closed.
func resolveTrust(cwd string, hasUI bool, override *bool) trust.Outcome {
	agentDir := config.AgentDir()

	def := trust.Ask
	if mgr, err := settings.Load(settings.Options{Cwd: cwd, AgentDir: agentDir}); err == nil {
		switch mgr.DefaultProjectTrust() {
		case settings.TrustAlways:
			def = trust.Always
		case settings.TrustNever:
			def = trust.Never
		}
	}

	home, _ := os.UserHomeDir()
	outcome, err := trust.Decide(trust.NewStore(agentDir), trust.Request{
		Cwd:           cwd,
		Override:      override,
		Default:       def,
		HasUI:         hasUI,
		ConfigDirName: config.DirName,
		HomeDir:       home,
	})
	if err != nil {
		// A broken trust store must not silently grant trust.
		return trust.Outcome{Trusted: false, Reason: "trust store unreadable: " + err.Error()}
	}
	return outcome
}
