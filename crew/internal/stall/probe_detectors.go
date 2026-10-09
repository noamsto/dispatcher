package stall

import (
	_ "embed"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

//go:embed nudged.jq
var nudgedProgram string

// probeDet is the probe-driven detectors' state across ticks.
type probeDet struct {
	d4At, d4Since, d5At, d6At, d8At int64
	d6Src                           string
	nudgedTS                        int64
	nudgeOff                        bool
}

// loadRe is D4's parse contract for a `_load_read` line, "load1 nproc".
var loadRe = regexp.MustCompile(`^[0-9]+([.][0-9]+)?[[:space:]]+[0-9]+$`)

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// probePre runs D4, D8, D5 and the role-mode end-of-life check, in the arm's
// order.
func (w *watch) probePre() error {
	for _, step := range []func() error{w.d4, w.d8, w.d5, w.roleEOL} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// shCall is Sh under the arm's signal semantics: a signal killed the arm in
// the middle of a call-out, so a call that returns into a cancelled context
// ends the watch instead of reading the cut-short answer as a verdict.
func (w *watch) shCall(op string, args ...string) (string, int, error) {
	out, rc := w.p.Sh(w.ctx, op, args...)
	if w.ctx.Err() != nil {
		return "", 0, exitCode(exitCodeFor(w.ctx))
	}
	return out, rc, nil
}

// d4 is host load. Engine-independent: it reads the host, not the pane.
// --load seconds of load1 above the core count posts one load: episode; it
// clears when load drops back to the cores. Never escalates. A malformed read
// stays silent that tick.
func (w *watch) d4() error {
	if w.suppressed || w.cfg.roleMode {
		return nil
	}
	line := w.p.Load(w.ctx)
	if !loadRe.MatchString(line) {
		return nil
	}
	f := strings.Fields(line)
	l1, cores := f[0], f[1]
	lv, _ := strconv.ParseFloat(l1, 64)
	cv, _ := strconv.ParseFloat(cores, 64)
	pd := &w.pd
	if lv <= cv {
		if pd.d4At != 0 {
			if err := w.postClear("load:"); err != nil {
				return err
			}
		}
		pd.d4At, pd.d4Since = 0, 0
		return nil
	}
	if pd.d4Since == 0 {
		pd.d4Since = w.now
	}
	if pd.d4At != 0 || w.now-pd.d4Since < w.cfg.loadWin {
		return nil
	}
	detail := "load: 1m load " + l1 + " on " + cores + " cores for " + strconv.FormatInt(w.now-pd.d4Since, 10) + "s"
	if top := w.p.Top(w.ctx); top != "" {
		lines := strings.SplitN(top, "\n", 3)
		detail += " (top: " + strings.Join(lines[:min(len(lines), 2)], " | ") + ")"
	}
	ok, err := w.postBlocked("load:", detail)
	if ok {
		pd.d4At = w.now
	}
	return err
}

// d8 is the engine budget. The launch gates refuse an exhausted engine only at
// dispatch time; this flags one that crosses the line while the worker runs.
// Bus-read cadence, and only while the row is live work or a watchdog episode:
// a pr_open run is finished, and a self-reported blocked is already parked in
// a zero-token await. A refresh can hold the tick up to 120s, so the bus is
// re-read after one; a cache that can't tell holds the episode as it is.
// Never escalates.
func (w *watch) d8() error {
	if !w.cfg.budgetOn || w.suppressed || w.tick%4 != 0 {
		return nil
	}
	ran, err := w.refreshMaybe()
	if err != nil {
		return err
	}
	if ran || w.busStale {
		if err := w.refresh(); err != nil {
			return err
		}
		w.busStale = false
	}
	if w.bus.state == "blocked" && w.bus.source != "watchdog" {
		w.suppressed = true
		return nil
	}
	switch w.bus.state {
	case "", "working", "blocked":
	default:
		return nil
	}
	detail, rc, err := w.budgetDetail()
	if err != nil {
		return err
	}
	switch {
	case rc == 0 && w.pd.d8At == 0:
		ok, err := w.postBlocked("budget:", detail)
		if ok {
			w.pd.d8At = w.now
		}
		return err
	case rc == 1 && w.pd.d8At != 0:
		if err := w.postClear("budget:"); err != nil {
			return err
		}
		w.pd.d8At = 0
	}
	return nil
}

// isShellCmd is `_is_shell_cmd`: only a bare interactive shell counts as
// "launch not started", so an empty or unknown command never raises D5.
func isShellCmd(cmd string) bool {
	switch strings.TrimPrefix(cmd, "-") {
	case "bash", "sh", "zsh", "fish", "dash", "ksh":
		return true
	}
	return false
}

// d5 is launch not started. dispatch posts `working` when it types the launch
// line and the launch script `exec`s the engine, so a pane still a shell
// --launch seconds into the watch means the binary is missing or the script
// failed. Once an engine has been seen this never fires again: an engine that
// exits later is quiet:/dead: territory.
func (w *watch) d5() error {
	if w.engineSeen || w.suppressed || w.cfg.roleMode {
		return nil
	}
	pcmd := w.p.PaneCmd(w.ctx)
	if pcmd != "" && roster.IsEngineCmd(pcmd) {
		w.engineSeen = true
		if w.pd.d5At != 0 {
			if err := w.postClear("stalled: launch-not-started"); err != nil {
				return err
			}
			w.pd.d5At = 0
		}
		return nil
	}
	if w.pd.d5At != 0 || w.now-w.start < w.cfg.launch || !isShellCmd(pcmd) {
		return nil
	}
	switch w.bus.state {
	case "", "working":
	default:
		return nil
	}
	detail := "stalled: launch-not-started — pane " + w.cfg.pane + " still a shell (" + strings.TrimPrefix(pcmd, "-") + ") " +
		strconv.FormatInt(w.cfg.launch, 10) + "s after launch; the engine is not running"
	ok, err := w.postBlocked("stalled: launch-not-started", detail)
	if ok {
		w.pd.d5At = w.now
	}
	return err
}

// roleEOL is the role-mode end-of-life exit. A role pane has no worker row to
// post a terminal status, so the watch exits once the engine returns to a bare
// shell — gated on having seen the engine, so a booting role pane is never
// mistaken for one that already exited.
func (w *watch) roleEOL() error {
	if !w.cfg.roleMode {
		return nil
	}
	pcmd := w.p.PaneCmd(w.ctx)
	if pcmd != "" && roster.IsEngineCmd(pcmd) {
		w.engineSeen = true
	} else if w.engineSeen && isShellCmd(pcmd) {
		return exitCode(0)
	}
	return nil
}

// readWords is `read -r a b <<<"$s"`: the first line split on blanks, the
// second name taking the rest.
func readWords(s string) (string, string) {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.Trim(line, " \t")
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimLeft(line[i:], " \t")
}

// msTS reads a scan's ms timestamp: digits only, base 10.
func msTS(s string) (int64, bool) {
	if !digitsRe.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// d6 is the unread detector. A lead that is `working` while a role's or the
// dispatcher's msg to its session sits past the delivered mark for --unread is
// a deadlock its repainting pane hides from D2/D3. The oldest of both is
// reported; a role msg the lead answered is handled, a dispatcher directive
// clears only on delivery. Never escalates.
//
// An overdue dispatcher directive first gets one auto-nudge per newest
// directive. An accepted nudge skips this tick's verdict, since the lead is
// reading its inbox now; a typed-but-unaccepted one is reported once as the
// episode; an anchor refusal means a stale pane or session, so nudging stops
// for good.
func (w *watch) d6() error {
	if w.suppressed || w.cfg.roleMode || w.tick%4 != 0 {
		return nil
	}
	pd := &w.pd
	skip, err := w.autoNudge()
	if err != nil || skip {
		return err
	}
	switch w.bus.state {
	case "", "working":
		if w.bus.source != "watchdog" {
			pd.d6At = 0
		}
		oldest, src, ok, err := w.unreadOldest()
		if err != nil {
			return err
		}
		if ok && w.now*1000-oldest >= w.cfg.unread*1000 {
			age := strconv.FormatInt((w.now*1000-oldest)/1000, 10)
			detail := "unread: role verdict undelivered for " + age + "s — lead is working but has not read it; nudge it to run `crew await`"
			if src == "dispatcher" {
				detail = "unread: dispatcher directive undelivered for " + age + "s — lead is working but has not reached a peek seam (long stage or idle on a background task)"
			}
			if pd.d6At != 0 {
				return nil
			}
			posted, err := w.postBlocked("unread:", detail)
			if posted {
				pd.d6At, pd.d6Src = w.now, src
			}
			return err
		}
		if pd.d6At != 0 {
			if err := w.postClear("unread:"); err != nil {
				return err
			}
			pd.d6At = 0
		}
	case "blocked":
		// Our own open episode still clears once the msg is delivered; a changed
		// oldest source means the label is stale, and the next tick re-posts.
		if pd.d6At == 0 || w.bus.source != "watchdog" {
			return nil
		}
		oldest, src, ok, err := w.unreadOldest()
		if err != nil {
			return err
		}
		if !ok || w.now*1000-oldest < w.cfg.unread*1000 || src != pd.d6Src {
			if err := w.postClear("unread:"); err != nil {
				return err
			}
			pd.d6At = 0
		}
	}
	return nil
}

// unreadOldest is `_unread_oldest`. A branch-keyed watch cannot name the
// lead's session, so `_unread_scan` stays silent for it; that check is made
// here too, so such a watch spawns nothing every fourth tick.
func (w *watch) unreadOldest() (int64, string, bool, error) {
	if w.cfg.fromID == w.cfg.me {
		return 0, "", false, nil
	}
	out, _, err := w.shCall("unread", w.cfg.crew, w.cfg.branch, w.cfg.me, w.cfg.fromID, strconv.FormatInt(w.cfg.runStartMS, 10), "oldest")
	if err != nil {
		return 0, "", false, err
	}
	a, src := readWords(out)
	oldest, ok := msTS(a)
	return oldest, src, ok, nil
}

// autoNudge is D6's nudge half; true skips this tick's verdict.
func (w *watch) autoNudge() (bool, error) {
	pd := &w.pd
	nudge := false
	switch w.bus.state {
	case "", "working":
		nudge = true
	case "blocked":
		nudge = pd.d6At != 0 && w.bus.source == "watchdog" && strings.HasPrefix(w.bus.detail, "unread:")
	}
	if !nudge || !w.cfg.nudgeOn || pd.nudgeOff || w.cfg.fromID == w.cfg.me ||
		(w.cfg.engine != "claude" && w.cfg.engine != "pi") {
		return false, nil
	}
	out, _, err := w.shCall("unread", w.cfg.crew, w.cfg.branch, w.cfg.me, w.cfg.fromID, strconv.FormatInt(w.cfg.runStartMS, 10), "dispatcher")
	if err != nil {
		return false, err
	}
	a, b := readWords(out)
	dOld, okOld := msTS(a)
	dNew, okNew := msTS(b)
	if !okOld || !okNew || w.now*1000-dOld < w.cfg.unread*1000 || dNew <= pd.nudgedTS {
		return false, nil
	}
	// Once per directive across watchdogs and the dispatcher: a restarted
	// watchdog or a manual `crew nudge` may already have typed for it.
	if w.nudged(dNew) {
		pd.nudgedTS = dNew
		return false, nil
	}
	out, rc, err := w.shCall("nudge", w.cfg.pane, w.cfg.engine, w.cfg.fromID, w.cfg.crew, b)
	if err != nil {
		return false, err
	}
	switch rc {
	case 0:
		pd.nudgedTS = dNew
		return true, nil
	case 3:
		pd.nudgedTS = dNew
		reason, _, _ := strings.Cut(out, " — ")
		// post, not postBlocked: our own open unread: episode must not swallow
		// the failure.
		err := w.post("blocked", "unread: dispatcher directive undelivered for "+strconv.FormatInt((w.now*1000-dOld)/1000, 10)+
			"s — auto-nudge typed but not accepted ("+reason+"); verify the pane with crew where")
		if err != nil {
			return true, err
		}
		pd.d6At, pd.d6Src = w.now, "dispatcher"
		return true, nil
	}
	if strings.HasPrefix(out, "anchor:") {
		pd.nudgeOff = true
	}
	return false, nil
}

// nudged runs nudged.jq over the log's last 2000 lines, each decoded on its
// own; any failure reads as not nudged.
func (w *watch) nudged(msgTS int64) bool {
	data, err := os.ReadFile(w.paths.Log)
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	lines = lines[max(len(lines)-2000, 0):]
	var rows []jsonv.Value
	for _, line := range lines {
		vs, err := jsonv.DecodeStream(strings.NewReader(line))
		if err != nil || len(vs) != 1 {
			continue
		}
		rows = append(rows, vs[0])
	}
	out, err := jqrun.Run(nudgedProgram, rows, 0, map[string]jsonv.Value{
		"c":  jsonv.Str(w.cfg.crew),
		"to": jsonv.Str(w.cfg.fromID),
		"m":  jsonv.Num(float64(msgTS)),
	})
	return err == nil && out.Kind() == jsonv.KindTrue
}
