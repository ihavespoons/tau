package ai

import (
	"reflect"
	"testing"
)

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

func TestCalculateCostBasic(t *testing.T) {
	m := &Model{Cost: ModelCost{ModelCostRates: ModelCostRates{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}}
	u := &Usage{Input: 1_000_000, Output: 2_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000}
	c := CalculateCost(m, u)
	if c.Input != 3 || c.Output != 30 || c.CacheRead != 0.3 || c.CacheWrite != 3.75 {
		t.Errorf("cost = %+v", c)
	}
	if c.Total != 3+30+0.3+3.75 {
		t.Errorf("total = %v", c.Total)
	}
}

func TestCalculateCostTiersPickHighestMatching(t *testing.T) {
	m := &Model{Cost: ModelCost{
		ModelCostRates: ModelCostRates{Input: 1, Output: 2},
		Tiers: []ModelCostTier{
			{ModelCostRates: ModelCostRates{Input: 2, Output: 4}, InputTokensAbove: 100_000},
			{ModelCostRates: ModelCostRates{Input: 3, Output: 6}, InputTokensAbove: 200_000},
		},
	}}
	// 150k input+cache → first tier only.
	u := &Usage{Input: 150_000}
	CalculateCost(m, u)
	if u.Cost.Input != 2.0/1e6*150_000 {
		t.Errorf("tier1 input cost = %v", u.Cost.Input)
	}
	// 250k → highest tier; the tier applies to the WHOLE request.
	u = &Usage{Input: 250_000}
	CalculateCost(m, u)
	if u.Cost.Input != 3.0/1e6*250_000 {
		t.Errorf("tier2 input cost = %v", u.Cost.Input)
	}
}

func TestCalculateCost1hCacheWriteDoubleBilled(t *testing.T) {
	m := &Model{Cost: ModelCost{ModelCostRates: ModelCostRates{Input: 3, CacheWrite: 3.75}}}
	u := &Usage{CacheWrite: 1000, CacheWrite1h: intp(400)}
	CalculateCost(m, u)
	want := (3.75*600 + 3*2*400) / 1e6
	if u.Cost.CacheWrite != want {
		t.Errorf("cacheWrite cost = %v want %v", u.Cost.CacheWrite, want)
	}
}

func TestSupportedThinkingLevels(t *testing.T) {
	nonReasoning := &Model{Reasoning: false}
	if got := SupportedThinkingLevels(nonReasoning); !reflect.DeepEqual(got, []ModelThinkingLevel{"off"}) {
		t.Errorf("non-reasoning = %v", got)
	}

	// Reasoning model, no map: off..high (xhigh/max need explicit entries).
	plain := &Model{Reasoning: true}
	want := []ModelThinkingLevel{"off", "minimal", "low", "medium", "high"}
	if got := SupportedThinkingLevels(plain); !reflect.DeepEqual(got, want) {
		t.Errorf("plain = %v", got)
	}

	// Map with xhigh present and medium disabled (null).
	mapped := &Model{Reasoning: true, ThinkingLevelMap: ThinkingLevelMap{
		"xhigh":  strp("xhigh"),
		"medium": nil,
	}}
	want = []ModelThinkingLevel{"off", "minimal", "low", "high", "xhigh"}
	if got := SupportedThinkingLevels(mapped); !reflect.DeepEqual(got, want) {
		t.Errorf("mapped = %v", got)
	}
}

func TestClampThinkingLevel(t *testing.T) {
	m := &Model{Reasoning: true, ThinkingLevelMap: ThinkingLevelMap{"medium": nil}}
	// medium unsupported → clamps up to high.
	if got := ClampThinkingLevel(m, "medium"); got != "high" {
		t.Errorf("clamp medium = %v", got)
	}
	// max unsupported (no entry) → clamps down from max… nothing above, walk down to high.
	if got := ClampThinkingLevel(m, "max"); got != "high" {
		t.Errorf("clamp max = %v", got)
	}
	// Non-reasoning: everything clamps to off.
	if got := ClampThinkingLevel(&Model{}, "high"); got != "off" {
		t.Errorf("clamp non-reasoning = %v", got)
	}
}

func TestThinkingLevelMapJSONNull(t *testing.T) {
	var m Model
	if err := unmarshalStrict(`{"id":"x","name":"x","api":"a","provider":"p","baseUrl":"","reasoning":true,"thinkingLevelMap":{"medium":null,"xhigh":"64000"},"input":["text"],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":1,"maxTokens":1}`, &m); err != nil {
		t.Fatal(err)
	}
	v, present := m.ThinkingLevelMap["medium"]
	if !present || v != nil {
		t.Errorf("medium: present=%v v=%v", present, v)
	}
	if v := m.ThinkingLevelMap["xhigh"]; v == nil || *v != "64000" {
		t.Errorf("xhigh = %v", v)
	}
}
