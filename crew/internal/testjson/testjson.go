// Package testjson gives tests a key-order-free JSON comparison under the
// value-equal contract (#821): Compact encodes a value with every object's
// members sorted by key, so a gojq fold (alphabetical keys) and a want
// fixture (program order) compare equal on values.
package testjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

// MustParse decodes exactly one JSON value, numbers as json.Number.
func MustParse(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("parse want: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("want exactly one value, got trailing data in %q", s)
	}
	return v
}

// Compact is v compact-encoded with every object's members sorted by key and
// every number reduced to its float64 value, an infinity clamped as
// jsonval.Encode does: want fixtures carry jq's literal text, a gojq fold
// re-formats numbers through float64, and the values are what must agree.
func Compact(v any) string {
	var buf bytes.Buffer
	if err := jsonval.Encode(&buf, numbersAsFloats(v), false); err != nil {
		return "encode error: " + err.Error()
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func numbersAsFloats(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = numbersAsFloats(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = numbersAsFloats(e)
		}
		return out
	}
	return v
}
