// Package invisible makes format characters visible on the crew read paths:
// models read these code points while terminals show nothing, so a read prints
// them as `\u{XXXX}` text. The set matches the display cleaners in dash and
// retro.jq, which drop it instead.
package invisible

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Is reports whether r is a zero-width, bidi-control or tag code point.
func Is(r rune) bool {
	switch {
	case r == 0x061c, r == 0x180e,
		r >= 0x200b && r <= 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x2060 && r <= 0x2064,
		r >= 0x2066 && r <= 0x2069,
		r == 0xfeff,
		r >= 0xe0000 && r <= 0xe007f:
		return true
	}
	return false
}

// Escape replaces every invisible code point in s with `\u{XXXX}`. The output
// is ASCII where it changed, so Escape is idempotent.
func Escape(s string) string {
	if !strings.ContainsFunc(s, Is) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if Is(r) {
			fmt.Fprintf(&b, `\u{%04X}`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Value returns a copy of v with every string leaf and map key escaped. Other
// values, json.Number included, pass through. A string that holds a JSON object
// or array is escaped inside, so a consumer that fromjson's the body still can.
func Value(v any) any {
	switch x := v.(type) {
	case string:
		if !strings.ContainsFunc(x, Is) {
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
