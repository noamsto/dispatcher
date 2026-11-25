// Package nudge types one fixed line into a worker lead's provably idle, empty
// input box and submits it. Two lines are ever typed: `crew inbox
// "$CREW_WORKER_ID"`, which points a lead its wake did not reach at an unread
// reason — the directive already posted with `crew reply`, or an overdue role
// verdict (`crew nudge`, and the watchdog's D6 auto-nudge) — and ContinueLine,
// which the watchdog's D9 offers a pi lead whose turn ended truncated or on a
// provider error.
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
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/tmuxstamp"
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
	MsgTS   string // the pointed-at msg's ms timestamp (a dispatcher directive or a role verdict); D9 types with "0"
	Line    string // what to type; empty is the inbox line
	// Agent carries the stability samples of a caller that re-checks (the --wait
	// loop, the watchdog); nil is a single look, which never accepts an agent-finished frame.
	Agent *AgentSample
}

// agentStableSecs is how long an agent-finished frame must hold byte-identical
// before it counts as idle. A lead that is about to start its next turn paints the
// spinner within a few seconds of the completion row, so ten seconds of stillness
// separates a parked lead from one mid-handoff without stalling a nudge long.
const agentStableSecs = 10

// Refusals for the agent-finished frame. ReasonAgentOnce is a single look;
// ReasonAgentPending is a re-checking caller still waiting out the stable window,
// which --wait treats like a live turn.
const (
	ReasonAgentOnce    = "background agent finished, no done marker: idle not yet proven — re-run with --wait to confirm the frame holds still"
	ReasonAgentPending = "background agent finished, no done marker: confirming the frame holds still"
)

// AgentSample is the first sighting of the current agent-finished frame.
type AgentSample struct {
	frame string
	at    int64
}

// confirm records the frame and reports whether it has held byte-identical for
// agentStableSecs. A nil receiver is a one-shot caller.
func (s *AgentSample) confirm(plain string, now int64) (string, bool) {
	if s == nil {
		return ReasonAgentOnce, false
	}
	if s.frame != plain {
		s.frame, s.at = plain, now
		return ReasonAgentPending, false
	}
	if now-s.at < agentStableSecs {
		return ReasonAgentPending, false
	}
	return "", true
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
// in-process. Tmux reads and types into panes; Events is the whole bus log, strictly decoded,
// for the anchor's session fold; Append posts one bus row; Clock stamps the row and
// pauses between the keystrokes.
type Deps struct {
	Tmux   tmux.Client
	Events func() ([]any, error)
	Append func(row any) error
	Sleep  func(ctx context.Context, seconds string) error
	Getenv func(string) string
	Clock  clock.Clock
	// Warn is what the caller must see but stdout cannot carry: the arm's stdout is
	// one outcome line, so a bus row that could not be written would otherwise be
	// invisible.
	Warn func(string)
	// Delivered reports whether the lead's read mark has advanced past req's msg; nil
	// skips the check, as the watchdog's callers do.
	Delivered func(ctx context.Context, req PaneRequest) bool
}

// Pane types one line into a provably idle, empty input box and submits it. It prints
// one line and returns 0 accepted, 2 refused before typing (nothing typed, no bus row;
// an anchor-gate refusal starts with `anchor:`), or 3 typed but not accepted (held,
// unknown, unconfirmed, or `typed, not delivered`: the pane took the line but the
// lead's read mark never advanced). Every typed attempt appends exactly one `kind:"nudge"` row; a
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
	gate := frameGate(ctx, d, req.Pane, req.Engine)
	if !gate.ok {
		return gate.why, exitRefused
	}
	if gate.agentFrame != "" {
		if why, ok := req.Agent.confirm(gate.agentFrame, d.Clock.Seconds()); !ok {
			return why, exitRefused
		}
	}
	if why, ok := anchorGate(ctx, d, req); !ok {
		return why, exitRefused
	}
	// A frame that changed since the stability proof is a turn that started.
	if again := frameGate(ctx, d, req.Pane, req.Engine); !again.ok || again.agentFrame != gate.agentFrame {
		if again.ok {
			return ReasonAgentPending, exitRefused
		}
		return again.why, exitRefused
	}
	if _, err := d.Tmux.Run(ctx, "send-keys", "-t", req.Pane, "-l", req.text()); err != nil {
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
			if !delivered(ctx, d, req) {
				result = "typed, not delivered"
				detail = "the lead took the line but its read mark has not advanced past the msg; it may still read it (crew where, or re-check later), never re-typed"
			}
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

// delivered polls d.Delivered, the first check immediate: a lead that took the line
// still has to run `crew inbox` before its mark moves. The bound is a try count.
func delivered(ctx context.Context, d Deps, req PaneRequest) bool {
	if d.Delivered == nil {
		return true
	}
	tries, err := strconv.Atoi(envOr(d, "CREW_NUDGE_DELIVER", "20"))
	if err != nil || tries < 1 {
		tries = 20
	}
	for i := 0; i < tries; i++ {
		if i > 0 && d.Sleep(ctx, "1") != nil {
			break
		}
		if d.Delivered(ctx, req) {
			return true
		}
	}
	return false
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
	_, err := d.Tmux.Run(ctx, "send-keys", "-t", pane, "Enter")
	return err == nil
}

// capture is the sampler's read of a pane. A failed colored capture is "" and
// not ok, which is what `pane unreadable` means.
func capture(ctx context.Context, d Deps, pane string, colored bool) (string, bool) {
	text, ok := probe.Capture(ctx, d.Tmux, d.Getenv, pane, colored)
	return text, ok && (!colored || text != "")
}

// gateResult is the frame gate's verdict: ok, or why not. agentFrame is set when the
// frame is idle only on the strength of a background-agent completion row, which a
// caller must see hold still before typing.
type gateResult struct {
	why        string
	ok         bool
	agentFrame string
}

// frameGate is `_nudge_frame_gate`: the pane shows an idle, empty input box, or the
// reason it does not.
func frameGate(ctx context.Context, d Deps, pane, engine string) gateResult {
	plain, _ := capture(ctx, d, pane, false)
	colored, _ := capture(ctx, d, pane, true)
	if plain == "" {
		return gateResult{why: "pane unreadable"}
	}
	if engine != "claude" {
		// pi: the option-select frame, then a live turn or no box, then a box
		// holding something the lead has not sent.
		switch {
		case frame.IsPrompt(engine, plain):
			return gateResult{why: "prompt on screen"}
		case !frame.PiIdleBox(plain):
			return gateResult{why: "live turn or no idle input box"}
		case boxText(plain, engine) != "":
			return gateResult{why: "unsent input in the input box"}
		}
		return gateResult{ok: true}
	}
	// skipBg: a detached background shell is no reason to refuse a directive, so an
	// idle box passes with one on screen.
	// An Interrupted turn has no finished-turn marker but waits for direction.
	// An Interrupted turn has no finished-turn marker but waits for direction; an
	// agent-finished row is idle only once the caller has seen it hold still.
	if why, idle := frame.PaneIdleReason(plain, colored, true); !idle {
		if why == "no finished-turn marker above the input box" {
			switch {
			case frame.InterruptedAboveBox(plain):
				return gateResult{ok: true}
			case frame.AgentFinishedAboveBox(plain):
				return gateResult{ok: true, agentFrame: plain}
			}
		}
		return gateResult{why: busyReason(plain, colored, why)}
	}
	return gateResult{ok: true}
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
	wins, err := tmuxstamp.Windows(ctx, d.Tmux)
	if err != nil {
		return "anchor: cannot read tmux windows", false
	}
	panes, err := tmuxstamp.Panes(ctx, d.Tmux)
	if err != nil {
		return "anchor: cannot read tmux panes", false
	}
	pi := slices.IndexFunc(panes, func(p tmuxstamp.Pane) bool { return p.ID == req.Pane })
	if pi < 0 {
		return "anchor: no pane " + req.Pane, false
	}
	wid := panes[pi].Window
	// A stamp that cannot be read cleanly reads as "", which matches no crew or
	// branch; a role that cannot is neither the lead's nor unlabelled.
	var wcrew, wbranch string
	if slices.ContainsFunc(wins, func(w tmuxstamp.Window) bool { return w.ID == wid }) {
		var err1, err2 error
		wcrew, _, err1 = tmuxstamp.WindowOption(ctx, d.Tmux, wid, "@crew_id")
		wbranch, _, err2 = tmuxstamp.WindowOption(ctx, d.Tmux, wid, "@crew_branch")
		if err := errors.Join(err1, err2); err != nil {
			return "anchor: cannot read tmux window stamps", false
		}
	}
	if req.Crew == "" || wcrew != req.Crew {
		return "anchor: window " + wid + " of " + req.Pane + " is not anchored to crew " + req.Crew, false
	}
	if wbranch != branch {
		return "anchor: window " + wid + " of " + req.Pane + " is not " + branch + "'s window", false
	}
	role, ok, err := tmuxstamp.PaneOption(ctx, d.Tmux, req.Pane, "@crew_role")
	if err != nil {
		return "anchor: cannot read tmux pane stamps", false
	}
	if !ok {
		role = "?"
	}
	if role != "lead" {
		// A window with a single unlabelled pane is the lead's: that is what a
		// pre-role worker window looks like.
		n := 0
		for _, p := range panes {
			if p.Window == wid {
				n++
			}
		}
		if role != "" || n != 1 {
			return "anchor: " + req.Pane + " is not the lead pane of " + wid, false
		}
	}
	cmd, _, err := tmuxstamp.Field(ctx, d.Tmux, req.Pane, "#{pane_current_command}")
	if err != nil {
		return "anchor: cannot read tmux pane command", false
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
