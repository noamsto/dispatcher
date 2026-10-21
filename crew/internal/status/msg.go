package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
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
	if line := reviewSeamRefusal(ctx, from, to, body, crew, paths, o); line != "" {
		say(stderr, "%s\n", line)
		return exitFailure
	}

	ts := o.Clock.RealMS()
	return post("msg", paths, stderr, func(b string) string {
		return encodeLine(row[string]{TS: ts, CrewID: crew, From: from, To: to, Kind: bus.KindMsg, Body: b})
	}, body)
}

// hasC0 is `_has_c0`: any byte `[[:cntrl:]]` matches under LC_ALL=C, the C0
// range and DEL.
func hasC0(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// reviewSeamRefusal is the plan gate's early check on a review seam (#1091): a
// branch gated on `plan: required` cannot post one until it has its plan seam,
// so a worker that skipped the plan phase meets it when it records its review
// seam, before the push and before pr_open, instead of only after it has pushed
// its way through both. Only a review seam body sent to review:<crew> by a
// worker session is checked: every other msg, and every session the status gate
// exempts (a trivial or review-kind doc, a resume, `plan: provided`), posts as
// before. The check is the status gate's own — same task-doc condition, same
// plan.jq fold over the same bus log — and it fails closed.
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
// "seam" is exactly "review", the shape seam.jq counts. Any other body — plain
// text, an array, another seam — is an ordinary msg. A leading byte order mark
// is skipped here as it is by the fold's `_jqfromjson`, so the two gates read
// the same body the same way.
func isReviewSeam(body string) bool {
	var v any
	if json.Unmarshal([]byte(strings.TrimPrefix(body, "\ufeff")), &v) != nil {
		return false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	s, ok := m["seam"].(string)
	return ok && s == "review"
}
