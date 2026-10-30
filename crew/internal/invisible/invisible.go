// Package invisible makes format characters visible on the crew read paths:
// models read these code points while terminals show nothing, so a read prints
// them as `\u{XXXX}` text and the display cleaners drop them. The policy is
// emoji-safe: zero-width joiners and variation selectors survive where they
// build an emoji (a joiner between emoji, a selector after an emoji base or
// before a keycap) and are hidden anywhere else. Every other listed code point
// (bidi controls, tags, soft hyphen, fillers, invisible operators) is always
// hidden.
package invisible

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	zwj    = 0x200d
	vs15   = 0xfe0e
	vs16   = 0xfe0f
	keycap = 0x20e3
)

// hiddenAlways lists the code points that are hidden in every context.
func hiddenAlways(r rune) bool {
	switch {
	case r == 0x00ad, r == 0x034f, r == 0x061c, r == 0x115f, r == 0x1160, r == 0x180e,
		r == 0x200b, r == 0x200c,
		r >= 0x200e && r <= 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x2060 && r <= 0x206f,
		r == 0x3164,
		r >= 0xfe00 && r <= 0xfe0d,
		r == 0xfeff, r == 0xffa0,
		r >= 0xfff9 && r <= 0xfffb,
		r >= 0x1bca0 && r <= 0x1bca3,
		r >= 0x1d173 && r <= 0x1d17a,
		r >= 0xe0000 && r <= 0xe007f,
		r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return false
}

// emojiBase reports whether r can start or continue an emoji sequence.
func emojiBase(r rune) bool {
	switch {
	case r == 0x00a9, r == 0x00ae, r == 0x203c, r == 0x2049, r == 0x2122, r == 0x2139,
		r >= 0x2194 && r <= 0x21aa,
		r >= 0x231a && r <= 0x23ff,
		r == 0x24c2,
		r >= 0x25aa && r <= 0x25fe,
		r >= 0x2600 && r <= 0x27bf,
		r >= 0x2934 && r <= 0x2935,
		r >= 0x2b05 && r <= 0x2b55,
		r == 0x3030, r == 0x303d, r == 0x3297, r == 0x3299,
		r >= 0x1f000 && r <= 0x1faff:
		return true
	}
	return false
}

func mayHide(r rune) bool {
	return hiddenAlways(r) || r == zwj || r == vs15 || r == vs16
}

// selectorKept reports whether the variation selector at rs[i] belongs to an
// emoji presentation or a keycap.
func selectorKept(rs []rune, i int) bool {
	if i == 0 {
		return false
	}
	prev := rs[i-1]
	if emojiBase(prev) {
		return true
	}
	return (prev >= '0' && prev <= '9' || prev == '#' || prev == '*') &&
		i+1 < len(rs) && rs[i+1] == keycap
}

// hidden decides from the original neighbours, so one pass suffices.
func hidden(rs []rune, i int) bool {
	r := rs[i]
	switch {
	case hiddenAlways(r):
		return true
	case r == vs15, r == vs16:
		return !selectorKept(rs, i)
	case r == zwj:
		if i == 0 || i+1 == len(rs) || !emojiBase(rs[i+1]) {
			return true
		}
		prev := rs[i-1]
		return !emojiBase(prev) && !(prev == vs16 && selectorKept(rs, i-1))
	}
	return false
}

func scan(s string, repl func(*strings.Builder, rune)) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if hidden(rs, i) {
			repl(&b, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Has reports whether Escape would change s.
func Has(s string) bool {
	if !strings.ContainsFunc(s, mayHide) {
		return false
	}
	rs := []rune(s)
	for i := range rs {
		if hidden(rs, i) {
			return true
		}
	}
	return false
}

// Escape replaces every hidden code point in s with `\u{XXXX}`. The output is
// ASCII where it changed and never forms an emoji context, so Escape is
// idempotent.
func Escape(s string) string {
	if !Has(s) {
		return s
	}
	return scan(s, func(b *strings.Builder, r rune) { fmt.Fprintf(b, `\u{%04X}`, r) })
}

// Drop removes every hidden code point from s.
func Drop(s string) string {
	if !Has(s) {
		return s
	}
	return scan(s, func(*strings.Builder, rune) {})
}

// Value returns a copy of v with every string leaf and map key escaped. Other
// values, json.Number included, pass through. A string that holds a JSON object
// or array is escaped inside, so a consumer that fromjson's the body still can.
func Value(v any) any {
	switch x := v.(type) {
	case string:
		if !Has(x) {
			return x
		}
		if inner, ok := escapeJSONText(x); ok {
			return inner
		}
		return Escape(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Value(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, e := range x {
			out[i] = Value(e).(map[string]any)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[Escape(k)] = Value(e)
		}
		return out
	}
	return v
}

func escapeJSONText(s string) (string, bool) {
	t := strings.TrimLeft(s, " \t\r\n")
	if t == "" || (t[0] != '{' && t[0] != '[') || !json.Valid([]byte(s)) {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return "", false
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(Value(v)) != nil {
		return "", false
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}
