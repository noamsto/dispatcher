// Package nudge types one fixed line into a worker lead's provably idle, empty
// input box and submits it. Two lines are ever typed: `crew inbox
// "$CREW_WORKER_ID"`, which points a lead whose wake expired at the directive
// already posted with `crew reply` (`crew nudge`, and the watchdog's D6
// auto-nudge), and ContinueLine, which the watchdog's D9 offers a pi lead whose
// turn ended truncated or on a provider error.
//
// Pane is the whole decision — frame gate, anchor gate, type, confirm, Enter,
// classify, one bus row — and it is exported because the arm and `internal/stall`
// both need it: the type/no-type call is allowed exactly one implementation.
//
// Typing into a sibling's pane is the dispatcher's call alone, so Run refuses a caller
// that is itself a worker or role session. Exit codes are the arm's: 0 accepted, 1 usage
// or resolution error, 2 refused before typing, 3 typed but not accepted.
package nudge

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
)

// line is `_nudge_line`, typed literally: the lead's own shell expands the variable its
// engine environment exports, so nothing bus- or branch-derived is ever typed.
const line = `crew inbox "$CREW_WORKER_ID"`

// ContinueLine is what D9 offers a pi lead stopped by a truncated response or a
// provider error. Constant for the same reason as line: the watchdog never
// types anything derived from the bus, and msg_ts 0 on its row keeps it from
// ever covering a real directive.
const ContinueLine = "continue from where you stopped; if a command's result is missing, re-run it"

const (
	exitFailure = 1 // the arm's `_nudge_fail`: the line is the message
	exitRefused = 2 // `_nudge_refuse`: refused before typing
	exitTyped   = 3 // typed but not accepted
)

// PaneRequest is what a caller must know to type into one lead's pane.
type PaneRequest struct {
	Pane    string
	Engine  string
	Session string // the sessioned worker id, `worker:<branch>#s<epoch>-<pid>`
	Crew    string
	Actor   string // `dispatcher:<crew>` from the arm, `watchdog` from D6 and D9
	MsgTS   string // the unread directive's ms timestamp; D9 types with "0"
	Line    string // what to type; empty is the inbox line
}

func (r PaneRequest) text() string {
	if r.Line != "" {
		return r.Line
	}
	return line
}

// key is the typed line with all whitespace dropped, because boxText compares
// whitespace-free: a box the engine wrapped mid-word still reads back as this.
func (r PaneRequest) key() string {
	return strings.Join(strings.Fields(r.text()), "")
}

// Deps are the pane's side effects, injected so every refusal path is testable
// in-process. Capture is the sampler's read of a pane, plain or colored (ok=false is a
// failed capture, which is what `pane unreadable` means); Tmux runs a tmux argv and
// reports whether it ran and its stdout; Events is the whole bus log, strictly decoded,
// for the anchor's session fold; Append posts one bus row; Clock stamps the row and
// pauses between the keystrokes.
type Deps struct {
	Capture func(ctx context.Context, pane string, colored bool) (string, bool)
	Tmux    func(ctx context.Context, args ...string) (string, bool)
	Events  func() ([]any, error)
	Append  func(row any) error
	Sleep   func(ctx context.Context, seconds string) error
	Getenv  func(string) string
	Clock   clock.Clock
	// Warn is what the caller must see but stdout cannot carry: the arm's stdout is
	// one outcome line, so a bus row that could not be written would otherwise be
	// invisible.
	Warn func(string)
}

// Pane types one line into a provably idle, empty input box and submits it. It prints
// one line and returns 0 accepted, 2 refused before typing (nothing typed, no bus row;
// an anchor-gate refusal starts with `anchor:`), or 3 typed but not accepted (held,
// unknown, unconfirmed). Every typed attempt appends exactly one `kind:"nudge"` row; a
// bus that will not take it is warned about, never returned. It never retries the text
// or the Enter: whatever the pane shows instead may be a human's.
func Pane(ctx context.Context, req PaneRequest, d Deps) (string, int) {
	if !bus.IsSessionID(req.Session) {
		return "'" + req.Session + "' is not a sessioned worker id", exitRefused
	}
	if req.MsgTS == "" || !ascii.IsDigits(req.MsgTS) {
		return "msg ts '" + req.MsgTS + "' is not a ms timestamp", exitRefused
	}
	if req.Engine != "claude" && req.Engine != "pi" {
		who := req.Engine
		if who == "" {
			who = "an unknown engine"
		}
		return "no verified idle lead frame for " + who, exitRefused
	}

	// The frame gate runs before the anchor gate: the anchor's session fold reads
	// the whole bus log, and a busy lead is the common refusal. It runs again right
	// before typing, since that read leaves time for a human to start typing.
	if why, ok := frameGate(ctx, d, req.Pane, req.Engine); !ok {
		return why, exitRefused
	}
	if why, ok := anchorGate(ctx, d, req); !ok {
		return why, exitRefused
	}
	if why, ok := frameGate(ctx, d, req.Pane, req.Engine); !ok {
		return why, exitRefused
	}
	if _, ok := d.Tmux(ctx, "send-keys", "-t", req.Pane, "-l", req.text()); !ok {
		return "send-keys failed", exitRefused
	}

	// Enter only once the line provably sits alone in a still-idle box: an Enter
	// into anything else could answer a dialog or submit a human's text. The settle
	// lets the engine redraw the typed text before it is read back.
	_ = d.Sleep(ctx, envOr(d, "CREW_NUDGE_SETTLE", "1"))
	p2, _ := capture(ctx, d, req.Pane, false)
	c2, _ := capture(ctx, d, req.Pane, true)
	entry := false
	if !frame.IsPrompt(req.Engine, p2) && boxText(p2, req.Engine) == req.key() {
		if req.Engine == "claude" {
			entry = !frame.IsPermissionPrompt("claude", p2) && frame.ClaudeIdleBox(p2, c2, true)
		} else {
			entry = frame.PiIdleBox(p2)
		}
	}

	var result, detail string
	switch {
	case !entry:
		result = "unconfirmed"
		detail = "typed but not confirmed in the input box; no Enter sent — the text may be left as an unsent draft a human must clear (crew where), never re-typed"
	case !sendEnter(ctx, d, req.Pane):
		result = "unknown"
		detail = "typed and confirmed, but the Enter failed; the line may sit in the input box (crew where), never re-typed"
	default:
		_ = d.Sleep(ctx, envOr(d, "CREW_NUDGE_GAP", "2"))
		switch result = classify(ctx, d, req); result {
		case "accepted":
			detail = "the lead took the line (live turn or emptied input box)"
		case "held":
			detail = "the line still sits in the input box after one Enter; not re-sent — a human must submit or clear it (crew where)"
		default:
			result = "unknown"
			detail = "the pane after Enter is neither a live turn nor the held line; verify it (crew where), never re-typed"
		}
	}

	out := "nudge " + result + ": " + req.Pane + " " + req.Session + " — " + detail
	// The outcome line and its 0/3 stand even when the bus will not take the row: the
	// pane really was typed, and the held/unconfirmed/unknown line telling a human to
	// clear it (crew where) is what the exit code is for.
	if err := d.Append(nudgeRow(req, result, detail, d.Clock.RealMS())); err != nil && d.Warn != nil {
		d.Warn("crew: nudge: the nudge row could not be written: " + err.Error())
	}
	if result != "accepted" {
		return out, exitTyped
	}
	return out, 0
}

// classify reads the pane after Enter: a live turn or an emptied box is `accepted`, the
// line still sitting in the box is `held`, and anything else is the empty string (the
// arm's `unknown` result).
func classify(ctx context.Context, d Deps, req PaneRequest) string {
	p3, _ := capture(ctx, d, req.Pane, false)
	c3, _ := capture(ctx, d, req.Pane, true)
	if req.Engine == "claude" {
		// Any of these is a live turn: the meter row, its subrow, the interrupt
		// hint, a spinner above the box, or a box idle again (the line was taken and
		// the turn finished inside the gap).
		tail := strings.Join(frame.Tail(p3, 30), "\n")
		_, above, boxOK := frame.BoxRows(p3, frame.FirstRowClaude)
		switch {
		case frame.MeterLine(tail) != "", frame.HasSubrow(tail),
			strings.Contains(tail, "esc to interrupt"),
			boxOK && frame.HasSpinnerRow(above),
			frame.ClaudeIdleBox(p3, c3, false):
			return "accepted"
		case frame.ClaudeIdleBox(p3, c3, true) && boxText(p3, req.Engine) == req.key():
			return "held"
		}
		return ""
	}
	if frame.PiWorkingLabel(p3) {
		return "accepted"
	}
	if frame.PiIdleBox(p3) {
		switch boxText(p3, req.Engine) {
		case "":
			return "accepted"
		case req.key():
			return "held"
		}
	}
	return ""
}

// nudgeRow is the arm's `kind:"nudge"` row. `ts` is jq's `now*1000|floor` — real time,
// deliberately not `CREW_CLOCK` — and every other field is the caller's. Key order is
// not part of the row's value (docs/crew-go-port.md's output contract).
func nudgeRow(req PaneRequest, result, detail string, ts int64) nudgeRowDoc {
	return nudgeRowDoc{
		TS:     ts,
		CrewID: req.Crew,
		Kind:   "nudge",
		From:   req.Actor,
		To:     req.Session,
		Branch: branchOf(req.Session),
		Pane:   req.Pane,
		Engine: req.Engine,
		Result: result,
		Detail: detail,
		MsgTS:  json.Number(digitsLiteral(req.MsgTS)),
	}
}

// nudgeRowDoc is the bus row nudgeRow builds. MsgTS is a number literal, not a string:
// the bus orders rows by it.
type nudgeRowDoc struct {
	TS     int64       `json:"ts"`
	CrewID string      `json:"crew_id"`
	Kind   string      `json:"kind"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Branch string      `json:"branch"`
	Pane   string      `json:"pane"`
	Engine string      `json:"engine"`
	Result string      `json:"result"`
	Detail string      `json:"detail"`
	MsgTS  json.Number `json:"msg_ts"`
}

// digitsLiteral is what `jq --argjson` makes of a digit string: the value, so leading
// zeros are gone and every other digit is kept exactly.
func digitsLiteral(digits string) string {
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

// envOr is `${CREW_NUDGE_SETTLE:-1}` and friends: read per call, as the arm does.
func envOr(d Deps, name, def string) string {
	if v := d.Getenv(name); v != "" {
		return v
	}
	return def
}

// branchOf is the anchor's `${sid%#*}` then `${…#worker:}`: the branch a session id
// addresses, a `#` inside the branch name itself intact.
func branchOf(sid string) string {
	return strings.TrimPrefix(strings.TrimSuffix(sid, lastHash(sid)), "worker:")
}

// sendEnter is the arm's second send-keys; its failure is the `unknown` result, since
// the typed text may still sit in the box.
func sendEnter(ctx context.Context, d Deps, pane string) bool {
	_, ok := d.Tmux(ctx, "send-keys", "-t", pane, "Enter")
	return ok
}

func capture(ctx context.Context, d Deps, pane string, colored bool) (string, bool) {
	if d.Capture == nil {
		return "", false
	}
	return d.Capture(ctx, pane, colored)
}

// frameGate is `_nudge_frame_gate`: the pane shows an idle, empty input box, or the
// reason it does not.
func frameGate(ctx context.Context, d Deps, pane, engine string) (string, bool) {
	plain, _ := capture(ctx, d, pane, false)
	colored, _ := capture(ctx, d, pane, true)
	if plain == "" {
		return "pane unreadable", false
	}
	if engine != "claude" {
		// pi: the option-select frame, then a live turn or no box, then a box
		// holding something the lead has not sent.
		switch {
		case frame.IsPrompt(engine, plain):
			return "prompt on screen", false
		case !frame.PiIdleBox(plain):
			return "live turn or no idle input box", false
		case boxText(plain, engine) != "":
			return "unsent input in the input box", false
		}
		return "", true
	}
	// skipBg: a detached background shell is no reason to refuse a directive, so an
	// idle box passes with one on screen.
	if why, idle := frame.PaneIdleReason(plain, colored, true); !idle {
		return busyReason(plain, colored, why), false
	}
	return "", true
}

// busyReason is `_nudge_busy_reason`: a claude lead's idle-frame refusal, narrowed to
// what is actually on screen.
func busyReason(plain, colored, why string) string {
	switch why {
	case "prompt on screen":
		switch {
		case frame.IsPermissionPrompt("claude", plain):
			return "permission dialog on screen"
		case frame.IsQuotaPrompt(plain) || frame.IsQuotaSessionLimit(plain):
			return "quota frame on screen"
		}
		return "dialog on screen (option-select or workspace trust)"
	case "live turn, unsent input, or no idle input box":
		// Only a non-empty, non-ghost row passes the own-text form after the
		// plain form failed.
		if frame.ClaudeIdleBox(plain, colored, true) {
			return "unsent input in the input box"
		}
		return "live turn or no idle input box"
	}
	return why
}

// anchorGate is the arm's anchor block: the pane is the lead of this crew's window for
// the session's branch, still runs an engine, and the session is that branch's live
// newest. Every refusal is prefixed `anchor:`, which is what tells a watchdog to stop
// nudging this directive rather than retry it.
func anchorGate(ctx context.Context, d Deps, req PaneRequest) (string, bool) {
	branch := strings.TrimPrefix(strings.TrimSuffix(req.Session, lastHash(req.Session)), "worker:")
	wins, ok := d.Tmux(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{@crew_branch}\t#{@crew_id}")
	if !ok {
		return "anchor: cannot read tmux windows", false
	}
	panes, ok := d.Tmux(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{@crew_role}\t#{pane_current_command}")
	if !ok {
		return "anchor: cannot read tmux panes", false
	}
	rows := tsvRows(panes)
	prow := firstWhere(rows, func(r []string) bool { return field(r, 1) == req.Pane })
	if prow == nil {
		return "anchor: no pane " + req.Pane, false
	}
	wid, role, cmd := field(prow, 0), field(prow, 2), field(prow, 3)
	wrow := firstWhere(tsvRows(wins), func(r []string) bool { return field(r, 0) == wid })
	if req.Crew == "" || field(wrow, 2) != req.Crew {
		return "anchor: window " + wid + " of " + req.Pane + " is not anchored to crew " + req.Crew, false
	}
	if field(wrow, 1) != branch {
		return "anchor: window " + wid + " of " + req.Pane + " is not " + branch + "'s window", false
	}
	if role != "lead" {
		// A window with a single unlabelled pane is the lead's: that is what a
		// pre-role worker window looks like.
		n := 0
		for _, r := range rows {
			if field(r, 0) == wid {
				n++
			}
		}
		if role != "" || n != 1 {
			return "anchor: " + req.Pane + " is not the lead pane of " + wid, false
		}
	}
	if !roster.IsEngineCmd(cmd) {
		who := cmd
		if who == "" {
			who = "no command"
		}
		return "anchor: " + req.Pane + " is not running an engine (" + who + ")", false
	}
	last, err := foldNewest(d, branch, req.Crew)
	if err != nil || last == nil || rowText(last, "worker_id") != req.Session || isTrue(last, "terminal") {
		return "anchor: " + req.Session + " is not the live newest session on " + branch, false
	}
	return "", true
}

// foldNewest is the sessions fold's newest entry, nil when the branch has none — a
// missing log is the fold's own `[]`, so it reads as no session rather than a failure.
// A log or fold that cannot be read is an error, which the anchor gate refuses around.
func foldNewest(d Deps, branch, crew string) (any, error) {
	events, err := d.Events()
	if err != nil {
		return nil, err
	}
	folded, err := sessions.Fold(events, branch, crew, float64(d.Clock.RealMS())/1000)
	if err != nil {
		return nil, err
	}
	out, ok := folded.([]any)
	if !ok || len(out) == 0 {
		return nil, nil
	}
	return out[len(out)-1], nil
}

// boxText is `_nudge_box_text`: the lead's input box contents, whitespace-free.
func boxText(text, engine string) string {
	first := frame.FirstRowAny
	if engine == "claude" {
		first = frame.FirstRowClaude
	}
	return frame.BoxText(text, first)
}

// lastHash is the `#`-prefixed suffix `${sid%#*}` strips — empty when the id has none.
func lastHash(sid string) string {
	if i := strings.LastIndex(sid, "#"); i >= 0 {
		return sid[i:]
	}
	return ""
}

func tsvRows(out string) [][]string {
	var rows [][]string
	for _, l := range strings.Split(out, "\n") {
		rows = append(rows, strings.Split(l, "\t"))
	}
	return rows
}

func firstWhere(rows [][]string, want func([]string) bool) []string {
	for _, r := range rows {
		if want(r) {
			return r
		}
	}
	return nil
}

func field(row []string, i int) string {
	if i < len(row) {
		return row[i]
	}
	return ""
}

func isTrue(v any, key string) bool {
	m, _ := v.(map[string]any)
	b, _ := m[key].(bool)
	return b
}
