package overseer

import (
	"context"
	"io"

	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/status"
)

// DeliverRequest is `crew overseer deliver --origin <host> -- <repo> <crew> '<json>'`.
type DeliverRequest struct{ Origin, Repo, Crew, Body string }

// RunDeliver posts a message the overseer of origin, another settings host,
// relayed over ssh: from overseer:<origin> to the dispatcher of <crew> on the
// bus of <repo>, under the same body rules as `crew overseer msg`. It prints
// the posted row's ts. The relay check runs before any repo is read, and every
// refusal prints one line and writes nothing.
func RunDeliver(ctx context.Context, req DeliverRequest, stdout, stderr io.Writer, o Options) int {
	if reason := badName("origin", req.Origin); reason != "" {
		return fail(stderr, "deliver", "%s", reason)
	}
	if reason := sender.CheckRelay(ctx, req.Origin, o.Sender); reason != "" {
		return fail(stderr, "deliver", "refusing relay from 'overseer:%s' — %s", req.Origin, reason)
	}
	if reason := bodyRefusal(req.Body); reason != "" {
		return fail(stderr, "deliver", "%s", reason)
	}
	paths, reason := busFor(ctx, req.Repo, req.Crew, "deliver", o, stderr)
	if reason != "" {
		return fail(stderr, "deliver", "%s", reason)
	}
	ts, rc := status.RunOverseerRelay(ctx, status.OverseerMsgRequest{Self: req.Origin, Crew: req.Crew, Body: req.Body}, paths, stderr, status.Options{Clock: o.Clock, Sender: o.Sender})
	if rc != 0 {
		return rc
	}
	return emit(stdout, stderr, "deliver", struct {
		TS int64 `json:"ts"`
	}{ts})
}
