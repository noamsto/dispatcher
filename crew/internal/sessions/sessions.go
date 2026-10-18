// Package sessions folds a branch's bus rows into one entry per session, oldest
// first. The fold is the `_sessions` jq program, ported verbatim from the bash
// library and run through gojq: under the value-equal contract (#821) the jq
// program is the clearest expression of the fold, and sessions.jq is its only
// source.
package sessions

import (
	_ "embed"

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
