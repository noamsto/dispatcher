package reap

import (
	"strings"
	"sync"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

var fromJSON = sync.OnceValues(func() (*jqrun.Program, error) {
	return jqrun.CompileAny("_jqfromjson", 0, nil, jqrun.WithJQFromJSON())
})

// parseOne is jq's `fromjson` over encoding/json (see jqrun.WithJQFromJSON):
// exactly one JSON text, numbers as json.Number.
func parseOne(s string) (any, error) {
	prog, err := fromJSON()
	if err != nil {
		return nil, err
	}
	return prog.RunAny(s)
}

// rowLine is v as one compact JSON line, without the trailing newline.
func rowLine(v any) string {
	var b strings.Builder
	_ = jsonval.Encode(&b, v, false)
	return strings.TrimSuffix(b.String(), "\n")
}
