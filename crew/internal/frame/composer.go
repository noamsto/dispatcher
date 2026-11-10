package frame

import (
	"regexp"
	"strings"
)

var (
	reCodexFooter   = posix(`^[[:space:]]{2}[^[:space:]].*[[:space:]]Vim:[[:space:]]Insert$`)
	reCodexStatus   = posix(`^[[:space:]]{2}.*(for shortcuts|warnings).*$`)
	reCodexWorking  = posix(`^[[:space:]]*[•·] Working \(`)
	reCursorVersion = posix(`^[[:space:]]*v[0-9][0-9.]*-`)
	reCursorFooter  = posix(`^[[:space:]]{2}[^[:space:]].*[[:space:]]Run Everything -- INSERT --$`)
	reCursorPath    = posix(`^[[:space:]]{2}([~/]|[[:alnum:]_.-]+/)`)
	reCursorBranch  = posix(`^[[:space:]]*.*[[:space:]]·[[:space:]][^[:space:]]+$`)
	reCursorModel   = posix(`^[[:space:]]+(256K Low|High)[[:space:]]*%?[[:space:]]*$`)
	reCursorThink   = posix(`Thinking[[:space:]]+[0-9]+ tokens`)
)

// anyLine is `printf '%s\n' "$text" | grep -qE re`: a per-row search.
func anyLine(text string, re *regexp.Regexp) bool {
	for _, l := range strings.Split(text, "\n") {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// soleFooter returns the index of the only row matching footer.
func soleFooter(rows []string, footer *regexp.Regexp) (int, bool) {
	at := -1
	for i, r := range rows {
		if !footer.MatchString(r) {
			continue
		}
		if at != -1 {
			return 0, false
		}
		at = i
	}
	return at, at != -1
}

// composerValue reconstructs the one block touching the blank separator rows
// above the footer: its first row minus prefix, then every wrapped row.
func composerValue(rows []string, footerAt, separatorRows int, prefix string) (string, bool) {
	for i := 1; i <= separatorRows; i++ {
		if rows[footerAt-i] != "" {
			return "", false
		}
	}
	start := footerAt - separatorRows - 1
	if rows[start] == "" {
		return "", false
	}
	for start > 0 && rows[start-1] != "" {
		start--
	}
	value, ok := strings.CutPrefix(rows[start], prefix)
	if !ok {
		return "", false
	}
	return value + strings.Join(rows[start+1:footerAt-separatorRows], ""), true
}

// FooterComposerEqual confirms only the composer immediately above one
// observed engine footer. Captures do not distinguish a literal editor newline
// from terminal wrapping, so an expected value containing one is refused.
func FooterComposerEqual(text, prefix string, footer *regexp.Regexp, separatorRows int, expected string, trailing ...*regexp.Regexp) bool {
	if strings.Contains(expected, "\n") {
		return false
	}
	rows := strings.Split(text, "\n")
	footerAt, ok := soleFooter(rows, footer)
	if !ok || footerAt <= separatorRows {
		return false
	}

	// A footer-like transcript line or dialog overlay is not a writable
	// composer: the observed status rows must follow the footer and end the
	// capture.
	for k, re := range trailing {
		at := footerAt + k + 1
		if at >= len(rows) || !re.MatchString(rows[at]) {
			return false
		}
	}
	if footerAt+len(trailing) != len(rows)-1 {
		return false
	}

	value, ok := composerValue(rows, footerAt, separatorRows, prefix)
	return ok && value == expected
}

// CodexComposer reports whether the Codex pane shows composer as its unsent
// input on an idle frame. The composer is unbordered, so the Vim status row
// and the live-turn vetoes are what keep another terminal's `›` line from
// becoming writable.
func CodexComposer(text, composer string) bool {
	if !FooterComposerEqual(text, "› ", reCodexFooter, 1, composer, reCodexStatus) {
		return false
	}
	return !strings.Contains(text, "esc to interrupt") && !anyLine(text, reCodexWorking)
}

func CodexIdleBox(text string) bool {
	return CodexComposer(text, "Ask Codex to do anything")
}

// CursorComposer is CodexComposer's counterpart: the version row, one mode
// footer and the status rows under that footer make the unbordered composer
// writable.
func CursorComposer(text, composer string) bool {
	if !anyLine(text, reCursorVersion) || strings.Contains(composer, "\n") {
		return false
	}
	rows := strings.Split(text, "\n")
	footerAt, ok := soleFooter(rows, reCursorFooter)
	if !ok || footerAt <= 2 {
		return false
	}
	const prefix = "  → "
	if value, ok := composerValue(rows, footerAt, 2, prefix); !ok || value != composer {
		return false
	}
	n, path := 0, false
	for _, r := range rows[footerAt+1:] {
		if r == "" {
			continue
		}
		n++
		if n > 6 {
			return false
		}
		switch {
		case reCursorPath.MatchString(r):
			path = true
		case reCursorBranch.MatchString(r), reCursorModel.MatchString(r):
		default:
			return false
		}
	}
	if n < 1 || !path {
		return false
	}
	return !strings.Contains(text, "ctrl+c to stop") && !anyLine(text, reCursorThink)
}

func CursorIdleBox(text string) bool {
	return CursorComposer(text, "Plan, search, build anything") ||
		CursorComposer(text, "Add a follow-up")
}
