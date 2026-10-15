// Package sessions folds a branch's bus rows into one entry per session, oldest
// first. The fold is the `_sessions` jq program, ported verbatim from the bash
// library and run through gojq: under the value-equal contract (#821) the jq
// program is the clearest expression of the fold, and sessions.jq is its only
// source.
package sessions

import (
	_ "embed"
	"encoding/json"
	"math"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

//go:embed sessions.jq
var program string

// Fold is `_sessions <branch> <crew>`: one row per session of branch, oldest
// first, with age_s computed against nowSec (jq's `now`). events and the result
// are in gojq's form (see jqrun.RunAny). A jq runtime error is returned as an
// error, which main maps to the jq-failure exit status.
func Fold(events []any, branch, crew string, nowSec float64) (any, error) {
	return jqrun.RunAny(program, events, nowSec, map[string]any{
		"b":    branch,
		"crew": crew,
	})
}

// JSON is Fold's result as compact JSON for a caller that still holds jsonv
// values. encoding/json refuses the non-finite float64s gojq computes from a
// corrupt ts, so they print as jq prints them: NaN as null, ±Inf as ±MaxFloat64.
func JSON(v any) ([]byte, error) {
	return json.Marshal(finite(v))
}

func finite(v any) any {
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
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = finite(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = finite(e)
		}
		return out
	}
	return v
}
