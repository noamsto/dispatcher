package rosterrender

import (
	"cmp"
	"strconv"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// The arm's refusals, in its order. Every one is exit 64, like `stream`.
const (
	errInterval     = "crew: --interval must be a positive integer number of seconds"
	errQuiet        = "crew: --quiet must be a non-negative integer number of seconds"
	errCrewRequired = "crew: roster-render: --crew is required"
	errOnceDetach   = "crew: roster-render: --once and --detach are mutually exclusive"
	errCrewID       = "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'"
)

const (
	defaultInterval = 2
	defaultQuiet    = 1800
)

// call is one parsed invocation. intervalText and quietText stay the caller's
// bytes as well as their values: they are what `--detach` and the build hop hand
// the daemon, exactly as the arm passed "$rr_interval" and "$rr_quiet".
type call struct {
	crew         string
	intervalText string
	quietText    string
	interval     int64
	quiet        int64
	pane         string
	noOpen, once bool
	detach       bool
}

// Request is `crew roster-render`'s flags as the caller typed them. An empty
// string is a flag never passed: a given-empty one is refused before it gets here.
type Request struct {
	Crew, Pane, Interval, Quiet string
	NoOpen, Once, Detach        bool
}

// check is the arm's five refusals that follow its flag loop, in that order: a
// bad interval beats a missing crew. msg is the stderr line and code its status
// when the arguments themselves fail.
func check(r Request) (call, string, int) {
	c := call{crew: r.Crew, pane: r.Pane, noOpen: r.NoOpen, once: r.Once, detach: r.Detach}
	// The arm's initialisers: a flag the caller never passed still reaches the
	// daemon with the default's own bytes.
	intervalText, quietText := cmp.Or(r.Interval, strconv.Itoa(defaultInterval)), cmp.Or(r.Quiet, strconv.Itoa(defaultQuiet))
	var interval, quiet int64

	// The arm tests the digits and then the sign, both against the same message.
	var ok bool
	if interval, ok = positive(intervalText); !ok || interval <= 0 {
		return c, errInterval, exitUsage
	}
	// All the arm did with the interval is `sleep "$rr_interval"`, which waits out
	// any value. Here it becomes a time.Duration, and past maxSleepInterval that
	// product wraps negative, turning a bad flag into a loop that hammers tmux with
	// no pause at all. Refusing is the nearest honest answer.
	if interval > maxSleepInterval {
		return c, errInterval, exitUsage
	}
	if quiet, ok = positive(quietText); !ok {
		return c, errQuiet, exitUsage
	}
	if c.crew == "" {
		return c, errCrewRequired, exitUsage
	}
	if c.once && c.detach {
		return c, errOnceDetach, exitUsage
	}
	// Last, like the arm's `case`, which is bus.ValidCrewID's inverse.
	if !bus.ValidCrewID(c.crew) {
		return c, errCrewID, exitUsage
	}

	c.interval, c.quiet = interval, quiet
	c.intervalText, c.quietText = strconv.FormatInt(interval, 10), strconv.FormatInt(quiet, 10)
	return c, "", exitOK
}

// positive is bash's digit `case` plus the decimal read. A digit string too big for
// an int64 fails bash's `[ "$v" -gt 0 ]` the way a non-digit does — `[` reports
// "integer expression expected", and the arm's `||` refused with its own message —
// so it is refused rather than clamped to a bound.
func positive(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
