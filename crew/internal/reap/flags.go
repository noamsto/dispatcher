package reap

import "strconv"

// Request is `crew reap`'s parsed flags. Idle is --idle's digits; "" is the
// default ReleaseGrace.
type Request struct {
	Quiet, DryRun, NoWait bool
	Idle, Discard         string
}

// flags is the arm's option loop.
type flags struct {
	quiet, dry, nowait bool
	// idle is --idle's seconds as given: digits only, so it is both the jq
	// `--argjson idle` text and the release grace.
	idle    string
	discard string
}

func newFlags(req Request) flags {
	fl := flags{quiet: req.Quiet, dry: req.DryRun, nowait: req.NoWait, idle: req.Idle, discard: req.Discard}
	if fl.idle == "" {
		fl.idle = strconv.Itoa(ReleaseGrace)
	}
	return fl
}
