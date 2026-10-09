// Package sessions folds a branch's bus rows into one entry per session,
// oldest first. The fold is the original `_sessions` jq program from
// adapters/core/crew.sh, run through gojq: under the value-equal contract
// (#821) the jq program is the clearest expression of the fold, and the
// program file (sessions.jq) keeps one shared source with crew.sh.
package sessions

import (
	_ "embed"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed sessions.jq
var program string

// Fold is `_sessions <branch> <crew>`: one row per session of branch, oldest
// first, with age_s computed against nowSec (jq's `now`). A jq runtime error
// is returned as an error, which main maps to the jq-failure exit status.
func Fold(events []jsonv.Value, branch, crew string, nowSec float64) (jsonv.Value, error) {
	return jqrun.Run(program, events, nowSec, map[string]jsonv.Value{
		"b":    jsonv.Str(branch),
		"crew": jsonv.Str(crew),
	})
}
