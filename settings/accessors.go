package settings

// Defaults, matching Pi's `?? value` fallbacks in settings-manager.ts.
const (
	DefaultCompactionReserveTokens    = 16384
	DefaultCompactionKeepRecentTokens = 20000
	DefaultBranchSummaryReserveTokens = 16384
	DefaultRetryMaxRetries            = 3
	DefaultRetryBaseDelayMs           = 2000
	DefaultProviderMaxRetryDelayMs    = 60000
	DefaultTransport                  = "auto"
)

// Resolved is the merged settings with defaults applied — what callers read.
type Resolved struct {
	s Settings
	m *Manager
}

// Resolve returns the merged settings with typed accessors.
func (m *Manager) Resolve() (*Resolved, error) {
	s, err := m.Settings()
	if err != nil {
		return nil, err
	}
	return &Resolved{s: s, m: m}, nil
}

// Raw exposes the merged settings struct.
func (r *Resolved) Raw() Settings { return r.s }

func str(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

func num(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func boolean(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// DefaultProvider is the provider used when none is specified.
func (r *Resolved) DefaultProvider() string { return str(r.s.DefaultProvider, "") }

// DefaultModel is the model used when none is specified.
func (r *Resolved) DefaultModel() string { return str(r.s.DefaultModel, "") }

// DefaultThinkingLevel is the reasoning level used when none is specified.
func (r *Resolved) DefaultThinkingLevel() string { return str(r.s.DefaultThinkingLevel, "") }

// Transport is the preferred provider transport.
func (r *Resolved) Transport() string { return str(r.s.Transport, DefaultTransport) }

// ThemeSetting is the raw theme setting, which is either a theme name or the
// automatic "<light>/<dark>" form naming one theme per terminal background.
func (r *Resolved) ThemeSetting() string { return str(r.s.Theme, "") }

// Theme is the configured theme name, ignoring package-qualified values
// (Pi returns undefined for names containing "/").
func (r *Resolved) Theme() string {
	t := str(r.s.Theme, "")
	for _, c := range t {
		if c == '/' {
			return ""
		}
	}
	return t
}

// SteeringMode controls steering-queue delivery.
func (r *Resolved) SteeringMode() QueueMode {
	return QueueMode(str(r.s.SteeringMode, string(QueueOneAtATime)))
}

// FollowUpMode controls follow-up-queue delivery.
func (r *Resolved) FollowUpMode() QueueMode {
	return QueueMode(str(r.s.FollowUpMode, string(QueueOneAtATime)))
}

// CompactionEnabled reports whether automatic compaction runs.
func (r *Resolved) CompactionEnabled() bool {
	if r.s.Compaction == nil {
		return true
	}
	return boolean(r.s.Compaction.Enabled, true)
}

// CompactionReserveTokens is the headroom kept for prompt plus response.
func (r *Resolved) CompactionReserveTokens() int {
	if r.s.Compaction == nil {
		return DefaultCompactionReserveTokens
	}
	return num(r.s.Compaction.ReserveTokens, DefaultCompactionReserveTokens)
}

// CompactionKeepRecentTokens is how much recent transcript survives compaction.
func (r *Resolved) CompactionKeepRecentTokens() int {
	if r.s.Compaction == nil {
		return DefaultCompactionKeepRecentTokens
	}
	return num(r.s.Compaction.KeepRecentTokens, DefaultCompactionKeepRecentTokens)
}

// BranchSummaryReserveTokens is the headroom for branch summaries.
func (r *Resolved) BranchSummaryReserveTokens() int {
	if r.s.BranchSummary == nil {
		return DefaultBranchSummaryReserveTokens
	}
	return num(r.s.BranchSummary.ReserveTokens, DefaultBranchSummaryReserveTokens)
}

// BranchSummarySkipPrompt reports whether the summary prompt is skipped.
func (r *Resolved) BranchSummarySkipPrompt() bool {
	if r.s.BranchSummary == nil {
		return false
	}
	return boolean(r.s.BranchSummary.SkipPrompt, false)
}

// RetryEnabled reports whether tau retries failed provider calls.
func (r *Resolved) RetryEnabled() bool {
	if r.s.Retry == nil {
		return true
	}
	return boolean(r.s.Retry.Enabled, true)
}

// RetryMaxRetries is the retry attempt cap.
func (r *Resolved) RetryMaxRetries() int {
	if r.s.Retry == nil {
		return DefaultRetryMaxRetries
	}
	return num(r.s.Retry.MaxRetries, DefaultRetryMaxRetries)
}

// RetryBaseDelayMs is the exponential-backoff base.
func (r *Resolved) RetryBaseDelayMs() int {
	if r.s.Retry == nil {
		return DefaultRetryBaseDelayMs
	}
	return num(r.s.Retry.BaseDelayMs, DefaultRetryBaseDelayMs)
}

// ProviderMaxRetryDelayMs caps a server-requested retry delay.
func (r *Resolved) ProviderMaxRetryDelayMs() int {
	if r.s.Retry == nil || r.s.Retry.Provider == nil {
		return DefaultProviderMaxRetryDelayMs
	}
	return num(r.s.Retry.Provider.MaxRetryDelayMs, DefaultProviderMaxRetryDelayMs)
}

// EnableSkillCommands reports whether skills register as /skill:name commands.
func (r *Resolved) EnableSkillCommands() bool { return boolean(r.s.EnableSkillCommands, true) }

// QuietStartup suppresses startup chrome.
func (r *Resolved) QuietStartup() bool { return boolean(r.s.QuietStartup, false) }

// HideThinkingBlock hides reasoning output in the transcript.
func (r *Resolved) HideThinkingBlock() bool { return boolean(r.s.HideThinkingBlock, false) }

// SessionDir overrides session storage.
func (r *Resolved) SessionDir() string { return str(r.s.SessionDir, "") }

// HTTPProxy is the proxy applied to tau-managed HTTP clients.
func (r *Resolved) HTTPProxy() string { return str(r.s.HTTPProxy, "") }

// EnabledModels are the patterns for the model cycle set.
func (r *Resolved) EnabledModels() []string { return append([]string{}, r.s.EnabledModels...) }

// ExtensionPaths, SkillPaths, PromptPaths, ThemePaths are configured resource
// locations, merged across scopes.
func (r *Resolved) ExtensionPaths() []string { return append([]string{}, r.s.Extensions...) }
func (r *Resolved) SkillPaths() []string     { return append([]string{}, r.s.Skills...) }
func (r *Resolved) PromptPaths() []string    { return append([]string{}, r.s.Prompts...) }
func (r *Resolved) ThemePaths() []string     { return append([]string{}, r.s.Themes...) }

// DefaultProjectTrust reads the trust fallback from the GLOBAL scope only.
//
// This is load-bearing for security: honoring it from the merged view would
// let an untrusted project set defaultProjectTrust:"always" in its own
// .tau/settings.json and thereby authorize itself. Pi does the same
// (settings-manager.ts:899-902 reads this.globalSettings, not this.settings).
func (m *Manager) DefaultProjectTrust() DefaultProjectTrust {
	g, err := m.Scoped(Global)
	if err != nil || g.DefaultProjectTrust == nil {
		return TrustAsk
	}
	switch *g.DefaultProjectTrust {
	case TrustAlways, TrustNever:
		return *g.DefaultProjectTrust
	default:
		return TrustAsk
	}
}
