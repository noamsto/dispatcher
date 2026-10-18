// Package retro is `crew retro`: the read-only rollup of the retro notes workers
// and the dispatcher post to the synthetic `retro:`/`metrics:` sinks. Notes are
// cross-run evidence, so the fold reads the whole bus with no crew filter —
// which is also why a clean run prints nothing at all.
//
// The fold is the original jq program from adapters/core/crew.sh (retro.jq), run
// through gojq, so it stays one shared source with the arm it replaces. It
// renders three shapes from one run: the per-run TSV rows, the padded --report
// table, and the --report --json object, pretty-printed and never coloured, since
// that output is compared by value.
package retro

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

//go:embed retro.jq
var program string

const (
	// header is the arm's printf, printed before jq starts and only without --report.
	header = "branch\tengine\tmodel\ttier\toutcome\ttags"
	// Usage is one line for two refusals: a flag outside the grammar and the
	// `--json` without `--report` test print it and take the same status.
	Usage       = "crew: retro takes --report and --json"
	exitType    = 5
	exitFailure = 1
)

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Request is `crew retro`'s two flags.
type Request struct {
	Report bool
	JSON   bool
}

// Run is the arm: the `--json` without `--report` test first (so a bad argument
// costs nothing but its own line), then the arm's `[ -f "$log" ]`, then the
// header, then the fold.
func Run(req Request, paths bus.Paths, stdout, stderr io.Writer) int {
	report, jsonOut := req.Report, req.JSON
	if jsonOut && !report {
		say(stderr, "%s\n", Usage)
		return exitFailure
	}

	// `jq -s` has no torn-tail tolerance: a break anywhere costs every row, so
	// this is the strict read. With no log the arm exits before its header, and
	// before jq starts at all.
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return 0
	}
	if !report {
		say(stdout, "%s\n", header)
	}
	if err != nil {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: retro: %s: %s\n", paths.Log, msg)
		return code
	}

	docs := make([]any, len(events))
	for i, ev := range events {
		docs[i] = ev.Doc
	}
	out, err := jqrun.RunAny(program, docs, 0, map[string]any{
		"want_report": report,
		"want_json":   jsonOut,
	})
	if err != nil {
		say(stderr, "crew: retro: %s: %v\n", paths.Log, err)
		return exitType
	}

	// --report --json is the arm's only non-string output, so the only shape jq
	// pretty-prints. The other two are strings the fold already joined: `jq -r`
	// printed them plus one newline, and the empty string — zero rows, never a
	// row without its tabs — printed nothing.
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			say(stderr, "crew: retro: %v\n", err)
			return exitFailure
		}
		return 0
	}
	if s, _ := out.(string); s != "" {
		say(stdout, "%s\n", s)
	}
	return 0
}
