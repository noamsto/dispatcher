package status

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// MsgRequest is `crew msg <from> <to> <body>`.
type MsgRequest struct{ From, To, Body string }

// RunMsg posts a message.
func RunMsg(ctx context.Context, req MsgRequest, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	crew, ok := begin("msg", paths, stderr, o)
	if !ok {
		return exitFailure
	}
	from, to, body := req.From, req.To, req.Body
	ts := o.Clock.RealMS()
	if refuseSender(ctx, "msg", from, crew, o, stderr) {
		return exitFailure
	}

	// A caller that expanded an unset shell var into the recipient (e.g.
	// `dispatcher:$CREW_ID` with CREW_ID empty) silently lands a message nobody
	// reads. Fail loudly on any prefix left with no id.
	if strings.HasSuffix(to, ":") {
		say(stderr, "crew: msg recipient '%s' is missing an id after the colon\n", to)
		return exitFailure
	}
	// tmux reads a control byte as terminal input, so the role-watch types an
	// assignment only when its body is one line. Refused at send time for every
	// role: recipient, since a lead does not know its role's delivery mode.
	if strings.HasPrefix(to, "role:") && hasC0(body) {
		say(stderr, "crew: msg: refusing to send to '%s': a role assignment body must be one line, but this one contains a control character (newline/tab/…); re-send it as compact JSON, e.g. jq -c\n", to)
		return exitFailure
	}
	if host, isPeer := strings.CutPrefix(to, "peer:"); isPeer {
		if reason := peerRefusal(from, host, body, crew, ts, o); reason != "" {
			say(stderr, "crew: msg: refusing to send to '%s': %s\n", to, reason)
			return exitFailure
		}
	}
	if line := reviewSeamRefusal(ctx, from, to, body, crew, paths, o); line != "" {
		say(stderr, "%s\n", line)
		return exitFailure
	}

	return post("msg", paths, stderr, func(b string) string {
		return encodeLine(row[string]{TS: ts, CrewID: crew, From: from, To: to, Kind: bus.KindMsg, Body: b})
	}, body)
}

// peerRefusal is why a msg to peer:<host> cannot ride the peer sync, "" when it
// can. The row is an ordinary bus row that the next sync exports, so every
// refusal is at send time: a row that never leaves this host, or that the
// receiver would drop or find elided, must not look sent.
func peerRefusal(from, host, body, crew string, ts int64, o Options) string {
	if from != "dispatcher:"+crew {
		return "only a dispatcher can message a peer, a worker's rows never leave this host"
	}
	if !remote.HostNameRe.MatchString(host) {
		return fmt.Sprintf("invalid host name %q", host)
	}
	hosts, err := o.PeerHosts()
	if err != nil {
		return err.Error()
	}
	if !hosts[host].Peer {
		return fmt.Sprintf("%q is not a peer host (settings hosts.%s.peer)", host, host)
	}
	if len(body) > peer.MaxBody {
		return fmt.Sprintf("body is %d bytes, over the %d-byte peer limit", len(body), peer.MaxBody)
	}
	if line := encodeLine(row[string]{TS: ts, CrewID: crew, From: from, To: "peer:" + host, Kind: bus.KindMsg, Body: body}); len(line) > bus.LineMax {
		return fmt.Sprintf("the encoded row is %d bytes, over the %d-byte bus line, so the body would be elided", len(line), bus.LineMax)
	}
	return ""
}

// hasC0 is `_has_c0`: any byte `[[:cntrl:]]` matches under LC_ALL=C, the C0
// range and DEL.
func hasC0(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// reviewSeamRefusal is the plan gate's early check on a review seam: a branch
// gated on `plan: required` cannot post one until it has its plan seam, so a
// worker that skipped the plan phase meets it when it records that seam instead
// of only after it has pushed. Only a review seam body sent to review:<crew> by
// a worker session is checked; every other msg, and every session the status
// gate exempts, posts as before. The check is the status gate's own — same
// task-doc condition, same plan.jq fold — and it fails closed.
func reviewSeamRefusal(ctx context.Context, from, to, body, crew string, paths bus.Paths, o Options) string {
	if !strings.HasPrefix(from, "worker:") || to != "review:"+crew || !isReviewSeam(body) {
		return ""
	}
	doc, ok := o.taskDoc(ctx)
	if !ok || !planGated(doc) {
		return ""
	}
	posted, err := foldSeam(o, planProg, readLog(paths.Log), seamVars(crew, from, doc))
	switch {
	case err == nil && posted:
		return ""
	case err != nil:
		code := jqRuntime
		if errors.Is(err, errLogUnreadable) {
			code = jqUnreadable
		}
		return fmt.Sprintf("crew: msg: refusing the review seam for %s session %s — could not read the crew log for the plan seam (jq exit %d); the plan gate fails closed: retry, and if it keeps failing post crew status blocked naming why", doc.tier, from, code)
	default:
		return fmt.Sprintf("crew: msg: refusing the review seam for %s session %s — %s", doc.tier, from, fmt.Sprintf(planHint, crew))
	}
}

// isReviewSeam reports whether a msg body is a review seam: a JSON object whose
// "seam" is exactly "review", the shape seam.jq counts. The body is parsed with
// the fold's own FromJSON, so the two gates cannot read one differently.
func isReviewSeam(body string) bool {
	v, err := jqrun.FromJSON(body)
	if err != nil {
		return false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	s, ok := m["seam"].(string)
	return ok && s == "review"
}
