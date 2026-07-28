package partialjson

import (
	"reflect"
	"testing"
)

func TestRepairControlChars(t *testing.T) {
	got := Repair("{\"a\":\"line1\nline2\ttab\"}")
	want := `{"a":"line1\nline2\ttab"}`
	if got != want {
		t.Errorf("Repair = %q want %q", got, want)
	}
}

func TestRepairInvalidEscape(t *testing.T) {
	// \q is not a valid escape: backslash gets doubled.
	got := Repair(`{"a":"c:\qux"}`)
	want := `{"a":"c:\\qux"}`
	if got != want {
		t.Errorf("Repair = %q want %q", got, want)
	}
}

func TestRepairValidEscapesUntouched(t *testing.T) {
	in := `{"a":"x\n\t\u00e9\"done\""}`
	if got := Repair(in); got != in {
		t.Errorf("Repair changed valid input: %q", got)
	}
}

func TestRepairTruncatedUnicodeEscape(t *testing.T) {
	got := Repair(`{"a":"\u12`)
	want := `{"a":"\\u12`
	if got != want {
		t.Errorf("Repair = %q want %q", got, want)
	}
}

func TestParseWithRepairStrictFirst(t *testing.T) {
	v, err := ParseWithRepair(`{"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v, map[string]any{"a": float64(1)}) {
		t.Errorf("v = %#v", v)
	}
}

func TestParseStreamingVectors(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]any
	}{
		{"", map[string]any{}},
		{"   ", map[string]any{}},
		{`{`, map[string]any{}},
		{`{"`, map[string]any{}},
		{`{"comm`, map[string]any{}},
		{`{"command"`, map[string]any{}},
		{`{"command":`, map[string]any{}},
		{`{"command": "ls`, map[string]any{"command": "ls"}},
		{`{"command": "ls -la"`, map[string]any{"command": "ls -la"}},
		{`{"command": "ls", "timeout": 12`, map[string]any{"command": "ls", "timeout": float64(12)}},
		{`{"command": "ls", "timeout":`, map[string]any{"command": "ls"}},
		{`{"a": tru`, map[string]any{}}, // truncated literal: pair dropped
		{`{"a": true`, map[string]any{"a": true}},
		{`{"a": [1, 2`, map[string]any{"a": []any{1.0, 2.0}}},
		{`{"a": [1, 2]}`, map[string]any{"a": []any{1.0, 2.0}}},
		{`{"a": {"b": "c`, map[string]any{"a": map[string]any{"b": "c"}}},
		{`{"a": -`, map[string]any{}},                   // bare minus: dropped
		{`{"a": 12.`, map[string]any{"a": float64(12)}}, // truncated number: valid prefix
		{`{"a": 1e`, map[string]any{"a": float64(1)}},   // truncated exponent
		{`{"path":"C:\qux"}`, map[string]any{"path": `C:\qux`}}, // invalid escape: repaired
		{"{\"a\":\"x\ny\"}", map[string]any{"a": "x\ny"}}, // raw newline in string: repaired
		{`{"s":"caf\u00e9"}`, map[string]any{"s": "caf\u00e9"}},
		{`{"s":"\ud83d\ude00"}`, map[string]any{"s": "\U0001F600"}}, // surrogate pair
		{`{"s":"\ud83d`, map[string]any{"s": "\uFFFD"}},             // lone high surrogate → replacement, unterminated
		{`total garbage`, map[string]any{}},
		{`[1,2,3]`, map[string]any{}}, // non-object → empty
	}
	for _, c := range cases {
		got := ParseStreaming(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseStreaming(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestParsePartialTopLevelArray(t *testing.T) {
	v, err := parsePartial(`[1, "two", {"three": 3`)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{1.0, "two", map[string]any{"three": 3.0}}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("v = %#v", v)
	}
}
