// Package partialjson salvages tool-call arguments from incomplete or
// malformed JSON produced during streaming. It is the tau port of Pi's
// utils/json-parse.ts (repairJson + parseStreamingJson) plus the subset of the
// partial-json library semantics Pi relies on.
package partialjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

var validEscapes = map[byte]bool{'"': true, '\\': true, '/': true, 'b': true, 'f': true, 'n': true, 'r': true, 't': true, 'u': true}

// Repair fixes malformed JSON string literals by escaping raw control
// characters inside strings and doubling backslashes before invalid escapes.
// Verbatim port of Pi's repairJson.
func Repair(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			b.WriteByte(c)
			if c == '"' {
				inString = true
			}
			continue
		}
		if c == '"' {
			b.WriteByte(c)
			inString = false
			continue
		}
		if c == '\\' {
			if i+1 >= len(s) {
				b.WriteString(`\\`)
				continue
			}
			next := s[i+1]
			if next == 'u' {
				if i+6 <= len(s) && isHex4(s[i+2:i+6]) {
					b.WriteString(s[i : i+6])
					i += 5
					continue
				}
			}
			if validEscapes[next] && next != 'u' {
				b.WriteByte('\\')
				b.WriteByte(next)
				i++
				continue
			}
			if next == 'u' {
				// \u without 4 hex digits: double the backslash.
				b.WriteString(`\\`)
				continue
			}
			b.WriteString(`\\`)
			continue
		}
		if c <= 0x1f {
			b.WriteString(escapeControl(c))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

func escapeControl(c byte) string {
	switch c {
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	default:
		return fmt.Sprintf(`\u%04x`, c)
	}
}

// ParseWithRepair parses strict JSON, retrying once with Repair applied.
func ParseWithRepair(s string) (any, error) {
	var v any
	if err := unmarshal(s, &v); err != nil {
		repaired := Repair(s)
		if repaired != s {
			var rv any
			if rerr := unmarshal(repaired, &rv); rerr == nil {
				return rv, nil
			}
		}
		return nil, err
	}
	return v, nil
}

func unmarshal(s string, v *any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Reject trailing non-whitespace, like JSON.parse.
	if dec.More() {
		return errors.New("partialjson: trailing data")
	}
	return nil
}

// ParseStreaming attempts to parse potentially incomplete JSON during
// streaming. It always returns a usable map, even for garbage input: strict
// parse → strict parse of repaired input → partial parse → partial parse of
// repaired input → empty map. Non-object results yield an empty map (tau's
// tool arguments are always objects).
func ParseStreaming(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	if v, err := ParseWithRepair(s); err == nil {
		return asObject(v)
	}
	if v, err := parsePartial(s); err == nil {
		return asObject(v)
	}
	if v, err := parsePartial(Repair(s)); err == nil {
		return asObject(v)
	}
	return map[string]any{}
}

func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// errIncomplete signals a value cut off by end-of-input that cannot yield a
// partial result (e.g. `tru`, a bare `-`, an unterminated key).
var errIncomplete = errors.New("partialjson: incomplete value")

// parsePartial parses JSON that may be truncated mid-value, returning the
// longest sensible prefix interpretation (partial-json library semantics):
// unterminated strings yield their content so far, arrays/objects yield the
// elements parsed so far, truncated numbers yield their valid prefix, and
// truncated literals (tru, fals, nul) are dropped.
func parsePartial(s string) (any, error) {
	p := &parser{s: s}
	p.ws()
	v, err := p.value()
	if err != nil && !errors.Is(err, errIncomplete) {
		return nil, err
	}
	if errors.Is(err, errIncomplete) {
		return nil, err
	}
	p.ws()
	if p.i < len(p.s) {
		return nil, errors.New("partialjson: trailing data")
	}
	return v, nil
}

type parser struct {
	s string
	i int
}

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) eof() bool { return p.i >= len(p.s) }

// value parses one JSON value. On truncation it returns the partial value if
// the type supports it, else errIncomplete.
func (p *parser) value() (any, error) {
	if p.eof() {
		return nil, errIncomplete
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		v, _, err := p.str()
		return v, err
	case c == 't':
		return p.literal("true", true)
	case c == 'f':
		return p.literal("false", false)
	case c == 'n':
		return p.literal("null", nil)
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, fmt.Errorf("partialjson: unexpected character %q at %d", c, p.i)
	}
}

func (p *parser) literal(word string, val any) (any, error) {
	rest := p.s[p.i:]
	if strings.HasPrefix(rest, word) {
		p.i += len(word)
		return val, nil
	}
	if strings.HasPrefix(word, rest) {
		// Truncated literal: consume to end, cannot produce a partial atom.
		p.i = len(p.s)
		return nil, errIncomplete
	}
	return nil, fmt.Errorf("partialjson: invalid literal at %d", p.i)
}

// str parses a string. Returns (value, complete, error). A truncated string
// (no closing quote, or cut mid-escape) yields its decoded prefix with
// complete=false and no error — strings support partial results.
func (p *parser) str() (string, bool, error) {
	p.i++ // opening quote
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return b.String(), true, nil
		}
		if c == '\\' {
			if p.i+1 >= len(p.s) {
				p.i = len(p.s) // truncated mid-escape: drop it
				return b.String(), false, nil
			}
			esc := p.s[p.i+1]
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
				p.i += 2
			case 'b':
				b.WriteByte('\b')
				p.i += 2
			case 'f':
				b.WriteByte('\f')
				p.i += 2
			case 'n':
				b.WriteByte('\n')
				p.i += 2
			case 'r':
				b.WriteByte('\r')
				p.i += 2
			case 't':
				b.WriteByte('\t')
				p.i += 2
			case 'u':
				if p.i+6 > len(p.s) {
					if isHexPrefix(p.s[p.i+2:]) {
						p.i = len(p.s) // truncated \uXX…: drop it
						return b.String(), false, nil
					}
					return "", false, fmt.Errorf("partialjson: invalid unicode escape at %d", p.i)
				}
				hex := p.s[p.i+2 : p.i+6]
				if !isHex4(hex) {
					return "", false, fmt.Errorf("partialjson: invalid unicode escape at %d", p.i)
				}
				n, _ := strconv.ParseUint(hex, 16, 32)
				r := rune(n)
				if utf16.IsSurrogate(r) && p.i+12 <= len(p.s) && p.s[p.i+6] == '\\' && p.s[p.i+7] == 'u' && isHex4(p.s[p.i+8:p.i+12]) {
					n2, _ := strconv.ParseUint(p.s[p.i+8:p.i+12], 16, 32)
					if dec := utf16.DecodeRune(r, rune(n2)); dec != 0xFFFD {
						b.WriteRune(dec)
						p.i += 12
						continue
					}
				}
				b.WriteRune(r)
				p.i += 6
			default:
				return "", false, fmt.Errorf("partialjson: invalid escape %q at %d", esc, p.i)
			}
			continue
		}
		if c <= 0x1f {
			return "", false, fmt.Errorf("partialjson: raw control character at %d", p.i)
		}
		b.WriteByte(c)
		p.i++
	}
	return b.String(), false, nil // unterminated string
}

func isHexPrefix(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

func (p *parser) number() (any, error) {
	start := p.i
	for p.i < len(p.s) && strings.ContainsRune("-+.eE0123456789", rune(p.s[p.i])) {
		p.i++
	}
	tok := p.s[start:p.i]
	truncatedAtEOF := p.eof()
	// Longest valid prefix.
	for len(tok) > 0 {
		if f, err := strconv.ParseFloat(tok, 64); err == nil && isStrictJSONNumber(tok) {
			return f, nil
		}
		if !truncatedAtEOF {
			return nil, fmt.Errorf("partialjson: invalid number at %d", start)
		}
		tok = tok[:len(tok)-1]
	}
	if truncatedAtEOF {
		return nil, errIncomplete // e.g. a bare "-"
	}
	return nil, fmt.Errorf("partialjson: invalid number at %d", start)
}

func isStrictJSONNumber(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

func (p *parser) array() (any, error) {
	p.i++ // [
	out := []any{}
	for {
		p.ws()
		if p.eof() {
			return out, nil // truncated array: keep parsed elements
		}
		if p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		v, err := p.value()
		if err != nil {
			if errors.Is(err, errIncomplete) {
				return out, nil // trailing element unproducible: drop it
			}
			return nil, err
		}
		// A partial trailing value is included; detect truncation by EOF.
		out = append(out, v)
		p.ws()
		if p.eof() {
			return out, nil
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, nil
		default:
			return nil, fmt.Errorf("partialjson: expected ',' or ']' at %d", p.i)
		}
	}
}

func (p *parser) object() (any, error) {
	p.i++ // {
	out := map[string]any{}
	for {
		p.ws()
		if p.eof() {
			return out, nil
		}
		if p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		if p.s[p.i] != '"' {
			return nil, fmt.Errorf("partialjson: expected object key at %d", p.i)
		}
		key, complete, err := p.str()
		if err != nil {
			return nil, err
		}
		if !complete {
			return out, nil // truncated key: drop the pair
		}
		p.ws()
		if p.eof() {
			return out, nil // key but no ':' yet
		}
		if p.s[p.i] != ':' {
			return nil, fmt.Errorf("partialjson: expected ':' at %d", p.i)
		}
		p.i++
		p.ws()
		if p.eof() {
			return out, nil // no value yet: drop the pair
		}
		v, err := p.value()
		if err != nil {
			if errors.Is(err, errIncomplete) {
				return out, nil // unproducible partial value: drop the pair
			}
			return nil, err
		}
		out[key] = v
		p.ws()
		if p.eof() {
			return out, nil
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return out, nil
		default:
			return nil, fmt.Errorf("partialjson: expected ',' or '}' at %d", p.i)
		}
	}
}
