package settings

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// harness writes settings files into a temp layout and loads a Manager.
type harness struct {
	t        *testing.T
	agentDir string
	cwd      string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:        t,
		agentDir: filepath.Join(root, "agent"),
		cwd:      filepath.Join(root, "project"),
	}
	if err := os.MkdirAll(h.agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.cwd, ".tau"), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) writeGlobal(body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.agentDir, "settings.json"), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeProject(body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.cwd, ".tau", "settings.json"), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) load(trusted bool) *Manager {
	h.t.Helper()
	m, err := Load(Options{Cwd: h.cwd, AgentDir: h.agentDir, ProjectTrusted: trusted})
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

func (h *harness) readProjectFile() string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.cwd, ".tau", "settings.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestMergeMatrix(t *testing.T) {
	cases := []struct {
		name    string
		global  string
		project string
		check   func(t *testing.T, m *Manager)
	}{
		{
			name:    "global only",
			global:  `{"defaultModel":"claude-opus-5"}`,
			project: ``,
			check: func(t *testing.T, m *Manager) {
				r, _ := m.Resolve()
				if got := r.DefaultModel(); got != "claude-opus-5" {
					t.Errorf("model = %q", got)
				}
				if scope, _ := m.Origin("defaultModel"); scope != Global {
					t.Errorf("origin = %v", scope)
				}
			},
		},
		{
			name:    "project only",
			global:  `{}`,
			project: `{"defaultModel":"claude-haiku-4-5"}`,
			check: func(t *testing.T, m *Manager) {
				r, _ := m.Resolve()
				if got := r.DefaultModel(); got != "claude-haiku-4-5" {
					t.Errorf("model = %q", got)
				}
				if scope, _ := m.Origin("defaultModel"); scope != Project {
					t.Errorf("origin = %v", scope)
				}
			},
		},
		{
			name:    "project wins over global",
			global:  `{"defaultModel":"claude-opus-5","theme":"dark"}`,
			project: `{"defaultModel":"claude-sonnet-5"}`,
			check: func(t *testing.T, m *Manager) {
				r, _ := m.Resolve()
				if got := r.DefaultModel(); got != "claude-sonnet-5" {
					t.Errorf("model = %q", got)
				}
				// An unset project key leaves the global value intact.
				if got := r.Theme(); got != "dark" {
					t.Errorf("theme = %q", got)
				}
			},
		},
		{
			name:    "nested objects merge one level",
			global:  `{"compaction":{"enabled":false,"reserveTokens":9000}}`,
			project: `{"compaction":{"reserveTokens":1234}}`,
			check: func(t *testing.T, m *Manager) {
				r, _ := m.Resolve()
				// reserveTokens overridden, enabled survives from global.
				if got := r.CompactionReserveTokens(); got != 1234 {
					t.Errorf("reserveTokens = %d", got)
				}
				if r.CompactionEnabled() {
					t.Error("enabled should still be false from global")
				}
			},
		},
		{
			name:    "arrays replace wholesale",
			global:  `{"extensions":["a","b","c"]}`,
			project: `{"extensions":["z"]}`,
			check: func(t *testing.T, m *Manager) {
				r, _ := m.Resolve()
				got := r.ExtensionPaths()
				if len(got) != 1 || got[0] != "z" {
					t.Errorf("extensions = %v, want [z] (arrays replace, never concat)", got)
				}
			},
		},
		{
			name:    "object replaces scalar on type mismatch",
			global:  `{"compaction":true}`,
			project: `{"compaction":{"enabled":false}}`,
			check: func(t *testing.T, m *Manager) {
				r, _ := m.Resolve()
				if r.CompactionEnabled() {
					t.Error("project object should replace the global scalar")
				}
			},
		},
		{
			name:    "explicit null replaces",
			global:  `{"theme":"dark"}`,
			project: `{"theme":null}`,
			check: func(t *testing.T, m *Manager) {
				r, _ := m.Resolve()
				if got := r.Theme(); got != "" {
					t.Errorf("theme = %q, want empty after explicit null", got)
				}
			},
		},
		{
			name:    "deeply nested replaces rather than recursing",
			global:  `{"retry":{"enabled":true,"provider":{"timeoutMs":100,"maxRetries":9}}}`,
			project: `{"retry":{"provider":{"timeoutMs":200}}}`,
			check: func(t *testing.T, m *Manager) {
				s, err := m.Settings()
				if err != nil {
					t.Fatal(err)
				}
				// Pi spreads ONE level: retry.provider is replaced whole, so
				// maxRetries from global is gone despite "deep merge" naming.
				if s.Retry == nil || s.Retry.Provider == nil {
					t.Fatal("retry.provider missing")
				}
				if s.Retry.Provider.MaxRetries != nil {
					t.Errorf("maxRetries = %v, want nil: nested merge is one level only",
						*s.Retry.Provider.MaxRetries)
				}
				if s.Retry.Provider.TimeoutMs == nil || *s.Retry.Provider.TimeoutMs != 200 {
					t.Error("timeoutMs should come from project")
				}
				if s.Retry.Enabled == nil || !*s.Retry.Enabled {
					t.Error("retry.enabled should survive from global (one-level spread)")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.global != "" {
				h.writeGlobal(tc.global)
			}
			if tc.project != "" {
				h.writeProject(tc.project)
			}
			tc.check(t, h.load(true))
		})
	}
}

// The whole point of the trust gate: an untrusted project contributes nothing.
func TestUntrustedProjectSettingsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"defaultModel":"claude-opus-5"}`)
	h.writeProject(`{"defaultModel":"evil-model","extensions":["./pwn.ts"]}`)

	m := h.load(false)
	r, err := m.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := r.DefaultModel(); got != "claude-opus-5" {
		t.Errorf("model = %q, want the global value", got)
	}
	if got := r.ExtensionPaths(); len(got) != 0 {
		t.Errorf("extensions = %v, want none from an untrusted project", got)
	}
	if scope, ok := m.Origin("extensions"); ok {
		t.Errorf("extensions resolved from %v; untrusted project must not contribute", scope)
	}
}

func TestUntrustedProjectWriteRefused(t *testing.T) {
	h := newHarness(t)
	m := h.load(false)
	err := m.Set(context.Background(), Project, "defaultModel", "x")
	if err == nil {
		t.Fatal("expected a write to an untrusted project to be refused")
	}
	if !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("err = %v", err)
	}
}

// A project must not be able to authorize itself.
func TestDefaultProjectTrustReadsGlobalScopeOnly(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"defaultProjectTrust":"never"}`)
	h.writeProject(`{"defaultProjectTrust":"always"}`)

	m := h.load(true) // even when trusted, the project value must not win
	if got := m.DefaultProjectTrust(); got != TrustNever {
		t.Errorf("DefaultProjectTrust = %q, want %q: a project must not escalate its own trust",
			got, TrustNever)
	}
}

func TestDefaultProjectTrustFallsBackToAsk(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"defaultProjectTrust":"bogus"}`)
	if got := h.load(true).DefaultProjectTrust(); got != TrustAsk {
		t.Errorf("got %q, want ask for an unrecognized value", got)
	}
}

// Non-negotiable: tau must never drop a key it does not model.
func TestUnknownKeysSurviveWrite(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{
  "defaultModel": "claude-opus-5",
  "aFutureKey": {"nested": [1, 2, 3], "deep": {"x": true}},
  "piOnlyKey": "keep me",
  "numberKey": 42
}`)

	m := h.load(true)
	if err := m.Set(context.Background(), Global, "defaultModel", "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(h.agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}

	if string(got["defaultModel"]) != `"claude-sonnet-5"` {
		t.Errorf("defaultModel = %s", got["defaultModel"])
	}
	for _, key := range []string{"aFutureKey", "piOnlyKey", "numberKey"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("unknown key %q was dropped by a write", key)
		}
	}
	var future map[string]any
	if err := json.Unmarshal(got["aFutureKey"], &future); err != nil {
		t.Fatal(err)
	}
	if deep, ok := future["deep"].(map[string]any); !ok || deep["x"] != true {
		t.Errorf("nested unknown structure was mangled: %v", future)
	}
	if string(got["numberKey"]) != "42" {
		t.Errorf("numberKey = %s", got["numberKey"])
	}
}

func TestUnknownKeysRoundTripThroughStruct(t *testing.T) {
	in := `{"defaultModel":"m","futureThing":{"a":1}}`
	var s Settings
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Extra) != 1 {
		t.Fatalf("Extra = %v", s.Extra)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["futureThing"]; !ok {
		t.Errorf("futureThing lost in round trip: %s", out)
	}
}

func TestSetNestedKeyMergesRatherThanReplaces(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"compaction":{"enabled":false,"reserveTokens":999}}`)

	m := h.load(true)
	if err := m.Set(context.Background(), Global, "compaction.reserveTokens", 4321); err != nil {
		t.Fatal(err)
	}

	r, err := m.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := r.CompactionReserveTokens(); got != 4321 {
		t.Errorf("reserveTokens = %d", got)
	}
	if r.CompactionEnabled() {
		t.Error("sibling key compaction.enabled was clobbered by a nested Set")
	}
}

func TestSetRejectsDeepNesting(t *testing.T) {
	h := newHarness(t)
	m := h.load(true)
	if err := m.Set(context.Background(), Global, "retry.provider.timeoutMs", 5); err == nil {
		t.Error("expected an error for a key nesting deeper than one level")
	}
}

func TestUnsetRemovesKey(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"defaultModel":"m","theme":"dark"}`)
	m := h.load(true)
	if err := m.Unset(context.Background(), Global, "theme"); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Resolve()
	if got := r.Theme(); got != "" {
		t.Errorf("theme = %q after unset", got)
	}
	if got := r.DefaultModel(); got != "m" {
		t.Errorf("defaultModel = %q, should be untouched", got)
	}
}

func TestMalformedJSONDegradesWithError(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"defaultModel": "unterminated`)
	h.writeProject(`{"theme":"dark"}`)

	m := h.load(true)
	errs := m.Errors()
	if len(errs) != 1 || errs[0].Scope != Global {
		t.Fatalf("errors = %v, want one global load error", errs)
	}
	// A broken global file must not take the project scope down with it.
	r, err := m.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Theme(); got != "dark" {
		t.Errorf("theme = %q; a bad global file should not break the project scope", got)
	}
}

func TestConcurrentSetsDoNotLoseKeys(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"preserved":"yes"}`)
	m := h.load(true)

	keys := []string{"defaultModel", "theme", "defaultProvider", "sessionDir"}
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := m.Set(context.Background(), Global, key, "v-"+key); err != nil {
				t.Errorf("Set(%s): %v", key, err)
			}
		}(k)
	}
	wg.Wait()

	b, err := os.ReadFile(filepath.Join(h.agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("concurrent writes produced invalid JSON: %v\n%s", err, b)
	}
	if _, ok := got["preserved"]; !ok {
		t.Error("pre-existing key lost under concurrent writes")
	}
	for _, k := range keys {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q lost under concurrent writes", k)
		}
	}
}

func TestMigrations(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, s Settings, raw map[string]json.RawMessage)
	}{
		{
			name: "queueMode becomes steeringMode",
			in:   `{"queueMode":"all"}`,
			check: func(t *testing.T, s Settings, raw map[string]json.RawMessage) {
				if s.SteeringMode == nil || *s.SteeringMode != "all" {
					t.Errorf("steeringMode = %v", s.SteeringMode)
				}
				if _, ok := raw["queueMode"]; ok {
					t.Error("legacy queueMode should be dropped")
				}
			},
		},
		{
			name: "websockets true becomes transport websocket",
			in:   `{"websockets":true}`,
			check: func(t *testing.T, s Settings, _ map[string]json.RawMessage) {
				if s.Transport == nil || *s.Transport != "websocket" {
					t.Errorf("transport = %v", s.Transport)
				}
			},
		},
		{
			name: "websockets false becomes transport sse",
			in:   `{"websockets":false}`,
			check: func(t *testing.T, s Settings, _ map[string]json.RawMessage) {
				if s.Transport == nil || *s.Transport != "sse" {
					t.Errorf("transport = %v", s.Transport)
				}
			},
		},
		{
			name: "explicit transport wins over legacy websockets",
			in:   `{"websockets":true,"transport":"sse"}`,
			check: func(t *testing.T, s Settings, _ map[string]json.RawMessage) {
				if s.Transport == nil || *s.Transport != "sse" {
					t.Errorf("transport = %v", s.Transport)
				}
			},
		},
		{
			name: "skills object becomes array plus flag",
			in:   `{"skills":{"enableSkillCommands":false,"customDirectories":["./s"]}}`,
			check: func(t *testing.T, s Settings, _ map[string]json.RawMessage) {
				if len(s.Skills) != 1 || s.Skills[0] != "./s" {
					t.Errorf("skills = %v", s.Skills)
				}
				if s.EnableSkillCommands == nil || *s.EnableSkillCommands {
					t.Error("enableSkillCommands should be lifted to the top level as false")
				}
			},
		},
		{
			name: "skills object with no directories drops the key",
			in:   `{"skills":{"enableSkillCommands":true}}`,
			check: func(t *testing.T, s Settings, raw map[string]json.RawMessage) {
				if len(s.Skills) != 0 {
					t.Errorf("skills = %v", s.Skills)
				}
				if _, ok := raw["skills"]; ok {
					t.Error("skills key should be removed when it has no directories")
				}
			},
		},
		{
			name: "retry.maxDelayMs moves under provider",
			in:   `{"retry":{"maxDelayMs":1234}}`,
			check: func(t *testing.T, s Settings, _ map[string]json.RawMessage) {
				if s.Retry == nil || s.Retry.Provider == nil || s.Retry.Provider.MaxRetryDelayMs == nil {
					t.Fatalf("retry = %+v", s.Retry)
				}
				if *s.Retry.Provider.MaxRetryDelayMs != 1234 {
					t.Errorf("maxRetryDelayMs = %d", *s.Retry.Provider.MaxRetryDelayMs)
				}
			},
		},
		{
			name: "existing maxRetryDelayMs is not overwritten",
			in:   `{"retry":{"maxDelayMs":1,"provider":{"maxRetryDelayMs":999}}}`,
			check: func(t *testing.T, s Settings, _ map[string]json.RawMessage) {
				if *s.Retry.Provider.MaxRetryDelayMs != 999 {
					t.Errorf("maxRetryDelayMs = %d, want the existing value kept",
						*s.Retry.Provider.MaxRetryDelayMs)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.writeGlobal(tc.in)
			m := h.load(true)
			s, err := m.Settings()
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, s, m.rawScope(Global))
		})
	}
}

func TestDefaults(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{}`)
	r, err := h.load(true).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !r.CompactionEnabled() {
		t.Error("compaction should default to enabled")
	}
	if got := r.CompactionReserveTokens(); got != DefaultCompactionReserveTokens {
		t.Errorf("reserveTokens = %d", got)
	}
	if got := r.CompactionKeepRecentTokens(); got != DefaultCompactionKeepRecentTokens {
		t.Errorf("keepRecentTokens = %d", got)
	}
	if got := r.SteeringMode(); got != QueueOneAtATime {
		t.Errorf("steeringMode = %q", got)
	}
	if got := r.RetryMaxRetries(); got != DefaultRetryMaxRetries {
		t.Errorf("retries = %d", got)
	}
	if got := r.Transport(); got != DefaultTransport {
		t.Errorf("transport = %q", got)
	}
	if !r.EnableSkillCommands() {
		t.Error("skill commands should default on")
	}
}

// Pi ignores a package-qualified theme name for the built-in theme lookup.
func TestThemeIgnoresPackageQualifiedNames(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{"theme":"some-package/theme"}`)
	r, _ := h.load(true).Resolve()
	if got := r.Theme(); got != "" {
		t.Errorf("theme = %q, want empty for a package-qualified name", got)
	}
}

// A realistic Pi settings.json must parse without loss.
func TestParsesRealisticPiSettings(t *testing.T) {
	h := newHarness(t)
	h.writeGlobal(`{
  "defaultProvider": "anthropic",
  "defaultModel": "claude-opus-4-8",
  "defaultThinkingLevel": "high",
  "theme": "dark",
  "steeringMode": "one-at-a-time",
  "followUpMode": "all",
  "compaction": { "enabled": true, "reserveTokens": 16384, "keepRecentTokens": 20000 },
  "branchSummary": { "reserveTokens": 16384, "skipPrompt": false },
  "retry": {
    "enabled": true,
    "maxRetries": 3,
    "baseDelayMs": 2000,
    "provider": { "timeoutMs": 600000, "maxRetries": 2, "maxRetryDelayMs": 60000 }
  },
  "enabledModels": ["anthropic/*", "openai/gpt-5.5:high"],
  "extensions": ["~/dev/my-ext.ts"],
  "skills": ["~/skills"],
  "prompts": [],
  "themes": [],
  "packages": ["npm:pi-mcp-adapter", { "source": "git:github.com/u/r", "autoload": false }],
  "thinkingBudgets": { "minimal": 1024, "low": 2048, "medium": 8192, "high": 16384 },
  "terminal": { "showImages": true, "imageWidthCells": 60 },
  "images": { "autoResize": true, "blockImages": false },
  "markdown": { "codeBlockIndent": "  " },
  "warnings": { "anthropicExtraUsage": true },
  "httpProxy": "http://localhost:8080",
  "httpIdleTimeoutMs": 0,
  "sessionDir": "~/sessions",
  "defaultProjectTrust": "ask",
  "quietStartup": false,
  "enableSkillCommands": true
}`)

	m := h.load(true)
	if errs := m.Errors(); len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	s, err := m.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultModel == nil || *s.DefaultModel != "claude-opus-4-8" {
		t.Error("defaultModel")
	}
	if len(s.EnabledModels) != 2 {
		t.Errorf("enabledModels = %v", s.EnabledModels)
	}
	if len(s.Packages) != 2 {
		t.Errorf("packages = %d entries", len(s.Packages))
	}
	if s.Retry == nil || s.Retry.Provider == nil || *s.Retry.Provider.TimeoutMs != 600000 {
		t.Error("retry.provider.timeoutMs")
	}
	if s.ThinkingBudgets == nil || *s.ThinkingBudgets.High != 16384 {
		t.Error("thinkingBudgets.high")
	}
	if len(s.Extra) != 0 {
		t.Errorf("unexpected unknown keys, the struct should cover these: %v", keysOf(s.Extra))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestWrittenFileIsIndentedJSON(t *testing.T) {
	h := newHarness(t)
	m := h.load(true)
	if err := m.Set(context.Background(), Project, "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	body := h.readProjectFile()
	if !strings.Contains(body, "\n  \"theme\"") {
		t.Errorf("expected two-space indented JSON, got:\n%s", body)
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("expected a trailing newline")
	}
}
