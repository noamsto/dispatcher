// Package jsonval reads and prints decoded JSON values the way jq does,
// including the ones encoding/json refuses: NaN as null and ±Inf as
// ±MaxFloat64.
package jsonval

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

// Clamp returns a copy of v with every NaN replaced by nil and every ±Inf by
// ±MaxFloat64. Anything else, json.Number included, is returned as is.
func Clamp(v any) any {
	switch x := v.(type) {
	case float64:
		switch {
		case math.IsNaN(x):
			return nil
		case math.IsInf(x, 1):
			return math.MaxFloat64
		case math.IsInf(x, -1):
			return -math.MaxFloat64
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Clamp(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Clamp(e)
		}
		return out
	}
	return v
}

// Text is jq's text for a computed double.
func Text(x float64) string {
	if math.IsNaN(x) {
		return "null"
	}
	switch {
	case math.IsInf(x, 1):
		x = math.MaxFloat64
	case math.IsInf(x, -1):
		x = -math.MaxFloat64
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

// Encode writes Clamp(v) as JSON plus a newline, without HTML escaping.
func Encode(w io.Writer, v any, indent bool) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(Clamp(v))
}

// Truthy is jq's truthiness: only null and false are false.
func Truthy(v any) bool { return v != nil && v != false }

// RawText is what `jq -r` prints for v: a string as is, a computed double as
// Text, anything else its compact JSON text.
func RawText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return Text(x)
	}
	var buf bytes.Buffer
	if err := Encode(&buf, v, false); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
