package status

import (
	"io"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// RunMsg is `crew msg <from> <to> <body>`.
func RunMsg(args []string, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	crew, ok := begin("msg", paths, stderr, o)
	if !ok {
		return exitFailure
	}
	at := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	from, to, body := at(0), at(1), at(2)

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

	ts := stamp(o)
	return post("msg", paths, stderr, func(b string) string {
		return compact(jsonv.Object(
			member("ts", ts),
			member("crew_id", jsonv.Str(crew)),
			member("from", jsonv.Str(from)),
			member("to", jsonv.Str(to)),
			member("kind", jsonv.Str("msg")),
			member("body", jsonv.Str(b)),
		))
	}, body)
}

// hasC0 is `_has_c0`: any byte `[[:cntrl:]]` matches under LC_ALL=C, the C0
// range and DEL.
func hasC0(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
