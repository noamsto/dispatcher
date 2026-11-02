package status

import (
	"context"
	"io"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// OverseerMsgRequest is the row `crew overseer msg` writes once it has picked
// the bus and checked the body: from overseer:<Self> to dispatcher:<Crew>.
type OverseerMsgRequest struct{ Self, Crew, Body string }

// RunOverseerMsg posts the overseer's message to a dispatcher. It runs the
// sender check itself, so no caller writes the row without it, and it refuses a
// row over the bus line rather than let the append elide the body. The caller
// picks only a repo whose bus already exists, so the dir is not created here.
func RunOverseerMsg(ctx context.Context, req OverseerMsgRequest, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	from, to := "overseer:"+req.Self, "dispatcher:"+req.Crew
	if refuseSender(ctx, "overseer msg", from, req.Crew, o, stderr) {
		return exitFailure
	}
	ts := o.Clock.RealMS()
	if line := msgLine(ts, req.Crew, from, to, req.Body); len(line) > bus.LineMax {
		say(stderr, "crew: overseer msg: the encoded row is %d bytes, over the %d-byte bus line, so the body would be elided\n", len(line), bus.LineMax)
		return exitFailure
	}
	return post("overseer msg", paths, stderr, func(b string) string {
		return msgLine(ts, req.Crew, from, to, b)
	}, req.Body)
}
