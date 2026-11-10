package rolewatch

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/frame"
)

type submit int

const (
	submitted submit = iota
	held
	dialog
	unknown
)

func (s submit) String() string {
	return [...]string{"submitted", "held", "dialog", "unknown"}[s]
}

var (
	rePasteMarker = regexp.MustCompile(`^\[paste #[0-9]+ ([0-9]{1,9}) chars\]$`)
)

// paneReady is the only gate in front of send-keys: fail closed on a dialog,
// prompt, quota stop, live turn or any frame an engine does not positively
// recognise as its empty composer. colored and own only matter to claude.
func paneReady(engine, plain, colored string, own bool) bool {
	if plain == "" || frame.IsPermissionPrompt(engine, plain) || frame.IsPrompt(engine, plain) {
		return false
	}
	switch engine {
	case "claude":
		return frame.ClaudeIdleBox(plain, colored, own)
	case "codex":
		return frame.CodexIdleBox(plain)
	case "cursor":
		return !frame.IsQuotaCursorLimit(plain) && frame.CursorIdleBox(plain)
	case "pi":
		return frame.PiIdleBox(plain)
	}
	return false
}

// assignmentConfirmed proves the pane holds the assignment this watcher just
// typed: codex and cursor by exact composer text, claude and pi by the ready
// check with own set so claude's ghost-vs-draft test accepts our own text.
func assignmentConfirmed(engine, plain, assignment, colored string) bool {
	if frame.IsPermissionPrompt(engine, plain) || frame.IsPrompt(engine, plain) {
		return false
	}
	switch engine {
	case "codex":
		return frame.CodexComposer(plain, "Assignment: "+assignment)
	case "cursor":
		return frame.CursorComposer(plain, "Assignment: "+assignment)
	}
	return paneReady(engine, plain, colored, true)
}

// submitState classifies the pane after an Enter. Only held is re-Entered; a
// dialog belongs to the dispatcher; unknown escalates after a few ticks.
func submitState(engine, plain, assignment, colored string) submit {
	if frame.IsPermissionPrompt(engine, plain) || frame.IsPrompt(engine, plain) {
		return dialog
	}
	if engine == "cursor" && frame.IsQuotaCursorLimit(plain) {
		return unknown
	}
	full := "Assignment: " + assignment
	switch engine {
	case "codex":
		switch {
		case frame.CodexComposer(plain, full):
			return held
		case frame.CodexLiveTurn(plain) || frame.CodexIdleBox(plain):
			return submitted
		}
	case "cursor":
		switch {
		case frame.CursorComposer(plain, full):
			return held
		case frame.CursorLiveTurn(plain) || frame.CursorIdleBox(plain):
			return submitted
		}
	case "claude":
		return claudeSubmitState(plain, full, colored)
	case "pi":
		return piSubmitState(plain, full)
	}
	return unknown
}

func claudeSubmitState(plain, full, colored string) submit {
	tail := strings.Join(frame.Tail(plain, 30), "\n")
	if frame.MeterLine(tail) != "" || frame.HasSubrow(tail) || strings.Contains(tail, "esc to interrupt") {
		return submitted
	}
	if frame.ClaudeIdleBox(plain, colored, false) {
		return submitted
	}
	row, above, ok := frame.BoxRows(plain, frame.FirstRowClaude)
	if ok && frame.HasSpinnerRow(above) {
		return submitted
	}
	if !frame.ClaudeIdleBox(plain, colored, true) {
		return unknown
	}
	row = frame.ClaudeDraft(row)
	if row != "" && (strings.HasPrefix(full, row) || strings.HasPrefix(row, "[Pasted text")) {
		return held
	}
	return unknown
}

func piSubmitState(plain, full string) submit {
	if frame.PiWorkingLabel(plain) {
		return submitted
	}
	if !frame.PiIdleBox(plain) {
		return unknown
	}
	row, _, _ := frame.BoxRows(plain, frame.FirstRowAny)
	row = frame.TrimSpace(row)
	if row == "" {
		return submitted
	}
	if strings.Contains(full, row) {
		return held
	}
	if m := rePasteMarker.FindStringSubmatch(row); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n == utf8.RuneCountInString(full) {
			return held
		}
	}
	return unknown
}
