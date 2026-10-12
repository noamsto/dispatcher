package entry

import (
	"io"
	"time"
)

// NewOptions is everything RunNew reads beyond stdout.
type NewOptions struct {
	// Now is the clock `date +%s` read.
	Now func() time.Time
	// PID is `$$`. The arm `exec`s this binary, so this process is the process
	// bash was, and its pid is the one `$$` named — which is the whole reason
	// two `crew new` calls in the same second still differ.
	PID int
}

// RunNew is crew.sh's `new` arm: mint a crew id, timestamp-pid and unique by
// construction, and print it. It registers nothing — the dispatcher records the
// id on its first event.
func RunNew(stdout io.Writer, o NewOptions) int {
	say(stdout, "%d-%d\n", o.Now().Unix(), o.PID)
	return 0
}
