package rate

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonval"
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

// encode writes v as JSON plus a newline, without HTML escaping.
func encode(w io.Writer, v any, indent bool) error {
	return jsonval.Encode(w, v, indent)
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
