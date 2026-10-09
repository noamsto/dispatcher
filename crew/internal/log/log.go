// Package log is `crew log`: every bus event of one crew as JSON lines, the
// contract behind the dispatcher's `crew log <crew> | jq …` idiom. The arm's
// jq program was `select(.crew_id==$crew)` — a member test, no fold — so this
// port runs no jq at all: it filters and re-encodes through jsonv, which keeps
// each line one compact JSON value with its source key order and number
// literals, and it keeps `jq -c`'s torn-tail behaviour through
// bus.ReadEventsTolerant (the well-formed prefix goes out, the parse error
// after it decides the exit status).
package log

import (
	"errors"
	"fmt"
	"io"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// jq's status for a log whose last row it could not index, and for one it could
// not parse: 5, the status jq gives a parse or type error.
const exitType = 5

// Options is everything Run reads beyond the bus: whether $JQ_COLORS was
// invalid (jq warns once, at startup, even when stdout is not a terminal), and
// the palette to colour with when it is a terminal (nil for plain `jq -c`
// output).
type Options struct {
	JQColorsInvalid bool
	Colors          *jsonv.Palette
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm: `jq -c --arg crew "$crew" 'select(.crew_id==$crew)' "$log"`.
// A log that is not a regular file prints nothing and exits 0, as the arm's
// `[ -f "$log" ]` does.
func Run(crew string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	events, err := bus.ReadEventsTolerant(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return 0
	}
	// jq has started by now, so a bad $JQ_COLORS warns even when it then fails
	// to read the log.
	if o.JQColorsInvalid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	// A torn or corrupt tail keeps the events parsed before the break: `jq -c`
	// had already printed them, and its parse error lands after the last line.
	var decode *bus.DecodeError
	if err != nil && !errors.As(err, &decode) {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: log: %s: %s\n", paths.Log, msg)
		return code
	}

	opts := jsonv.Options{Colors: o.Colors}
	// `.crew_id` is jq's index: an object either carries the crew or does not,
	// null indexes to null, and any other value is a type error — one stderr
	// line each, and jq's exit status is the *last* input's outcome rather than
	// a sticky flag (`5 {…}` exits 0, `{…} 5` exits 5).
	failed := false
	for _, ev := range events {
		v := ev.Raw
		switch v.Kind() {
		case jsonv.KindObject:
			// jq compares values: a non-string crew_id matches no crew, the empty
			// crew of a caller with no id to default to included.
			if id, ok := v.Get("crew_id"); ok {
				if s, isStr := id.AsString(); isStr && s == crew {
					_ = jsonv.Encode(stdout, v, opts)
					say(stdout, "\n")
				}
			}
			failed = false
		case jsonv.KindNull:
			failed = false
		case jsonv.KindFalse, jsonv.KindTrue, jsonv.KindNumber, jsonv.KindString, jsonv.KindArray:
			say(stderr, "crew: log: %s: cannot index %v with \"crew_id\"\n", paths.Log, v.Kind())
			failed = true
		}
	}
	if decode != nil {
		say(stderr, "crew: log: %s: %v\n", paths.Log, decode.Err)
		return exitType
	}
	if failed {
		return exitType
	}
	return 0
}
