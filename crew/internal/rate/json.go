package rate

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
)

// decodeAll reads every JSON value from r. A torn or garbage stream returns
// an error so the caller can fold it to its empty value, like `jq -s … || true`.
func decodeAll(r io.Reader) ([]any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var out []any
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// parseOne decodes s as exactly one JSON value; trailing whitespace is fine.
func parseOne(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// clamp maps the floats encoding/json rejects the way jq prints them: NaN
// becomes null and ±Inf the largest finite double.
func clamp(v any) any {
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
			out[i] = clamp(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = clamp(e)
		}
		return out
	}
	return v
}

// encode writes v as JSON plus a newline, without HTML escaping.
func encode(w io.Writer, v any, indent bool) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(clamp(v))
}

func field(v any, key string) any {
	m, _ := v.(map[string]any)
	return m[key]
}

// isTruthy follows jq: only null and false are falsy.
func isTruthy(v any) bool {
	b, ok := v.(bool)
	return v != nil && (!ok || b)
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}
