// Package frame classifies tmux pane captures: prompt, quota, background-wait,
// meter and input-box shapes of the claude, codex, pi and cursor panes. The
// predicates came from bash — the ones dispatch.sh's --role-watch still ships
// are its own copies, and the drift test compares this package against them —
// while the quota-prompt, session-limit and idle-reason detectors exist only
// here, their expectations frozen in testdata/golden-oracle.tsv. Every function
// takes the sampler's stdout with trailing newlines stripped, as
// `printf '%s\n' "$1"` presents it, and engine replaces the bash predicates'
// read of the global $engine.
package frame

import (
	"regexp"
	"strings"
)

// posixClasses maps the bash regexes' POSIX classes onto what glibc's UTF-8
// locale (the arm's runtime) matches; RE2's own [[:space:]] is ASCII only.
// glibc's space class leaves out NBSP, U+2007 and U+202F.
// glibc's alnum also takes Other_Alphabetic combining marks (U+093E, U+0345), which RE2 cannot name.
var posixClasses = strings.NewReplacer(
	"[:space:]", `\t\n\v\f\r \x{1680}\x{2000}-\x{2006}\x{2008}-\x{200A}\x{2028}\x{2029}\x{205F}\x{3000}`,
	"[:alnum:]", `\p{L}\p{Nd}\p{Nl}`,
)

func posix(pattern string) *regexp.Regexp {
	return regexp.MustCompile(posixClasses.Replace(pattern))
}

func posixLongest(pattern string) *regexp.Regexp {
	re := posix(pattern)
	re.Longest()
	return re
}

var (
	reBlank   = posix(`^[[:space:]]*$`)
	reOption  = posix(`^[[:space:]]*(>|❯|\*)?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]`)
	reMeter   = posix(`^[^[:alnum:]]*[A-Za-z]+…[[:space:]]\(([0-9]+h([[:space:]][0-9]+m)?([[:space:]][0-9]+s)?|[0-9]+m([[:space:]][0-9]+s)?|[0-9]+s)[[:space:]]·[[:space:]]↓[[:space:]][0-9.]+k?[[:space:]]tokens`)
	reSubrow  = posix(`^[[:space:]]*[^[:alnum:][:space:]]+[[:space:]]+[a-z][a-z-]+[[:space:]][[:space:]]+.*[[:space:]](([0-9]+h[[:space:]])?([0-9]+m[[:space:]])?[0-9]+s)[[:space:]]·[[:space:]]↓`)
	reSpinner = posix(`^[^[:alnum:]]*[A-Za-z]+…`)
	reDone    = posix(`·[[:space:]]done[[:space:]]+[0-9]{1,2}:[0-9]{2}`)
	reShells  = posix(`(^|[^0-9])[1-9][0-9]*[[:space:]]shells?([[:space:]]still running|[[:space:]]·|$)`)
	reBgWork  = posix(`(^|[^0-9])[1-9][0-9]*[[:space:]](shells?|monitors?)([[:space:]]still running|[[:space:]]·|$)`)
	reDraft   = posixLongest("^[[:space:]]*❯([[:space:]]|\u00a0)*")
	reCSI     = regexp.MustCompile(`\x1b\[[0-9;]*m`)

	// The post-turn lines claude can print under the finished-turn marker.
	reClearHint  = posix(`^[[:space:]]*new task\?[[:space:]]+/clear[[:space:]]`)
	reHookReload = posix(`^[[:space:]]*●[[:space:]]+[^[:space:]:]+:[[:space:]]+hooks\.json changed — reloaded`)
	reRecapEnd   = posix(`/config\)[[:space:]]*$`)
	reRecap      = posix(`^[[:space:]]*※[[:space:]]+recap:`)

	rePiScrollUp   = regexp.MustCompile(`↑ [0-9]+ more`)
	rePiScrollDown = regexp.MustCompile(`↓ [0-9]+ more`)
	rePiVimMode    = posixLongest(`[[:space:]]+(INSERT|NORMAL|EX|VISUAL|V-LINE)([[:space:]].*)?[[:space:]]*$`)
	// The bash originals run these two under LC_ALL=C, so their space class
	// is ASCII.
	rePiWorkingRow  = regexp.MustCompile(`^[\t\n\v\f\r ]*[\x{2800}-\x{28FF}].*Working`)
	rePiWorkingRule = regexp.MustCompile(`^─.*(Working|[\x{2800}-\x{28FF}])`)

	rePiTruncated = regexp.MustCompile(`^ ?Response was truncated before completion\.`)
	rePiError     = regexp.MustCompile(`^ ?Error: `)
	rePiBugHint   = regexp.MustCompile(`^ ?If this looks like a `)
	reAborted     = regexp.MustCompile(`(?i)\b(abort(ed)?|cancel(l?ed)?)\b`)
)

const (
	hookReviewFrame = "Hooks need review\n" +
		"  1 hook is new or changed.\n" +
		"  Hooks can run outside the sandbox after you trust them.\n" +
		"› 1. Review hooks  2. Trust all and continue  3. Continue without trusting"
	ghostMarker = "❯\u00a0\x1b[2m"
)

var (
	FirstRowClaude = posix(`^[[:space:]]*❯`)
	FirstRowAny    = regexp.MustCompile(`.*`)
)

// Tail is `printf '%s\n' "$text" | grep -v '^[[:space:]]*$' | tail -n`: the sampler's
// window over a capture, blank rows dropped on the raw line as grep sees it.
func Tail(text string, n int) []string {
	var keep []string
	for _, l := range strings.Split(text, "\n") {
		if !reBlank.MatchString(l) {
			keep = append(keep, l)
		}
	}
	return keep[max(len(keep)-n, 0):]
}

func anyMatch(lines []string, re *regexp.Regexp) bool {
	for _, l := range lines {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

func anyContains(lines []string, s string) bool {
	for _, l := range lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func lastMatch(lines []string, re *regexp.Regexp) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if re.MatchString(lines[i]) {
			return lines[i]
		}
	}
	return ""
}

// IsCodexHookReviewPrompt is _is_codex_hook_review_prompt.
func IsCodexHookReviewPrompt(text string) bool {
	return strings.Join(Tail(text, 4), "\n") == hookReviewFrame
}

// IsPermissionPrompt is _is_permission_prompt.
func IsPermissionPrompt(engine, text string) bool {
	if engine != "claude" {
		return false
	}
	t := Tail(text, 10)
	if len(t) == 0 || !strings.Contains(t[len(t)-1], "Esc to cancel · Tab to amend") {
		return false
	}
	above := t[:len(t)-1]
	return anyMatch(above, reOption) && anyContains(above, "Do you want to proceed?")
}

// IsPrompt is _is_prompt.
func IsPrompt(engine, text string) bool {
	if engine == "codex" {
		return IsCodexHookReviewPrompt(text)
	}
	t := Tail(text, 7)
	if len(t) == 0 {
		return false
	}
	last := t[len(t)-1]
	if !strings.Contains(last, "Enter to select") && !strings.Contains(last, "Enter to confirm") {
		return false
	}
	return anyMatch(t[:len(t)-1], reOption)
}

// IsQuotaPrompt is _is_quota_prompt.
func IsQuotaPrompt(text string) bool {
	return anyContains(Tail(text, 12), "Stop and wait for limit to reset")
}

// IsQuotaSessionLimit is _is_quota_session_limit.
func IsQuotaSessionLimit(text string) bool {
	t := Tail(text, 15)
	return anyContains(t, "You've hit your session limit") &&
		anyContains(t, "/upgrade to increase your usage limit") &&
		anyContains(t[max(len(t)-6, 0):], "uses your weekly limit")
}

// IsQuotaCursorLimit is _is_quota_cursor_limit.
func IsQuotaCursorLimit(text string) bool {
	t := Tail(text, 12)
	return anyContains(t, "You've reached your monthly usage limit") && anyContains(t, "spendLimitHit: true")
}

// IsBgWait is _is_bg_wait: a finished turn's `· done HH:MM` marker with a
// background shell still running and no live spinner or meter.
func IsBgWait(text string) bool {
	t := Tail(text, 8)
	return anyMatch(t, reDone) && anyMatch(t, reShells) && !anyMatch(t, reSpinner) && MeterLine(text) == ""
}

// MeterLine is _meter_line.
func MeterLine(text string) string {
	return lastMatch(strings.Split(text, "\n"), reMeter)
}

// HasSubrow is _has_subrow.
func HasSubrow(text string) bool {
	return anyMatch(strings.Split(text, "\n"), reSubrow)
}

// BoxRows is _box_rows: row is the first row inside the box verbatim ("" for
// an empty box), above the up to 7 CSI-stripped rows above the upper rule,
// nearest first.
func BoxRows(text string, first *regexp.Regexp) (row string, above []string, ok bool) {
	raw, plain, a, b, off, ok := scanBox(text)
	if !ok {
		return "", nil, false
	}
	if b-a == 1 {
		if !first.MatchString("") {
			return "", nil, false
		}
	} else {
		if !first.MatchString(plain[a+1]) {
			return "", nil, false
		}
		row = raw[a+1]
	}
	for j := a - 1; j >= off && j >= a-7; j-- {
		above = append(above, plain[j])
	}
	return row, above, true
}

// BoxText is every row inside the input box (the same bounds as BoxRows) concatenated
// with claude's `❯` and all whitespace dropped, so a box the engine wrapped — at a
// space or mid-word — still compares equal to the whitespace-free line a caller typed.
// Empty when the box is empty or absent: unlike BoxRows, an empty box is a result and
// not a failure, so the first-row check runs only when rows sit between the rules.
func BoxText(text string, first *regexp.Regexp) string {
	_, plain, a, b, _, ok := scanBox(text)
	if !ok {
		return ""
	}
	if b-a > 1 && !first.MatchString(plain[a+1]) {
		return ""
	}
	s := strings.Join(plain[a+1:b], "")
	if loc := first0(FirstRowClaude, s); loc >= 0 {
		s = s[loc:]
	}
	return strings.Map(func(r rune) rune {
		// The awk's `[[:space:]]|\302\240`: glibc's class, plus the NBSP the
		// class leaves out (claude's ghost-text separator).
		if isPosixSpace(r) || r == '\u00a0' {
			return -1
		}
		return r
	}, s)
}

// first0 is `sub(/^[[:space:]]*❯/, "", s)`: the byte offset just past a leading
// match, or -1 when the pattern does not anchor at the start.
func first0(re *regexp.Regexp, s string) int {
	if loc := re.FindStringIndex(s); loc != nil && loc[0] == 0 {
		return loc[1]
	}
	return -1
}

// HasSpinnerRow is the post-Enter live-turn test over a box's above rows: any row
// matching the `…` spinner (`^[^[:alnum:]]*[A-Za-z]+…`).
func HasSpinnerRow(rows []string) bool { return anyMatch(rows, reSpinner) }

// isPosixSpace is the character class behind posixClasses' `[:space:]` mapping —
// what glibc's UTF-8 locale matches, which is not RE2's ASCII-only class.
func isPosixSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		'\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006',
		'\u2008', '\u2009', '\u200a', '\u2028', '\u2029', '\u205f', '\u3000':
		return true
	}
	return false
}

// scanBox is the box scan BoxRows and BoxText share: the capture's rows with CSI
// stripped and blanks dropped, plus the indices of the LAST two `─` rules within the
// sampler's 30-row window (a is the upper rule, b the lower) and the window's offset.
// ok is false when there is no such pair or the bounds say it is not a box: at most 12
// rows inside, 1-5 rows after the lower rule.
func scanBox(text string) (raw, plain []string, a, b, off int, ok bool) {
	for _, l := range strings.Split(text, "\n") {
		s := reCSI.ReplaceAllLiteralString(l, "")
		if reBlank.MatchString(s) {
			continue
		}
		raw = append(raw, l)
		plain = append(plain, s)
	}
	n := len(plain)
	off = max(n-30, 0)
	a, b = -1, -1
	for i := n - 1; i >= off; i-- {
		if !strings.HasPrefix(plain[i], "─") {
			continue
		}
		if b < 0 {
			b = i
			continue
		}
		a = i
		break
	}
	if a < 0 || b-a-1 > 12 || n-1-b < 1 || n-1-b > 5 {
		return nil, nil, 0, 0, 0, false
	}
	return raw, plain, a, b, off, true
}

// ClaudeIdleBox is _claude_idle_box; colored "" means no colored capture.
func ClaudeIdleBox(plain, colored string, own bool) bool {
	t := Tail(plain, 30)
	if lastMatch(t, reMeter) != "" || anyMatch(t, reSubrow) || anyContains(t, "esc to interrupt") {
		return false
	}
	row, above, ok := BoxRows(plain, FirstRowClaude)
	if !ok || reOption.MatchString(row) || anyMatch(above, reSpinner) {
		return false
	}
	if own {
		return true
	}
	if reDraft.ReplaceAllLiteralString(row, "") == "" {
		return true
	}
	if colored == "" {
		return false
	}
	coloredRow, _, ok := BoxRows(colored, FirstRowClaude)
	return ok && strings.Contains(coloredRow, ghostMarker)
}

// PiLiveTurn is _pi_live_turn: rule text other than pi's scroll indicators
// and pi-vim's lower-rule mode label.
func PiLiveTurn(text string) bool {
	for _, l := range Tail(text, 30) {
		if !strings.HasPrefix(l, "─") {
			continue
		}
		l = strings.ReplaceAll(l, "─", "")
		l = rePiScrollUp.ReplaceAllLiteralString(l, "")
		l = rePiScrollDown.ReplaceAllLiteralString(l, "")
		if loc := rePiVimMode.FindStringIndex(l); loc != nil {
			l = l[:loc[0]] + l[loc[1]:]
		}
		if !reBlank.MatchString(l) {
			return true
		}
	}
	return false
}

// PiWorkingRow is _pi_working_row.
func PiWorkingRow(text string) bool {
	return anyMatch(strings.Split(text, "\n"), rePiWorkingRow)
}

// PiWorkingLabel is _pi_working_label.
func PiWorkingLabel(text string) bool {
	if anyMatch(Tail(text, 30), rePiWorkingRule) {
		return true
	}
	_, above, ok := BoxRows(text, FirstRowAny)
	return ok && anyMatch(above, rePiWorkingRow)
}

// PiIdleBox is _pi_idle_box.
func PiIdleBox(text string) bool {
	if PiLiveTurn(text) {
		return false
	}
	row, above, ok := BoxRows(text, FirstRowAny)
	return ok && !reOption.MatchString(row) && !anyMatch(above, rePiWorkingRow)
}

// piBox locates the editor box in the CSI-stripped rows of text, blank rows
// kept, with BoxRows' bounds: a and b index the upper and lower rules.
func piBox(text string) (lines []string, a, b int, ok bool) {
	var nonBlank []int
	for i, l := range strings.Split(text, "\n") {
		s := reCSI.ReplaceAllLiteralString(l, "")
		lines = append(lines, s)
		if !reBlank.MatchString(s) {
			nonBlank = append(nonBlank, i)
		}
	}
	n := len(nonBlank)
	off := max(n-30, 0)
	ai, bi := -1, -1
	for i := n - 1; i >= off; i-- {
		if !strings.HasPrefix(lines[nonBlank[i]], "─") {
			continue
		}
		if bi < 0 {
			bi = i
			continue
		}
		ai = i
		break
	}
	if ai < 0 || bi-ai-1 > 12 || n-1-bi < 1 || n-1-bi > 5 {
		return nil, 0, 0, false
	}
	return lines, nonBlank[ai], nonBlank[bi], true
}

// PiBoxOccupied reports an idle pi box that holds text: a held continue line
// or a human draft.
func PiBoxOccupied(text string) bool {
	if !PiIdleBox(text) {
		return false
	}
	lines, a, b, ok := piBox(text)
	return ok && b-a > 1 && anyNonBlank(lines[a+1:b])
}

func anyNonBlank(lines []string) bool {
	for _, l := range lines {
		if !reBlank.MatchString(l) {
			return true
		}
	}
	return false
}

// PiStopped classifies why an idle pi turn ended without finishing: pi paints
// "Response was truncated before completion." (stopReason length) or
// "Error: <provider message>" (stopReason error) just above an empty editor
// box. It returns "truncated", "provider-error" or "".
func PiStopped(text string) string {
	if IsPrompt("pi", text) || !PiIdleBox(text) {
		return ""
	}
	lines, a, b, ok := piBox(text)
	if !ok || anyNonBlank(lines[a+1:b]) {
		return ""
	}
	end := a
	for end > 0 && reBlank.MatchString(lines[end-1]) {
		end--
	}
	block := piBlock(lines, end)
	if len(block) > 0 && rePiBugHint.MatchString(block[0]) {
		end -= len(block)
		for end > 0 && reBlank.MatchString(lines[end-1]) {
			end--
		}
		block = piBlock(lines, end)
	}
	for i, l := range block {
		if rePiBugHint.MatchString(l) {
			block = block[:i]
			break
		}
	}
	switch {
	case len(block) == 1 && rePiTruncated.MatchString(block[0]):
		return "truncated"
	case len(block) > 0 && len(block) <= 4 && rePiError.MatchString(block[0]) &&
		!reAborted.MatchString(strings.Join(block, "\n")):
		return "provider-error"
	}
	return ""
}

// piBlock is the contiguous non-blank run of at most 8 rows ending before
// index end.
func piBlock(lines []string, end int) []string {
	start := end
	for start > 0 && end-start < 8 && !reBlank.MatchString(lines[start-1]) {
		start--
	}
	return lines[start:end]
}

// PaneIdleReason is _pane_idle_reason with engine fixed to claude: idle, or
// the keep reason it prints.
func PaneIdleReason(plain, colored string, skipBg bool) (reason string, idle bool) {
	if IsPermissionPrompt("claude", plain) || IsPrompt("claude", plain) || IsQuotaSessionLimit(plain) {
		return "prompt on screen", false
	}
	if !skipBg && anyMatch(Tail(plain, 30), reBgWork) {
		return "background shell or monitor still running", false
	}
	if !ClaudeIdleBox(plain, colored, false) {
		return "live turn, unsent input, or no idle input box", false
	}
	if _, above, _ := BoxRows(plain, FirstRowClaude); !anyMatch(above, reDone) && !doneBehindPostTurn(plain) {
		return "no finished-turn marker above the input box", false
	}
	return "", true
}

// maxRecapRows bounds the wrapped rows of one recap.
const maxRecapRows = 12

// doneBehindPostTurn finds the finished-turn marker past the fixed window above
// the box, walking up through nothing but the hint, hook-reload and recap lines
// claude prints after a turn ends. A recap is one contiguous block that ends
// with its `(disable recaps in /config)` tail and starts at `※ recap:`. Any
// other row ends the walk, so the pane stays not-idle.
func doneBehindPostTurn(text string) bool {
	_, plain, a, _, off, ok := scanBox(text)
	if !ok {
		return false
	}
	inRecap, rows := false, 0
	for j := a - 1; j >= off; j-- {
		switch row := plain[j]; {
		case reDone.MatchString(row):
			return !inRecap
		case reClearHint.MatchString(row) || reHookReload.MatchString(row):
			if inRecap {
				return false
			}
		case reRecap.MatchString(row):
			if !inRecap && !reRecapEnd.MatchString(row) {
				return false
			}
			inRecap = false
		case !inRecap:
			if !reRecapEnd.MatchString(row) {
				return false
			}
			inRecap, rows = true, 0
		default:
			if rows++; rows >= maxRecapRows {
				return false
			}
		}
	}
	return false
}
