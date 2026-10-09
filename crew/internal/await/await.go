// Package await is `crew await`: block until a msg answers one agent's question,
// print it, exit 0. Its delivered marks are shared state — bash `nudge` and
// `stall-watch --unread` read the same file through `_await_marks`.
//
// The fold is the arm's own jq, embedded and handed to jqrun (see await.jq): the
// due test is jq's total-order `>` and the batch a stable `sort_by`, and jsonv's
// fold comparators were deleted in #861 with gojq owning ordering.
package await

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

//go:embed await.jq
var program string

const (
	usageMsg     = "crew: await <agent> [--from SENDER] [--timeout S] [--interval S]"
	noCrewMsg    = "crew: CREW_ID unset and no WORKER_TASK.md crew_id"
	timeoutUsage = "crew: --timeout needs a non-negative integer (seconds)"

	defaultTimeout  = 300
	defaultInterval = "2"
	// maxTimeout is the tool ceiling: a held await past it is killed by the
	// harness mid-wait, so the arm clamps and says so instead.
	maxTimeout = 600
)

// Options is everything Run reads beyond the bus: how to resolve the crew the
// arm defaults to (`_crew_id`), how to put the buffered stdout on the wire (the
// marks are raised only once the rows are on it), the clock, and whether
// $JQ_COLORS was invalid.
type Options struct {
	CrewID          func() string
	Flush           func() error
	Clock           clock.Clock
	JQColorsInvalid bool
}

// timeoutRe is the arm's `^[0-9]{1,9}$` — up to nine digits, so the value always
// fits an int and the arm's `$((10#$timeout))` never overflows. (`_is_session_id`
// has its Go twin in bus.IsSessionID.)
var timeoutRe = regexp.MustCompile(`\A[0-9]{1,9}\z`)

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// call is one parsed invocation.
type call struct {
	me, from, interval string
	timeout            int
}

// Run is the arm. A timeout is not a failure: empty stdout, not the exit status,
// is the marker, and every msg of the winning sender's due backlog prints so a
// backlog is drained instead of hidden.
func Run(argv []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	crew := o.CrewID()
	if crew == "" {
		say(stderr, "%s\n", noCrewMsg)
		return 1
	}
	c, msg := parse(argv)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return 1
	}
	if c.timeout > maxTimeout {
		// The arm printed this after its own `$((10#$timeout))`, so the value
		// named is the normalized one.
		say(stderr, "crew: await --timeout %d clamped to %d (the 600s tool ceiling)\n", c.timeout, maxTimeout)
		c.timeout = maxTimeout
	}

	// `_clock_now_ms` is the arm's one jq whose stderr it did not throw away, so
	// a bad $JQ_COLORS surfaced once for `start` and once per poll. Go prints it
	// once for the call — the documented rule, and hold's — and under CREW_CLOCK
	// that jq never starts at all.
	warned := false
	nowMS := func() int64 {
		if !warned && o.JQColorsInvalid && o.Clock.CrewClock == "" {
			warned = true
			say(stderr, "Failed to set $JQ_COLORS\n")
		}
		return o.Clock.NowMS()
	}

	start := nowMS()
	deadline := start + int64(c.timeout)*1000
	// The marks are read once, before the loop: a mark another reader raises
	// while this await polls does not shrink what this caller is owed.
	delivered := marks.Read(paths.Dir, crew, c.me)

	for {
		if rows := due(paths.Log, crew, c, delivered); len(rows) > 0 {
			opts := jsonv.Options{}
			for _, r := range rows {
				_ = jsonv.Encode(stdout, r, opts)
				say(stdout, "\n")
			}
			// Flush before recording: a mark raised for rows nobody received
			// makes the next await skip them for good (inbox took the same
			// inversion in #876; the arm recorded before its printf).
			if o.Flush != nil {
				if err := o.Flush(); err != nil {
					return 1
				}
			}
			marks.Record(paths.Dir, crew, c.me, rows)
			return 0
		}
		now := nowMS()
		if now >= deadline {
			say(stderr, "crew: await ended after %ds — no reply to %s%s yet\n",
				(now-start)/1000, c.me, fromNote(c.from))
			return 0
		}
		if err := o.Clock.Sleep(c.interval, stderr); err != nil {
			return 1
		}
	}
}

// parse reads the arm's arguments in its order: the agent is argv[0] whatever it
// is (`--timeout` included, which the arm accepted as an addressee), then the
// flags with the last of each winning. msg is the arm's stderr line for a call
// it refuses.
func parse(argv []string) (call, string) {
	c := call{timeout: defaultTimeout, interval: defaultInterval}
	if len(argv) > 0 {
		c.me, argv = argv[0], argv[1:]
	}
	if c.me == "" {
		return c, usageMsg
	}
	// Only a worker id promises a session: a branch-only one matches no message
	// the caller could be waiting for, and the marks are per session.
	if strings.HasPrefix(c.me, "worker:") && !bus.IsSessionID(c.me) {
		return c, fmt.Sprintf(
			"crew: await: '%s' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message", c.me)
	}
	timeoutText := strconv.Itoa(defaultTimeout)
	for len(argv) > 0 {
		var dst *string
		switch flag := argv[0]; flag {
		case "--timeout":
			dst = &timeoutText
		case "--interval":
			dst = &c.interval
		case "--from":
			dst = &c.from
		default:
			return c, fmt.Sprintf("crew: await: unknown arg '%s'", flag)
		}
		if len(argv) < 2 || argv[1] == "" {
			return c, fmt.Sprintf("crew: %s needs a value", argv[0])
		}
		*dst, argv = argv[1], argv[2:]
	}
	if !timeoutRe.MatchString(timeoutText) {
		return c, timeoutUsage
	}
	// Base 10 like the arm's `$((10#$timeout))`, so `010` is ten and not eight.
	c.timeout, _ = strconv.Atoi(timeoutText)
	return c, ""
}

// due is the arm's one pass over the log, its `-R` + `fromjson?` per-line decode
// in Go and its fold in await.jq. Any failure is "nothing due": the arm ran jq
// under `2>/dev/null || true`, so a row it cannot index, a torn tail, an
// unreadable log and a missing log all leave stdout empty and the poll running
// to the deadline.
func due(path, crew string, c call, got jsonv.Value) []jsonv.Value {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var in []jsonv.Value
	for _, line := range strings.Split(string(data), "\n") {
		vs, err := jsonv.DecodeStream(strings.NewReader(line))
		if err != nil || len(vs) != 1 {
			continue
		}
		in = append(in, vs[0])
	}
	out, err := jqrun.Run(program, in, 0, map[string]jsonv.Value{
		"crew": jsonv.Str(crew),
		"me":   jsonv.Str(c.me),
		"from": jsonv.Str(c.from),
		"got":  got,
	})
	if err != nil || out.Kind() != jsonv.KindArray {
		return nil
	}
	return out.Elems()
}

// fromNote is the arm's `${from:+ from $from}`.
func fromNote(from string) string {
	if from == "" {
		return ""
	}
	return " from " + from
}
