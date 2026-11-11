package rolewatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/spill"
)

const (
	pendingMax = 50
	unknownMax = 5
	reguardMax = 3
)

var reCSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// entry is one queued assignment; ts is its bus ts, which a pull role's ack
// must postdate.
type entry struct {
	body, from string
	ts         int64
}

type watcher struct {
	ctx context.Context
	o   Options

	role, pane, branch, engine, interval string
	win, delivery, wCrew, crew, roleID   string
	submitRetries                        int
	deferNotice, since                   int64

	pending                                           []entry
	inflight, inflightFrom, leadWorker, leadID        string
	submitting                                        bool
	submitTries, unknownTicks, reguardTries, cooldown int
	escalated, retyped, verdictSeen, awaiting, unsent bool
	deferredSince                                     int64
	deferredTold, roleReady, roleBusy                 bool
	logSize                                           int64 // the bus size at the last read; -1 before one
}

// tmux keeps stdout even on failure, as `$(tmux … 2>/dev/null || true)` does.
// Every argv and the per-tick call order are the arm's: the dispatch bats stub
// matches argv and counts calls.
func (w *watcher) tmux(args ...string) string {
	out, _ := w.o.Tmux.Run(w.ctx, args...)
	return out
}

func (w *watcher) exited() bool {
	return w.tmux("display-message", "-p", "-t", w.pane, "#{@crew_exited}") == "1"
}

// setState never writes past @crew_exited, so a dead role cannot read as live.
func (w *watcher) setState(s string) {
	if w.exited() {
		return
	}
	w.tmux("set-option", "-p", "-t", w.pane, "@crew_state", s)
}

// stamped is F23/F24: a watcher can outlive a tmux server kill, and pane ids
// restart, so another repo's grid with the same branch and role could reuse it.
func (w *watcher) stamped() bool {
	f := fields(w.tmux("display-message", "-p", "-t", w.pane, "#{@crew_role}|#{window_id}"), 3)
	b := w.tmux("show-options", "-wqv", "-t", f[1], "@crew_branch")
	c := w.tmux("show-options", "-wqv", "-t", f[1], "@crew_id")
	return f[0] == w.role && b == w.branch && c == w.wCrew
}

// capture's plain frame is a second `$(…)` in the arm, so trailing newlines
// left by stripped rows go too; FooterComposerEqual needs the last row last.
func (w *watcher) capture() (plain, colored string) {
	colored = w.tmux("capture-pane", "-e", "-p", "-t", w.pane)
	return strings.TrimRight(reCSI.ReplaceAllString(colored, ""), "\n"), colored
}

// notify posts body once to each non-empty recipient, in order.
func (w *watcher) notify(body string, to ...string) {
	var sent []string
	for _, t := range to {
		if t == "" || slices.Contains(sent, t) {
			continue
		}
		sent = append(sent, t)
		w.o.Post(w.ctx, w.crew, w.roleID, t, body)
	}
}

func (w *watcher) dispatcher() string {
	if w.wCrew == "" {
		return ""
	}
	return "dispatcher:" + w.wCrew
}

func (w *watcher) loop() int {
	for w.ctx.Err() == nil && w.tmux("display-message", "-p", "-t", w.pane, "#{pane_id}") == w.pane {
		if w.exited() {
			break
		}
		w.intake()
		if !w.tick() {
			break
		}
		if err := w.o.Clock.SleepCtx(w.ctx, w.interval, w.o.Stderr); err != nil {
			if w.ctx.Err() != nil {
				return 0
			}
			// set -e ended the arm on a failed sleep.
			return 1
		}
	}
	return 0
}

// intake processes, in log order, the rows newer than since that are msgs to or
// from this role or its own statuses, then moves since to the newest of them.
func (w *watcher) intake() {
	// The bus is append-only, so an unchanged size means no new rows.
	fi, err := os.Stat(w.o.Log)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == w.logSize {
		return
	}
	evs, err := bus.ReadEventsTolerant(w.o.Log)
	var oe *bus.OpenError
	if errors.As(err, &oe) {
		return
	}
	w.logSize = fi.Size()
	next := w.since
	for _, e := range evs {
		obj, _ := e.Doc.(map[string]any)
		ts, ok := tsOf(obj)
		if !ok || ts <= w.since {
			continue
		}
		switch {
		case e.Kind == bus.KindStatus && e.From == w.roleID:
			w.onStatus(obj, ts)
		case e.Kind == bus.KindMsg && (e.To == w.roleID || e.From == w.roleID):
			w.onMsg(obj, ts)
		default:
			continue
		}
		next = max(next, ts)
	}
	w.since = next
}

// onStatus: any working status of a pull role is it up and pulling; one whose
// detail is `assignment:…` is the ack that clears every older entry.
func (w *watcher) onStatus(obj map[string]any, ts int64) {
	body, _ := obj["body"].(map[string]any)
	if w.delivery != "pull" || body["state"] != "working" {
		return
	}
	if !w.roleReady {
		w.roleReady, w.deferredSince = true, 0
	}
	if detail, _ := body["detail"].(string); !strings.HasPrefix(detail, "assignment:") {
		return
	}
	w.roleBusy = true
	if len(w.pending) == 0 {
		return
	}
	w.pending = slices.DeleteFunc(w.pending, func(p entry) bool { return p.ts < ts })
	w.deferredSince, w.deferredTold = 0, false
	w.setState("working")
}

func (w *watcher) onMsg(obj map[string]any, ts int64) {
	to := rawText(obj["to"])
	external := strings.HasPrefix(to, "dispatcher:")
	switch {
	case to == w.roleID:
		w.enqueue(obj, ts)
	case !external && w.delivery == "pull" && bodyEvent(obj["body"]) == "":
		// The role's verdict proves it pulled whatever preceded it.
		w.pending = slices.DeleteFunc(w.pending, func(p entry) bool { return p.ts <= ts })
		w.roleBusy, w.deferredSince = false, 0
		if len(w.pending) == 0 {
			w.setState("idle")
		}
	case !external && w.submitting:
		if bodyEvent(obj["body"]) != "assignment_unsubmitted" {
			w.verdictSeen = true
		}
	case !external && bodyEvent(obj["body"]) == "":
		w.awaiting = false
		if len(w.pending) == 0 {
			w.setState("idle")
		}
	}
}

// enqueue applies W1 before anything is queued.
func (w *watcher) enqueue(obj map[string]any, ts int64) {
	from := rawText(obj["from"])
	if !SenderAllowed(from, w.branch, w.wCrew) {
		w.notify(dropNotice(w.role, w.pane, from, w.roleID), w.dispatcher())
		return
	}
	if w.delivery != "pull" {
		// A typed role has no await to read the spilled file back, so the paste
		// carries the whole assignment.
		spill.Expand(obj, bus.Paths{Dir: filepath.Dir(w.o.Log)}, w.wCrew)
		if note, _ := obj[spill.NoteField].(string); note != "" {
			w.notify(bodyUnavailableNotice(w.role, w.pane, w.engine, note), from, w.dispatcher())
			obj["body"] = rawText(obj["body"]) + " [role-watch: " + note + "]"
		}
	}
	body := rawText(obj["body"])
	if body == "" {
		return
	}
	w.leadID = from
	if strings.HasPrefix(from, "worker:") {
		w.leadWorker = from
	}
	if w.delivery == "pull" {
		// A final release needs no ack, and the role never pulls it as work.
		if o, _ := fromJSON(body).(map[string]any); o["final"] == true {
			return
		}
	}
	if len(w.pending) >= pendingMax {
		w.pending = w.pending[1:]
	}
	w.pending = append(w.pending, entry{body, from, ts})
	if w.delivery != "pull" {
		w.setState("working")
	}
}

// tick is the arm's branch chain: at most one delivery or verification step.
// It is false when the watcher must exit.
func (w *watcher) tick() bool {
	switch {
	case w.submitting && w.cooldown > 0:
		w.cooldown--
	case w.submitting:
		return w.verify()
	case len(w.pending) == 0:
		w.deferredSince, w.deferredTold = 0, false
	case w.delivery == "pull" && w.roleBusy:
		w.deferredSince = 0
	case w.delivery == "pull":
		wait, detail := w.deferNotice, detailPullUnacked
		if !w.roleReady {
			wait, detail = w.deferNotice*5, detailPullUnstarted
		}
		to := w.leadWorker
		if to == "" {
			to = w.leadID
		}
		w.deferral(wait, to, deferredNotice(w.role, w.pane, w.engine, "pull", detail))
	case w.cooldown > 0:
		w.cooldown--
	case !w.exited():
		return w.deliver()
	}
	return true
}

// verify checks that the Enter submitted the inflight assignment; only Enter is
// ever re-sent, never the text.
func (w *watcher) verify() bool {
	plain, colored := w.capture()
	switch submitState(w.engine, plain, w.inflight, colored) {
	case submitted:
		w.inflight = ""
		w.submitting = false
		w.submitTries, w.unknownTicks, w.reguardTries = 0, 0, 0
		w.escalated, w.retyped = false, false
		w.cooldown = 1
		if w.verdictSeen && len(w.pending) == 0 {
			w.awaiting = false
			w.setState("idle")
		} else {
			w.awaiting = true
		}
		w.verdictSeen = false
	case dialog:
	case held:
		w.unknownTicks = 0
		switch {
		case w.submitTries < w.submitRetries:
			if !w.stamped() {
				w.orphanedUnlessDone()
				return false
			}
			w.tmux("send-keys", "-t", w.pane, "Enter")
			w.submitTries++
			w.cooldown = 1 << w.submitTries
		case w.engine == "pi" && !w.retyped && !w.escalated:
			// No C-u here: the next delivery sends one while unsent, and one now
			// would clear the re-paste before its Enter.
			w.pending = append([]entry{{w.inflight, w.inflightFrom, 0}}, w.pending...)
			w.inflight, w.inflightFrom = "", ""
			w.submitting = false
			w.submitTries = 0
			w.unsent, w.retyped = true, true
		case !w.escalated:
			w.escalate(detailStillInBox(w.submitRetries))
		}
	case unknown:
		// A pane squeezed to a sliver renders no known frame: restore its width
		// and re-sample before counting the tick against the assignment.
		if w.reguardWidth() {
			w.cooldown = 1
		} else {
			w.unknownTicks++
		}
		if w.unknownTicks >= unknownMax && !w.escalated {
			w.escalate(detailUnknownFrame)
		}
	}
	return true
}

// deliver types the head assignment into a positively idle pane, after F23.
func (w *watcher) deliver() bool {
	if !w.stamped() {
		w.orphanedUnlessDone()
		return false
	}
	plain, colored := w.capture()
	head := w.pending[0]
	switch {
	case !AssignmentSafe(head.body):
		w.notify(rejectedNotice(w.role, w.pane, w.engine), head.from, w.leadWorker)
		w.pending = w.pending[1:]
		w.deferredSince, w.deferredTold = 0, false
		// The drop is not a delivery: an earlier one may still await its verdict.
		if len(w.pending) == 0 && !w.awaiting {
			w.setState("idle")
		}
	case paneReady(w.engine, plain, colored, w.unsent):
		w.paste(head)
	default:
		w.deferral(w.deferNotice, w.leadID, deferredNotice(w.role, w.pane, w.engine, "typed", detailTypedDeferred))
	}
	return true
}

// paste is the load-buffer/paste-buffer round trip, the re-check, then Enter.
func (w *watcher) paste(head entry) {
	if w.unsent {
		w.tmux("send-keys", "-t", w.pane, "C-u")
	}
	buf := fmt.Sprintf("rw-assign-%d-%d", w.o.PID, w.o.Rand())
	if _, err := w.o.Tmux.RunInput(w.ctx, "Assignment: "+head.body, "load-buffer", "-b", buf, "-"); err != nil {
		// Nothing was typed, so unsent stays: set, the next tick's own-text
		// check could pass and its C-u wipe a real draft.
		return
	}
	if _, err := w.o.Tmux.Run(w.ctx, "paste-buffer", "-p", "-d", "-r", "-b", buf, "-t", w.pane); err != nil {
		// -d never ran and every attempt names a fresh buffer, so nothing
		// would reclaim this one; a stale idle frame must not earn an Enter.
		w.tmux("delete-buffer", "-b", buf)
		return
	}
	plain, colored := w.capture()
	if !assignmentConfirmed(w.engine, plain, head.body, colored) {
		w.unsent = true
		return
	}
	w.tmux("send-keys", "-t", w.pane, "Enter")
	w.inflight, w.inflightFrom = head.body, head.from
	w.pending = w.pending[1:]
	w.submitting = true
	w.submitTries, w.unknownTicks = 0, 0
	w.escalated, w.unsent = false, false
	w.cooldown = 1
	w.deferredSince, w.deferredTold = 0, false
}

// deferral starts the deferral clock and tells to once, wait seconds later.
func (w *watcher) deferral(wait int64, to, body string) {
	if w.deferredSince <= 0 {
		w.deferredSince = w.o.Clock.Seconds()
	}
	if !w.deferredTold && w.leadID != "" && w.o.Clock.Seconds()-w.deferredSince >= wait {
		w.deferredTold = true
		w.notify(body, to)
	}
}

func (w *watcher) escalate(detail string) {
	w.escalated = true
	w.notify(unsubmittedNotice(w.role, w.pane, w.engine, detail), w.inflightFrom, w.leadWorker, w.dispatcher())
}

// orphanedUnlessDone skips the notice when ctx is done: a cancelled ctx makes
// every tmux read empty, so the stamp check fails without the pane having
// changed.
func (w *watcher) orphanedUnlessDone() {
	if w.ctx.Err() == nil {
		w.orphaned()
	}
}

// orphaned tells whoever still waits on undelivered work before the exit.
func (w *watcher) orphaned() {
	if len(w.pending) == 0 && w.inflight == "" {
		return
	}
	var head string
	if len(w.pending) > 0 {
		head = w.pending[0].from
	}
	w.notify(orphanedNotice(w.role, w.pane, w.branch), w.inflightFrom, head, w.leadWorker, w.dispatcher())
}

// tsOf is a row's numeric ts. jq would order a non-number ts against since by
// type; crew never writes one, so such a row is skipped.
func tsOf(obj map[string]any) (int64, bool) {
	n, ok := obj["ts"].(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	return int64(math.Floor(f)), err == nil
}

// rawText is `jq -r '.x // ""'` of v.
func rawText(v any) string {
	if !jsonval.Truthy(v) {
		return ""
	}
	return jsonval.RawText(v)
}

// bodyEvent is `jq -r '.body | fromjson? | .event // ""'`.
func bodyEvent(body any) string {
	s, _ := body.(string)
	obj, _ := fromJSON(s).(map[string]any)
	return rawText(obj["event"])
}

// fromJSON is jq's fromjson: exactly one JSON value, else nil.
func fromJSON(s string) any {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil
	}
	return v
}
