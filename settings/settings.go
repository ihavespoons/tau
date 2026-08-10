// Package settings is tau's two-scope configuration layer — the port of Pi's
// settings-manager.ts. Global settings live at ~/.tau/agent/settings.json and
// project settings at <cwd>/.tau/settings.json, with project winning.
//
// Unknown keys are never lost. Reads capture them into Extra and writes are
// field-level merges onto a fresh read of the file, so a key written by Pi or
// by a newer tau survives a round trip untouched.
package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Scope selects which settings file a value comes from or goes to.
type Scope string

const (
	// Global is ~/.tau/agent/settings.json.
	Global Scope = "global"
	// Project is <cwd>/.tau/settings.json, loaded only for trusted projects.
	Project Scope = "project"
)

// QueueMode controls how many queued messages the agent takes per poll.
type QueueMode string

const (
	QueueAll        QueueMode = "all"
	QueueOneAtATime QueueMode = "one-at-a-time"
)

// DefaultProjectTrust is the fallback when no saved trust decision applies.
type DefaultProjectTrust string

const (
	TrustAsk    DefaultProjectTrust = "ask"
	TrustAlways DefaultProjectTrust = "always"
	TrustNever  DefaultProjectTrust = "never"
)

// Compaction bounds automatic context compaction.
type Compaction struct {
	Enabled          *bool `json:"enabled,omitempty"`
	ReserveTokens    *int  `json:"reserveTokens,omitempty"`
	KeepRecentTokens *int  `json:"keepRecentTokens,omitempty"`
}

// BranchSummary configures branch-navigation summaries.
type BranchSummary struct {
	ReserveTokens *int  `json:"reserveTokens,omitempty"`
	SkipPrompt    *bool `json:"skipPrompt,omitempty"`
}

// ProviderRetry are the provider/SDK-level retry knobs.
type ProviderRetry struct {
	TimeoutMs       *int `json:"timeoutMs,omitempty"`
	MaxRetries      *int `json:"maxRetries,omitempty"`
	MaxRetryDelayMs *int `json:"maxRetryDelayMs,omitempty"`
}

// Retry configures tau's own retry loop around provider calls.
type Retry struct {
	Enabled     *bool          `json:"enabled,omitempty"`
	MaxRetries  *int           `json:"maxRetries,omitempty"`
	BaseDelayMs *int           `json:"baseDelayMs,omitempty"`
	Provider    *ProviderRetry `json:"provider,omitempty"`
}

// ThinkingBudgets overrides token budgets per thinking level.
type ThinkingBudgets struct {
	Minimal *int `json:"minimal,omitempty"`
	Low     *int `json:"low,omitempty"`
	Medium  *int `json:"medium,omitempty"`
	High    *int `json:"high,omitempty"`
}

// Terminal holds terminal-rendering preferences.
type Terminal struct {
	ShowImages           *bool `json:"showImages,omitempty"`
	ImageWidthCells      *int  `json:"imageWidthCells,omitempty"`
	ClearOnShrink        *bool `json:"clearOnShrink,omitempty"`
	ShowTerminalProgress *bool `json:"showTerminalProgress,omitempty"`
}

// Images controls image handling before send.
type Images struct {
	AutoResize  *bool `json:"autoResize,omitempty"`
	BlockImages *bool `json:"blockImages,omitempty"`
}

// Markdown holds markdown-rendering preferences.
type Markdown struct {
	CodeBlockIndent *string `json:"codeBlockIndent,omitempty"`
}

// Warnings toggles advisory notices.
type Warnings struct {
	AnthropicExtraUsage *bool `json:"anthropicExtraUsage,omitempty"`
}

// Settings is the typed view of a settings file. Pointer fields distinguish
// "absent" from "set to the zero value", which the merge depends on.
//
// Extra carries every key tau does not know about so writes preserve them.
type Settings struct {
	LastChangelogVersion *string `json:"lastChangelogVersion,omitempty"`
	DefaultProvider      *string `json:"defaultProvider,omitempty"`
	DefaultModel         *string `json:"defaultModel,omitempty"`
	DefaultThinkingLevel *string `json:"defaultThinkingLevel,omitempty"`
	Transport            *string `json:"transport,omitempty"`
	SteeringMode         *string `json:"steeringMode,omitempty"`
	FollowUpMode         *string `json:"followUpMode,omitempty"`
	Theme                *string `json:"theme,omitempty"`

	Compaction    *Compaction    `json:"compaction,omitempty"`
	BranchSummary *BranchSummary `json:"branchSummary,omitempty"`
	Retry         *Retry         `json:"retry,omitempty"`

	HideThinkingBlock    *bool   `json:"hideThinkingBlock,omitempty"`
	ShowCacheMissNotices *bool   `json:"showCacheMissNotices,omitempty"`
	ExternalEditor       *string `json:"externalEditor,omitempty"`
	ShellPath            *string `json:"shellPath,omitempty"`
	QuietStartup         *bool   `json:"quietStartup,omitempty"`

	// DefaultProjectTrust is honored from the global scope only: a project
	// must not be able to escalate its own trust.
	DefaultProjectTrust *DefaultProjectTrust `json:"defaultProjectTrust,omitempty"`

	ShellCommandPrefix *string  `json:"shellCommandPrefix,omitempty"`
	NpmCommand         []string `json:"npmCommand,omitempty"`
	CollapseChangelog  *bool    `json:"collapseChangelog,omitempty"`

	EnableInstallTelemetry *bool   `json:"enableInstallTelemetry,omitempty"`
	EnableAnalytics        *bool   `json:"enableAnalytics,omitempty"`
	TrackingID             *string `json:"trackingId,omitempty"`

	Packages   []json.RawMessage `json:"packages,omitempty"` // string | {source,...}
	Extensions []string          `json:"extensions,omitempty"`
	Skills     []string          `json:"skills,omitempty"`
	Prompts    []string          `json:"prompts,omitempty"`
	Themes     []string          `json:"themes,omitempty"`

	EnableSkillCommands *bool `json:"enableSkillCommands,omitempty"`

	Terminal  *Terminal `json:"terminal,omitempty"`
	ImagesCfg *Images   `json:"images,omitempty"`

	EnabledModels []string `json:"enabledModels,omitempty"`

	DoubleEscapeAction *string `json:"doubleEscapeAction,omitempty"`
	TreeFilterMode     *string `json:"treeFilterMode,omitempty"`

	ThinkingBudgets *ThinkingBudgets `json:"thinkingBudgets,omitempty"`

	EditorPaddingX         *int  `json:"editorPaddingX,omitempty"`
	OutputPad              *int  `json:"outputPad,omitempty"`
	AutocompleteMaxVisible *int  `json:"autocompleteMaxVisible,omitempty"`
	ShowHardwareCursor     *bool `json:"showHardwareCursor,omitempty"`

	Markdown *Markdown `json:"markdown,omitempty"`
	Warnings *Warnings `json:"warnings,omitempty"`

	SessionDir *string `json:"sessionDir,omitempty"`

	HTTPProxy                 *string `json:"httpProxy,omitempty"`
	HTTPIdleTimeoutMs         *int    `json:"httpIdleTimeoutMs,omitempty"`
	WebsocketConnectTimeoutMs *int    `json:"websocketConnectTimeoutMs,omitempty"`

	// Extra holds keys tau does not model, preserved verbatim across writes.
	Extra map[string]json.RawMessage `json:"-"`
}

// knownKeys is the set of JSON keys the typed struct covers. Anything else
// lands in Extra.
var knownKeys = map[string]struct{}{}

func init() {
	for _, k := range []string{
		"lastChangelogVersion", "defaultProvider", "defaultModel", "defaultThinkingLevel",
		"transport", "steeringMode", "followUpMode", "theme",
		"compaction", "branchSummary", "retry",
		"hideThinkingBlock", "showCacheMissNotices", "externalEditor", "shellPath", "quietStartup",
		"defaultProjectTrust", "shellCommandPrefix", "npmCommand", "collapseChangelog",
		"enableInstallTelemetry", "enableAnalytics", "trackingId",
		"packages", "extensions", "skills", "prompts", "themes", "enableSkillCommands",
		"terminal", "images", "enabledModels", "doubleEscapeAction", "treeFilterMode",
		"thinkingBudgets", "editorPaddingX", "outputPad", "autocompleteMaxVisible",
		"showHardwareCursor", "markdown", "warnings", "sessionDir",
		"httpProxy", "httpIdleTimeoutMs", "websocketConnectTimeoutMs",
	} {
		knownKeys[k] = struct{}{}
	}
}

// Keys lists the top-level settings keys tau models, sorted. Other keys are
// accepted and preserved verbatim across writes, but nothing in tau reads
// them — which is exactly what a caller offering completions wants to say.
func Keys() []string {
	out := make([]string, 0, len(knownKeys))
	for k := range knownKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Known reports whether tau models a top-level key.
func Known(key string) bool {
	_, ok := knownKeys[key]
	return ok
}

// UnmarshalJSON decodes known fields and captures the rest into Extra.
func (s *Settings) UnmarshalJSON(data []byte) error {
	type alias Settings
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = Settings(a)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if _, known := knownKeys[k]; known {
			continue
		}
		if s.Extra == nil {
			s.Extra = map[string]json.RawMessage{}
		}
		s.Extra[k] = v
	}
	return nil
}

// MarshalJSON emits known fields plus Extra, with stable key ordering.
func (s Settings) MarshalJSON() ([]byte, error) {
	type alias Settings
	b, err := json.Marshal(alias(s))
	if err != nil {
		return nil, err
	}
	if len(s.Extra) == 0 {
		return b, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range s.Extra {
		if _, known := knownKeys[k]; known {
			continue // a known key always wins over a stale Extra entry
		}
		merged[k] = v
	}
	return marshalStable(merged)
}

// marshalStable renders a map with sorted keys and two-space indent, matching
// Pi's JSON.stringify(x, null, 2).
func marshalStable(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := []byte{'{'}
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, m[k]...)
	}
	out = append(out, '}')
	return out, nil
}

// indent renders JSON with Pi's two-space indent and a trailing newline.
func indent(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return nil, fmt.Errorf("settings: render json: %w", err)
	}
	return buf.Bytes(), nil
}
