package reap

import (
	"math"
	"strconv"
)

// DefaultStaleAfter is how many seconds a terminal worker's kept worktree may
// sit before it reads as stale.
const DefaultStaleAfter = 259200

// Request is `crew reap`'s parsed flags. Idle is --idle's digits; "" is the
// default ReleaseGrace. StaleAfter is --stale-after's digits; "" is
// DefaultStaleAfter.
type Request struct {
	Quiet, DryRun, NoWait, JSON bool
	Idle, Discard, StaleAfter   string
	// keeps is the fleet run's shared recorder; a repo's Run appends to it
	// instead of printing its own document.
	keeps *keepLog
}

// flags is what the typed reap command resolves to.
type flags struct {
	quiet, dry, nowait, json bool
	// idle is --idle's seconds as given: digits only, so it is both the jq
	// `--argjson idle` text and the release grace.
	idle       string
	discard    string
	staleAfter string
}

func newFlags(req Request) flags {
	fl := flags{quiet: req.Quiet, dry: req.DryRun, nowait: req.NoWait, json: req.JSON, idle: req.Idle, discard: req.Discard, staleAfter: req.StaleAfter}
	if fl.idle == "" {
		fl.idle = strconv.Itoa(ReleaseGrace)
	}
	if fl.staleAfter == "" {
		fl.staleAfter = strconv.Itoa(DefaultStaleAfter)
	}
	return fl
}

// staleSecs is --stale-after's seconds; a value that is not a number never goes stale.
func (f flags) staleSecs() int64 {
	n, err := strconv.ParseInt(f.staleAfter, 10, 64)
	if err != nil {
		return math.MaxInt64
	}
	return n
}

// grace is --idle's seconds as the release grace: a value that is not a number
// never expires a window.
func (f flags) grace() int64 {
	g, err := strconv.ParseInt(f.idle, 10, 64)
	if err != nil {
		return math.MaxInt64
	}
	return g
}
