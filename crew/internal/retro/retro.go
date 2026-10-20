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
//
// `--draft` is the one shape the fold never had: it reads one crew and prints the
// session_summary note for it, folding in Go (draft.go) because there is no arm
// program to share. Its codenames come off `crew roster`'s own rows.
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

// The --draft refusals, one line each like Usage.
const (
	DraftUsage      = "crew: retro --draft takes no --report/--json"
	CrewUsage       = "crew: retro --crew/--all need --draft"
	CrewNeededUsage = "crew: retro --draft needs --crew ID or CREW_ID"
)

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Request is `crew retro`'s flags: the fold's two, and the three --draft reads
// one crew with.
type Request struct {
	Report bool
	JSON   bool
	Draft  bool
	All    bool
	Crew   string
}

// Options is everything Run reads beyond the bus.
type Options struct {
	// CrewID is the crew --draft defaults to: the checkout's task doc, else
	// $CREW_ID. It reads "" when neither is set, which --draft refuses.
	CrewID func() string
	// Roster is `crew roster`'s fold for a crew: --draft reads each branch's
	// codename off its rows, instead of re-deriving here the `·<issue>` suffix
	// roster adds when two branches hash to one codename. Only --draft calls it.
	Roster func(docs []any, crew string) []map[string]any
}

func (o Options) withDefaults() Options {
	if o.CrewID == nil {
		o.CrewID = func() string { return "" }
	}
	if o.Roster == nil {
		o.Roster = func([]any, string) []map[string]any { return nil }
	}
	return o
}

// refusal is why the line is a usage error, or "" when it runs.
func (r Request) refusal() string {
	switch {
	case r.Draft && (r.Report || r.JSON):
		return DraftUsage
	case !r.Draft && (r.Crew != "" || r.All):
		return CrewUsage
	case r.JSON && !r.Report:
		return Usage
	}
	return ""
}

// Run is the arm: the `--json` without `--report` test first (so a bad argument
// costs nothing but its own line), then the arm's `[ -f "$log" ]`, then the
// header, then the fold. --draft answers the same read with one note instead
// (draft.go).
func Run(req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	if why := req.refusal(); why != "" {
		say(stderr, "%s\n", why)
		return exitFailure
	}
	crew := req.Crew
	if crew == "" {
		crew = o.CrewID()
	}
	if req.Draft && crew == "" {
		say(stderr, "%s\n", CrewNeededUsage)
		return exitFailure
	}

	report, jsonOut := req.Report, req.JSON

	// `jq -s` has no torn-tail tolerance: a break anywhere costs every row, so
	// this is the strict read. With no log the arm exits before its header, and
	// before jq starts at all.
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return 0
	}
	if req.Draft {
		// No header precedes the draft, so a torn tail fails before any output.
		if err != nil {
			msg, code := bus.JQFailure(err)
			say(stderr, "crew: retro: %s: %s\n", paths.Log, msg)
			return code
		}
		return draft(crew, req.All, events, o, stdout, stderr)
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
