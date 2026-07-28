package models

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ihavespoons/tau/ai"
)

// thinkingLevels are the suffixes accepted after a colon in a model spec.
var thinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true,
}

// IsThinkingLevel reports whether s names a thinking level.
func IsThinkingLevel(s string) bool { return thinkingLevels[s] }

// Match is a resolved model plus any thinking level parsed from its spec.
type Match struct {
	Model *ai.Model
	// ThinkingLevel is set only when the spec carried a ":level" suffix.
	ThinkingLevel string
	// Warning describes a recoverable oddity, such as an unrecognized suffix
	// that was treated as part of the model id.
	Warning string
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// isAlias reports whether an id looks like a stable alias rather than a dated
// snapshot (model-resolver.ts:64-71).
func isAlias(id string) bool {
	if strings.HasSuffix(id, "-latest") {
		return true
	}
	return !dateSuffix.MatchString(id)
}

// FindExact resolves an unambiguous reference: canonical "provider/id", then
// provider+id split on the first slash, then a bare id. Every comparison is
// case-insensitive, and an ambiguous match resolves to nothing rather than
// guessing (model-resolver.ts:78-121).
func FindExact(ref string, available []ai.Model) *ai.Model {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	lower := strings.ToLower(ref)

	var canonical []int
	for i := range available {
		full := strings.ToLower(available[i].Provider + "/" + available[i].ID)
		if full == lower {
			canonical = append(canonical, i)
		}
	}
	if len(canonical) == 1 {
		return &available[canonical[0]]
	}
	if len(canonical) > 1 {
		return nil
	}

	if slash := strings.Index(ref, "/"); slash != -1 {
		provider := strings.TrimSpace(ref[:slash])
		modelID := strings.TrimSpace(ref[slash+1:])
		if provider != "" && modelID != "" {
			var hits []int
			for i := range available {
				if strings.EqualFold(available[i].Provider, provider) &&
					strings.EqualFold(available[i].ID, modelID) {
					hits = append(hits, i)
				}
			}
			if len(hits) == 1 {
				return &available[hits[0]]
			}
			if len(hits) > 1 {
				return nil
			}
		}
	}

	var byID []int
	for i := range available {
		if strings.EqualFold(available[i].ID, ref) {
			byID = append(byID, i)
		}
	}
	if len(byID) == 1 {
		return &available[byID[0]]
	}
	return nil
}

// tryMatch resolves a pattern exactly, then by substring on id or name,
// preferring aliases over dated snapshots and the highest-sorting id within
// each group (model-resolver.ts:127-157).
func tryMatch(pattern string, available []ai.Model) *ai.Model {
	if m := FindExact(pattern, available); m != nil {
		return m
	}
	lower := strings.ToLower(pattern)
	var aliases, dated []*ai.Model
	for i := range available {
		m := &available[i]
		if strings.Contains(strings.ToLower(m.ID), lower) ||
			(m.Name != "" && strings.Contains(strings.ToLower(m.Name), lower)) {
			if isAlias(m.ID) {
				aliases = append(aliases, m)
			} else {
				dated = append(dated, m)
			}
		}
	}
	pick := func(group []*ai.Model) *ai.Model {
		sort.Slice(group, func(i, j int) bool { return group[i].ID > group[j].ID })
		return group[0]
	}
	if len(aliases) > 0 {
		return pick(aliases)
	}
	if len(dated) > 0 {
		return pick(dated)
	}
	return nil
}

// ParseSpec resolves a model spec, which may carry a ":level" thinking suffix.
//
// The full spec is matched as a model FIRST, before any colon splitting —
// model ids legitimately contain colons (OpenRouter's ":free" and ":exacto"),
// so splitting eagerly would resolve the wrong model. Only when the whole
// string fails to match is the last colon considered a separator
// (model-resolver.ts:195-248).
//
// strict rejects an unrecognized suffix outright instead of treating it as
// part of the id; Pi uses strict for --model and lenient for scope patterns.
func ParseSpec(spec string, available []ai.Model, strict bool) Match {
	if m := tryMatch(spec, available); m != nil {
		return Match{Model: m}
	}
	idx := strings.LastIndex(spec, ":")
	if idx == -1 {
		return Match{}
	}
	prefix, suffix := spec[:idx], spec[idx+1:]

	if IsThinkingLevel(suffix) {
		inner := ParseSpec(prefix, available, strict)
		if inner.Model == nil {
			return inner
		}
		if inner.Warning != "" {
			// A warning from the inner parse means the level is unreliable.
			return Match{Model: inner.Model, Warning: inner.Warning}
		}
		return Match{Model: inner.Model, ThinkingLevel: suffix}
	}

	if strict {
		return Match{}
	}
	inner := ParseSpec(prefix, available, strict)
	if inner.Model == nil {
		return inner
	}
	return Match{
		Model:   inner.Model,
		Warning: fmt.Sprintf("Invalid thinking level %q in pattern %q. Using default instead.", suffix, spec),
	}
}

// Diagnostic reports a non-fatal problem resolving a scope pattern.
type Diagnostic struct {
	Pattern string
	Message string
}

// Scoped expands cycle-set patterns into models, in pattern order and
// deduplicated. Patterns containing * ? or [ are globbed against both
// "provider/id" and bare "id"; others go through ParseSpec
// (model-resolver.ts:273-352).
func Scoped(patterns []string, available []ai.Model) ([]Match, []Diagnostic) {
	var out []Match
	var diags []Diagnostic

	seen := func(m *ai.Model) bool {
		for _, existing := range out {
			if ai.ModelsEqual(existing.Model, m) {
				return true
			}
		}
		return false
	}

	for _, pattern := range patterns {
		if strings.ContainsAny(pattern, "*?[") {
			globPattern, level := pattern, ""
			if idx := strings.LastIndex(pattern, ":"); idx != -1 {
				if suffix := pattern[idx+1:]; IsThinkingLevel(suffix) {
					level = suffix
					globPattern = pattern[:idx]
				}
			}
			if exact := FindExact(globPattern, available); exact != nil {
				if !seen(exact) {
					out = append(out, Match{Model: exact, ThinkingLevel: level})
				}
				continue
			}
			matched := false
			for i := range available {
				m := &available[i]
				full := m.Provider + "/" + m.ID
				if globMatch(globPattern, full) || globMatch(globPattern, m.ID) {
					matched = true
					if !seen(m) {
						out = append(out, Match{Model: m, ThinkingLevel: level})
					}
				}
			}
			if !matched {
				diags = append(diags, Diagnostic{
					Pattern: pattern,
					Message: fmt.Sprintf("No models match pattern %q", pattern),
				})
			}
			continue
		}

		res := ParseSpec(pattern, available, false)
		if res.Warning != "" {
			diags = append(diags, Diagnostic{Pattern: pattern, Message: res.Warning})
		}
		if res.Model == nil {
			diags = append(diags, Diagnostic{
				Pattern: pattern,
				Message: fmt.Sprintf("No models match pattern %q", pattern),
			})
			continue
		}
		if !seen(res.Model) {
			out = append(out, res)
		}
	}
	return out, diags
}

// globMatch is a case-insensitive glob, matching minimatch's nocase behavior
// for the patterns model scoping uses. path.Match treats "/" as a separator,
// so a leading "*" would not span providers; patterns are matched against both
// the full id and the bare id by the caller to compensate.
func globMatch(pattern, s string) bool {
	pattern, s = strings.ToLower(pattern), strings.ToLower(s)
	if ok, err := path.Match(pattern, s); err == nil && ok {
		return true
	}
	// minimatch's "*" crosses "/" in the patterns used here (e.g. "*sonnet*"
	// against "anthropic/claude-sonnet-5"), which path.Match refuses. Fall
	// back to a segment-agnostic matcher.
	return wildcardMatch(pattern, s)
}

// wildcardMatch implements * ? and [...] where * spans any character.
func wildcardMatch(pattern, s string) bool {
	var match func(p, str int) bool
	match = func(p, str int) bool {
		for p < len(pattern) {
			switch pattern[p] {
			case '*':
				for skip := str; skip <= len(s); skip++ {
					if match(p+1, skip) {
						return true
					}
				}
				return false
			case '?':
				if str >= len(s) {
					return false
				}
				p++
				str++
			case '[':
				end := strings.IndexByte(pattern[p:], ']')
				if end == -1 || str >= len(s) {
					return false
				}
				set := pattern[p+1 : p+end]
				negate := strings.HasPrefix(set, "!") || strings.HasPrefix(set, "^")
				if negate {
					set = set[1:]
				}
				if strings.ContainsRune(set, rune(s[str])) == negate {
					return false
				}
				p += end + 1
				str++
			default:
				if str >= len(s) || pattern[p] != s[str] {
					return false
				}
				p++
				str++
			}
		}
		return str == len(s)
	}
	return match(0, 0)
}
