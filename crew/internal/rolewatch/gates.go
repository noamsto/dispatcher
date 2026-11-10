package rolewatch

import "strings"

// SenderAllowed admits only the role's own lead: a worker on this branch (a
// `#` after the `#s` session mark would let worker:<branch>#y#s.. pass as a
// sibling branch's worker) or the crew's dispatcher.
func SenderAllowed(from, branch, crew string) bool {
	if rest, ok := strings.CutPrefix(from, "worker:"+branch+"#s"); ok && !strings.Contains(rest, "#") {
		return true
	}
	return crew != "" && from == "dispatcher:"+crew
}

// AssignmentSafe refuses every C0 byte and DEL: tmux would read them as
// terminal input. Multi-byte UTF-8 runes are all >= 0x80 and pass.
func AssignmentSafe(body string) bool {
	return !strings.ContainsFunc(body, func(r rune) bool { return r < 0x20 || r == 0x7f })
}
