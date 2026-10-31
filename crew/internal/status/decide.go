package status

import (
	"context"
	"io"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/peer"
)

// RunDecide is `crew decide "<text>"`: it posts the dispatcher's decision. The
// row is an ordinary bus row the peer sync exports, so a text the peer would
// drop is refused here rather than looking shared.
func RunDecide(ctx context.Context, text string, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	crew, ok := begin("decide", paths, stderr, o)
	if !ok {
		return exitFailure
	}
	ts := o.Clock.RealMS()
	if refuseSender(ctx, "decide", "dispatcher:"+crew, crew, o, stderr) {
		return exitFailure
	}
	if text == "" {
		say(stderr, "crew: decide: the decision text is empty\n")
		return exitFailure
	}
	if len(text) > peer.MaxBody {
		say(stderr, "crew: decide: text is %d bytes, over the %d-byte peer limit\n", len(text), peer.MaxBody)
		return exitFailure
	}
	row := peer.DecisionRow(ts, crew, text)
	if reason := peer.ExportDrop(row, "peer", time.UnixMilli(ts)); reason != "" {
		say(stderr, "crew: decide: peers would drop this row as %s (the encoded row is %d bytes; the peer row limit is %d)\n", reason, len(row), peer.MaxRow)
		return exitFailure
	}
	return post("decide", paths, stderr, func(b string) string { return peer.DecisionRow(ts, crew, b) }, text)
}
