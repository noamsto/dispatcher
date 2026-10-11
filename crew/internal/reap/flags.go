package reap

import (
	"fmt"
	"io"
	"strconv"
)

// flags is the arm's option loop.
type flags struct {
	quiet, dry, nowait bool
	// idle is --idle's seconds as given: digits only, so it is both the jq
	// `--argjson idle` text and the release grace.
	idle    string
	discard string
}

// parseFlags is the arm's `while [ $# -gt 0 ]` loop and its --idle check. A
// refusal has printed its one line; the arm exits 1.
func parseFlags(args []string, stderr io.Writer) (flags, bool) {
	fl := flags{idle: strconv.Itoa(ReleaseGrace)}
	refuse := func(line string) (flags, bool) {
		_, _ = fmt.Fprintln(stderr, line)
		return flags{}, false
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--quiet":
			fl.quiet = true
		case "--dry-run":
			fl.dry = true
		case "--no-wait":
			fl.nowait = true
		case "--idle":
			if i+1 >= len(args) || args[i+1] == "" {
				return refuse("crew: --idle needs a value in seconds")
			}
			i++
			fl.idle = args[i]
		case "--discard":
			if i+1 >= len(args) || args[i+1] == "" {
				return refuse("crew: --discard needs a branch")
			}
			i++
			fl.discard = args[i]
		default:
			return refuse("crew: reap takes --quiet, --dry-run, --no-wait, --idle S and --discard BRANCH (got '" + args[i] + "')")
		}
	}
	if !allDigits(fl.idle) {
		return refuse("crew: --idle must be a non-negative integer number of seconds")
	}
	return fl, true
}

// allDigits is the arm's --idle check: refused when empty or holding a
// non-digit.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
