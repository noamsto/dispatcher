package status

import (
	"context"
	"io"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/sender"
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
	from := "overseer:" + req.Self
	if refuseSender(ctx, "overseer msg", from, req.Crew, o, stderr) {
		return exitFailure
	}
	return postOverseerRow("overseer msg", req, from, o.Clock.RealMS(), paths, stderr)
}

// RunOverseerRelay posts a row another host's overseer relayed over ssh:
// from overseer:<req.Self> (the origin host) to dispatcher:<req.Crew>. It runs
// sender.CheckRelay itself and returns the posted row's ts.
func RunOverseerRelay(ctx context.Context, req OverseerMsgRequest, paths bus.Paths, stderr io.Writer, o Options) (ts int64, rc int) {
	o = o.withDefaults()
	from := "overseer:" + req.Self
	if reason := sender.CheckRelay(ctx, req.Self, o.Sender); reason != "" {
		say(stderr, "crew: overseer deliver: refusing relay from '%s' — %s\n", from, reason)
		return 0, exitFailure
	}
	ts = o.Clock.RealMS()
	if rc := postOverseerRow("overseer deliver", req, from, ts, paths, stderr); rc != 0 {
		return 0, rc
	}
	return ts, 0
}

// postOverseerRow is the row both entry points write, so its shape cannot drift.
func postOverseerRow(sub string, req OverseerMsgRequest, from string, ts int64, paths bus.Paths, stderr io.Writer) int {
	to := "dispatcher:" + req.Crew
	if line := msgLine(ts, req.Crew, from, to, req.Body); len(line) > bus.LineMax {
		say(stderr, "crew: %s: the encoded row is %d bytes, over the %d-byte bus line, so the body would be elided\n", sub, len(line), bus.LineMax)
		return exitFailure
	}
	return post(sub, paths, stderr, func(b string) string {
		return msgLine(ts, req.Crew, from, to, b)
	}, req.Body)
}
