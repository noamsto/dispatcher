package overseer

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

// pullWindow is how far back a pull with no --since reaches.
const pullWindow = 24 * time.Hour

// StatusRequest is `crew overseer status --origin <host> [--since <ms>]`.
type StatusRequest struct {
	Origin string
	Since  *int64
}

// statusRefusal is the answer a refusing far status writes on stdout: the
// requester words the code locally, so one JSON line carries v, this host's
// name and clock, and the fixed refuse code alone.
type statusRefusal struct {
	V      int    `json:"v"`
	Self   string `json:"self"`
	TS     int64  `json:"ts"`
	Refuse string `json:"refuse"`
}

// RunStatus answers a pull from the overseer of origin, another settings host:
// this host's fleet status in served mode, and the msgs to overseer:<origin>
// above since, as one JSON line. An answer that cannot advance is refused: one
// coded line on stdout, the human reason on stderr, the exit non-zero.
func RunStatus(ctx context.Context, req StatusRequest, stdout, stderr io.Writer, o Options) int {
	if reason := originRefusal(req.Origin, o.Sender); reason != "" {
		return fail(stderr, "status", "%s", reason)
	}
	since := o.Clock.Now().Add(-pullWindow).UnixMilli()
	if req.Since != nil {
		if *req.Since < 0 {
			return fail(stderr, "status", "--since %d is below 0", *req.Since)
		}
		since = *req.Since
	}
	root, err := o.servedRoot(ctx)
	if err != nil {
		return fail(stderr, "status", "overseer root: %v", err)
	}
	o.Status.Served = &fleet.ServedStatus{Root: root}
	ans, err := fleet.ServeFar(ctx, req.Origin, since, stderr, o.Status)
	if err != nil {
		var refusal fleet.Refusal
		if errors.As(err, &refusal) {
			// A refusal is still one coded answer on stdout; the human reason
			// stays on stderr and the exit stays non-zero.
			self, _ := hostName(o.Sender)
			_ = emit(stdout, stderr, "status", statusRefusal{V: 1, Self: self, TS: o.Clock.Now().UnixMilli(), Refuse: refusal.Code})
		}
		return fail(stderr, "status", "%v", err)
	}
	return emit(stdout, stderr, "status", ans)
}

// originRefusal is why origin may not pull from this host, "" when it may.
func originRefusal(origin string, p sender.Probes) string {
	if reason := badName("origin", origin); reason != "" {
		return reason
	}
	self, err := hostName(p)
	if err != nil {
		return "this host's name cannot be read (" + err.Error() + ")"
	}
	if origin == self {
		return "origin " + origin + " is this host's own name, and only another host's overseer pulls"
	}
	return settingsHost("origin", origin, p)
}
