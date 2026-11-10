package adopt

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// reattach is `crew adopt --reattach <id> <pid>`: it re-registers a crew whose
// dispatcher registration was lost — a launcher that exited and deregistered
// while its engine kept dispatching — and only for a caller that is plausibly
// that dispatcher. `dispatch` runs it on every call, so once id and pid are
// present it is silent and exits 0 unless it writes.
func reattach(ctx context.Context, req Request, paths bus.Paths, stderr io.Writer, o Options) int {
	if !bus.ValidCrewID(req.ID) {
		return 0
	}
	if !ascii.IsDigits(req.PID) {
		return 0
	}
	startPID, err := strconv.Atoi(req.PID)
	if err != nil || startPID <= 0 {
		return 0
	}
	if o.Getenv("CREW_WORKER_ID") != "" || o.Getenv("CREW_ROLE_ID") != "" {
		return 0
	}
	pane := o.Getenv("TMUX_PANE")
	if !strings.HasPrefix(pane, "%") || !ascii.IsDigits(pane[1:]) {
		return 0
	}

	// The start pid is the shell `dispatch` runs in; recording it would name a
	// process that exits with the call, so only an engine above it qualifies.
	engine, name, ok := proc.FirstNonShell(o.Proc, startPID)
	if !ok || !roster.IsEngineCmd(name) {
		return 0
	}

	// The engine must run in this pane, and the pane must not be a worker's
	// window, which tmux marks with @crew_branch.
	out := tmux.Output(ctx, o.Tmux, "display-message", "-p", "-t", pane, "#{pane_pid}\t#{@crew_branch}")
	pidText, branch, _ := strings.Cut(out, "\t")
	if !ascii.IsDigits(pidText) || branch != "" {
		return 0
	}
	panePID, err := strconv.Atoi(pidText)
	if err != nil || panePID <= 0 || !inAncestry(o.Proc, engine, panePID) {
		return 0
	}

	// Checked after the process gates: loggedCrew can read the whole bus, and
	// most dispatches fail a cheaper gate first.
	cdir := paths.CrewDir(req.ID)
	if !filekind.IsDir(cdir) && !loggedCrew(paths, req.ID) {
		return 0
	}

	pidfile := cdir + "/pid"
	epid, _ := crews.PidFileText(pidfile)
	if a := crews.PidAlive(o.Proc, o.Clock.Now(), pidfile, epid); a != nil && *a {
		return 0
	}

	newPID := strconv.Itoa(engine)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		say(stderr, "crew: adopt: %v\n", err)
		return exitFailure
	}
	if err := os.WriteFile(pidfile, []byte(newPID+"\n"), 0o644); err != nil {
		say(stderr, "crew: adopt: %v\n", err)
		return exitFailure
	}
	if err := os.WriteFile(cdir+"/pane", []byte(pane+"\n"), 0o644); err != nil {
		say(stderr, "crew: adopt: %v\n", err)
		return exitFailure
	}
	o.pidfileLog(paths, "reattach", "ok", req.ID, epid, newPID)
	// No claim release: the caller carries on this crew's work, so the labels
	// it dispatched are still its own.
	say(stderr, "crew adopt: re-registered crew %s to dispatcher pid %s in pane %s — it had no live registration\n", req.ID, newPID, pane)
	return 0
}

// inAncestry reports whether panePID is pid or one of its ancestors, within the
// owner walk's 32-step bound.
func inAncestry(t proc.Table, pid, panePID int) bool {
	cur := pid
	for range 32 {
		if cur == panePID {
			return true
		}
		if cur <= 1 {
			return false
		}
		next, ok := t.Parent(cur)
		if !ok {
			return false
		}
		cur = next
	}
	return cur == panePID
}
