package overseer

import (
	"context"
	"io"

	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/status"
)

// farRefusal is the code a refusing deliver prints on stdout as
// {"refused":"<code>"}, so the client words the refusal itself and shows no
// far text.
type farRefusal string

const (
	refusedOrigin farRefusal = "origin"
	refusedRelay  farRefusal = "relay"
	refusedBody   farRefusal = "body"
	refusedRoot   farRefusal = "root"
	refusedRepo   farRefusal = "repo"
	refusedCrew   farRefusal = "crew"
	refusedPost   farRefusal = "post"
)

// farRefusals is the client's fixed sentence for each refusal code.
var farRefusals = map[farRefusal]string{
	refusedOrigin: "the far host does not take this host's name as an origin",
	refusedRelay:  "the far host does not accept a relay from this host",
	refusedBody:   "the body is not one the far host accepts",
	refusedRoot:   "the far host cannot read its overseer root",
	refusedRepo:   "the repo is not a crew bus the far host serves",
	refusedCrew:   "the crew is not on that repo's bus there",
	refusedPost:   "the far host could not post the row",
}

// DeliverRequest is `crew overseer deliver --origin <host> -- <repo> <crew> '<json>'`.
type DeliverRequest struct{ Origin, Repo, Crew, Body string }

// RunDeliver posts a message the overseer of origin, another settings host,
// relayed over ssh: from overseer:<origin> to the dispatcher of <crew> on the
// bus of <repo>, under the same body rules as `crew overseer msg`. <repo> is
// one the served `status` names. It prints the posted row's ts. The relay
// check runs before any repo is read, and every refusal prints one stderr line
// and its {"refused":"<code>"} answer, and writes nothing.
func RunDeliver(ctx context.Context, req DeliverRequest, stdout, stderr io.Writer, o Options) int {
	if reason := badName("origin", req.Origin); reason != "" {
		return refuseFar(stdout, stderr, refusedOrigin, "%s", reason)
	}
	if reason := sender.CheckRelay(ctx, req.Origin, o.Sender); reason != "" {
		return refuseFar(stdout, stderr, refusedRelay, "refusing relay from 'overseer:%s' — %s", req.Origin, reason)
	}
	if reason := bodyRefusal(req.Body); reason != "" {
		return refuseFar(stdout, stderr, refusedBody, "%s", reason)
	}
	root, err := o.servedRoot(ctx)
	if err != nil {
		return refuseFar(stdout, stderr, refusedRoot, "overseer root: %v", err)
	}
	common, reason := findBus(ctx, req.Repo, fleet.ServedRepos(o.Registry, o.Getenv, root, stderr, "crew: overseer deliver"), o)
	if reason != "" {
		return refuseFar(stdout, stderr, refusedRepo, "%s", reason)
	}
	paths, reason := onBus(common, req.Crew)
	if reason != "" {
		return refuseFar(stdout, stderr, refusedCrew, "%s", reason)
	}
	ts, rc := status.RunOverseerRelay(ctx, status.OverseerMsgRequest{Self: req.Origin, Crew: req.Crew, Body: req.Body}, paths, stderr, status.Options{Clock: o.Clock, Sender: o.Sender})
	if rc != 0 {
		_ = emit(stdout, stderr, "deliver", refusal{refusedPost})
		return rc
	}
	return emit(stdout, stderr, "deliver", struct {
		TS int64 `json:"ts"`
	}{ts})
}

// refusal is a refusing deliver's answer on stdout.
type refusal struct {
	Refused farRefusal `json:"refused"`
}

// refuseFar prints a deliver refusal: its line on stderr, and its code on
// stdout, the only part the client reads.
func refuseFar(stdout, stderr io.Writer, code farRefusal, format string, args ...any) int {
	_ = emit(stdout, stderr, "deliver", refusal{code})
	return fail(stderr, "deliver", format, args...)
}

// servedRoot is o.ServedRoot's answer, "" when none is wired.
func (o Options) servedRoot(ctx context.Context) (string, error) {
	if o.ServedRoot == nil {
		return "", nil
	}
	return o.ServedRoot(ctx)
}
