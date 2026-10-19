package status

import (
	"context"
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
