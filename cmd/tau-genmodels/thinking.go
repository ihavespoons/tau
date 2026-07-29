package main

import "github.com/ihavespoons/tau/ai"

// thinkingLevels are the levels tau lets a user select, in ascending order.
// "off" is handled separately because it is expressed differently: a provider
// either names a value for it or cannot express it at all.
var thinkingLevels = []ai.ModelThinkingLevel{
	"minimal", "low", "medium", "high", "xhigh", "max",
}

func strptr(s string) *string { return &s }

// effortThinkingLevelMap converts models.dev's verified effort values into a
// tau thinking-level map.
//
// The map is exhaustive on purpose: every level tau offers gets an entry, and
// a level the model does not support is recorded as an explicit nil rather
// than being left out. Omission and nil mean different things downstream — a
// missing key falls back to sending the level's own name, which for an
// unsupported level is a request the provider rejects.
//
// models.dev's "default" and JSON null values have no tau equivalent and are
// dropped; they name a provider-side default rather than a level a user can
// pick.
func effortThinkingLevelMap(options []reasoningOption) ai.ThinkingLevelMap {
	supported := map[string]bool{}
	sawEffort := false
	for _, opt := range options {
		if opt.Type != "effort" {
			continue
		}
		sawEffort = true
		for _, v := range opt.Values {
			if v != nil {
				supported[*v] = true
			}
		}
	}
	if !sawEffort {
		return nil
	}

	// A model whose only effort values are "default"/null exposes nothing
	// selectable, so it gets no map at all.
	any := supported["none"]
	for _, level := range thinkingLevels {
		if supported[string(level)] {
			any = true
		}
	}
	if !any {
		return nil
	}

	m := ai.ThinkingLevelMap{}
	if supported["none"] {
		m[ai.ThinkingOff] = strptr("none")
	} else {
		m[ai.ThinkingOff] = nil
	}
	for _, level := range thinkingLevels {
		if supported[string(level)] {
			m[level] = strptr(string(level))
		} else {
			m[level] = nil
		}
	}
	return m
}

// mergeThinkingLevelMap layers overrides onto a model's existing map, creating
// one if needed. Later entries win, which is what makes a hand-written
// correction able to contradict models.dev.
func mergeThinkingLevelMap(m *ai.Model, overrides ai.ThinkingLevelMap) {
	if len(overrides) == 0 {
		return
	}
	if m.ThinkingLevelMap == nil {
		m.ThinkingLevelMap = ai.ThinkingLevelMap{}
	}
	for k, v := range overrides {
		m.ThinkingLevelMap[k] = v
	}
}
