// Package frame classifies tmux pane captures the way the bash predicates in
// adapters/core/crew.sh (_frame_classifier, _pane_idle_reason) do: prompt,
// quota, background-wait, meter and input-box shapes of the claude, codex, pi
// and cursor panes. Every function takes the sampler's stdout with trailing
// newlines stripped, as `printf '%s\n' "$1"` presents it, and engine replaces
// the bash predicates' read of the global $engine.
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

	rePiScrollUp   = regexp.MustCompile(`↑ [0-9]+ more`)
	rePiScrollDown = regexp.MustCompile(`↓ [0-9]+ more`)
	rePiVimMode    = posixLongest(`[[:space:]]+(INSERT|NORMAL|EX|VISUAL|V-LINE)([[:space:]].*)?[[:space:]]*$`)
	// The bash originals run these two under LC_ALL=C, so their space class
	// is ASCII.
	rePiWorkingRow  = regexp.MustCompile(`^[\t\n\v\f\r ]*[\x{2800}-\x{28FF}].*Working`)
	rePiWorkingRule = regexp.MustCompile(`^─.*(Working|[\x{2800}-\x{28FF}])`)
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

// tail is `printf '%s\n' "$text" | grep -v '^[[:space:]]*$' | tail -n`.
func tail(text string, n int) []string {
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
	return strings.Join(tail(text, 4), "\n") == hookReviewFrame
}

// IsPermissionPrompt is _is_permission_prompt.
func IsPermissionPrompt(engine, text string) bool {
	if engine != "claude" {
		return false
	}
	t := tail(text, 10)
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
	t := tail(text, 7)
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
	return anyContains(tail(text, 12), "Stop and wait for limit to reset")
}

// IsQuotaSessionLimit is _is_quota_session_limit.
func IsQuotaSessionLimit(text string) bool {
	t := tail(text, 15)
	return anyContains(t, "You've hit your session limit") &&
		anyContains(t, "/upgrade to increase your usage limit") &&
		anyContains(t[max(len(t)-6, 0):], "uses your weekly limit")
}

// IsQuotaCursorLimit is _is_quota_cursor_limit.
func IsQuotaCursorLimit(text string) bool {
	t := tail(text, 12)
	return anyContains(t, "You've reached your monthly usage limit") && anyContains(t, "spendLimitHit: true")
}

// IsBgWait is _is_bg_wait: a finished turn's `· done HH:MM` marker with a
// background shell still running and no live spinner or meter.
func IsBgWait(text string) bool {
	t := tail(text, 8)
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
	var raw, plain []string
	for _, l := range strings.Split(text, "\n") {
		s := reCSI.ReplaceAllLiteralString(l, "")
		if reBlank.MatchString(s) {
			continue
		}
		raw = append(raw, l)
		plain = append(plain, s)
	}
	n := len(plain)
	off := max(n-30, 0)
	a, b := -1, -1
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

// ClaudeIdleBox is _claude_idle_box; colored "" means no colored capture.
func ClaudeIdleBox(plain, colored string, own bool) bool {
	t := tail(plain, 30)
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
	for _, l := range tail(text, 30) {
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
	if anyMatch(tail(text, 30), rePiWorkingRule) {
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

// PaneIdleReason is _pane_idle_reason with engine fixed to claude: idle, or
// the keep reason it prints.
func PaneIdleReason(plain, colored string, skipBg bool) (reason string, idle bool) {
	if IsPermissionPrompt("claude", plain) || IsPrompt("claude", plain) || IsQuotaSessionLimit(plain) {
		return "prompt on screen", false
	}
	if !skipBg && anyMatch(tail(plain, 30), reBgWork) {
		return "background shell or monitor still running", false
	}
	if !ClaudeIdleBox(plain, colored, false) {
		return "live turn, unsent input, or no idle input box", false
	}
	if _, above, _ := BoxRows(plain, FirstRowClaude); !anyMatch(above, reDone) {
		return "no finished-turn marker above the input box", false
	}
	return "", true
}
