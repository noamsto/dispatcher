// Package adopt is `crew adopt`: re-attaching the caller to a crew that is
// already on disk after a restart lost CREW_ID (#29), and releasing the
// `dispatched` labels a crew that died mid-claim left behind (#73).
//
// stdout is a machine channel: callers run `CREW_ID=$(crew adopt <id> $PPID)`,
// so it carries the bare id alone and every message the run has to say — kept
// claim, released label, refusal — goes to stderr.
//
// The pid file and `$dir/pidfile.log` are shared with the bash arms that still
// write them (`register`, `deregister`) and with the readers in crews and
// dispatch, so the line format and the append protocol are contracts, not
// internals: `_pidfile_log`, `_bus_append` and `_owner_pid` have Go copies here
// or in internal/crews.
package adopt

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

const usage = "crew: adopt [--force] <id> [pid]"

const exitFailure = 1

// claimsProgram is the arm's claim fold. See the file for the one patch a jq
// program needs before jqrun can run it.
//
//go:embed claims.jq
var claimsProgram string

// Options is everything Run reads beyond the bus: the pid probes, the clock the
// log line stamps, the commands the arm shells out to, and the process identity
// the log line records.
type Options struct {
	// Probes are the pid reads: whether the recorded dispatcher is alive,
	// whether it is this process's ancestor, and the owner-pid walk.
	Probes crews.Probes
	// Now is the clock the pidfile.log line stamps.
	Now func() time.Time
	// Gh runs `gh <args…>` with stderr merged into stdout as the arm's
	// `$(gh … 2>&1)` does, because the text it captures is what its failure
	// line prints.
	Gh func(args ...string) (string, error)
	// GhQuiet runs `gh <args…>` with both streams discarded as the arm's
	// `gh … >/dev/null 2>&1` does: only the exit status is read.
	GhQuiet func(args ...string) error
	// Worktrees is `git worktree list --porcelain`, run once per branch as the
	// arm runs it. A failed git is the arm's empty `$(…)`: no worktree, not an
	// error.
	Worktrees func() string
	// Windows and SelfWindow are the two tmux reads that decide whether a
	// worktree is occupied. `_occupants` also reads the pane list, but those
	// rows fill only the advisory `engine`/`panes` fields of the JSON it prints,
	// and adopt asks just whether that array is empty.
	Windows    func() string
	SelfWindow func(pane string) string
	// Args is `ps -o args= -p <pid>`: the caller's command line, which the log
	// line names so a crew dir that changes owner can be traced (#432). A ps
	// that cannot say is the arm's `$(ps … || true)`: no command line.
	Args func(pid int) string
	// LookupEnv reads TMUX_PANE.
	LookupEnv func(string) string
	// PID, PPID and Pwd are `$$`, `$PPID` and `$PWD` of the arm. crew.sh execs
	// this binary, so the Go process keeps the arm's pid and its parent is the
	// caller's shell — the same three fields bash writes.
	PID  int
	PPID int
	Pwd  string
}

// claim is one issue this crew claimed, with every branch any crew claimed it
// on: field one of a fold row is the issue as text, field two this crew's
// branch and the rest the other branches — the arm's `crow` array.
type claim struct {
	issue    string
	branch   string
	branches []string
}

func Run(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	o = o.withDefaults()

	// `--force` is stripped from anywhere in the argv, so `crew adopt <id>
	// --force` cannot record the literal string "--force" as the pid.
	force := false
	var pos []string
	for _, a := range args {
		if a == "--force" {
			force = true
			continue
		}
		pos = append(pos, a)
	}

	id := arg(pos, 0)
	if id == "" {
		say(stderr, "%s\n", usage)
		return exitFailure
	}
	// The same charset bus.ValidCrewID gates every other writer of a crew id
	// with: an unsanitized id reaches file paths and jq arguments downstream.
	if !bus.ValidCrewID(id) {
		say(stderr, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n")
		return exitFailure
	}

	// The pid to record. `${2:-…}` falls back to the owner walk on an empty
	// second arg as well as a missing one, and whatever text results is written
	// verbatim — the arm never checks it names a real process.
	pidText := arg(pos, 1)
	if pidText == "" {
		pidText = strconv.Itoa(o.Probes.OwnerPID(o.PPID))
	}

	cdir := paths.CrewDir(id)
	pidfile := cdir + "/pid"

	// A crew is known from the directory, or from the bus when a restart took
	// the directory with it: the id still has to appear in the log to be
	// adopted rather than invented.
	if !isDir(cdir) && !loggedCrew(paths, id) {
		say(stderr, "crew: no crew '%s' in this repo — run 'crew crews' to list them, or 'crew new' to start one\n", id)
		return exitFailure
	}

	epid, _ := crews.PidFileText(pidfile)
	live := false
	if a := crews.PidAlive(o.Probes, o.Now(), pidfile, epid); a != nil && *a {
		live = true
	}

	if !force && live {
		// A live owner that is this process's ancestor is the idempotent case:
		// `dispatch` adopts the crew it just made on every retry. Liveness
		// already proved the text is a positive integer, so the parse cannot fail.
		n, _ := strconv.Atoi(epid)
		if !o.Probes.IsAncestor(n) {
			o.pidfileLog(paths, "adopt", "refused", id, epid, pidText)
			say(stderr, "crew: crew '%s' still has a live dispatcher — 'crew new' starts your own; '--force' overrides if that process is a stale pid reuse\n", id)
			return exitFailure
		}
	}

	if err := os.MkdirAll(cdir, 0o755); err != nil {
		say(stderr, "crew: adopt: %v\n", err)
		return exitFailure
	}
	if err := os.WriteFile(pidfile, []byte(pidText+"\n"), 0o644); err != nil {
		say(stderr, "crew: adopt: %v\n", err)
		return exitFailure
	}
	outcome := "ok"
	if force && live {
		outcome = "forced"
	}
	o.pidfileLog(paths, "adopt", outcome, id, epid, pidText)

	// A crew adopted over a dead dispatcher picks up the labels its owner
	// claimed and never released; one adopted from a live ancestor changes
	// nothing about who owns the work, so it releases nothing.
	if !live {
		o.releaseClaims(paths, id, stderr)
	}

	say(stdout, "%s\n", id)
	return 0
}

// loggedCrew is the arm's `jq -r 'select(.crew_id == $id)' | head -1`: any row
// naming the id, from the readable prefix of the log. A torn tail is what
// ReadEventsTolerant is for.
func loggedCrew(paths bus.Paths, id string) bool {
	events, _ := bus.ReadEventsTolerant(paths.Log)
	for _, ev := range events {
		if ev.CrewID == id {
			return true
		}
	}
	return false
}

// releaseClaims is the arm's claim release: for each issue this crew claimed
// while it was dispatched, drop the `dispatched` label unless the branch is
// still occupied or heads an open PR.
func (o Options) releaseClaims(paths bus.Paths, id string, stderr io.Writer) {
	claims := claimRows(paths, id)
	if len(claims) == 0 {
		return
	}
	heads, err := o.Gh("pr", "list", "--state", "open", "--limit", "500",
		"--json", "headRefName", "--jq", ".[].headRefName")
	if err != nil {
		// The arm's `2>&1`: gh's own message is captured, not printed, so this
		// line is the only place it reaches the terminal.
		say(stderr, "crew adopt: could not list open PRs (%s) — leaving the recorded claims in place\n", heads)
		return
	}
	for _, c := range claims {
		if c.issue == "" {
			continue
		}
		// `gh issue edit` takes a number, and the bus is caller-writable, so a
		// malformed issue is reported and skipped rather than passed as an argv
		// word. The well-formed rows beside it still release.
		if !isNumber(c.issue) {
			say(stderr, "crew adopt: skipping a claim row whose issue is not a number (%s)\n", c.issue)
			continue
		}
		if held := o.held(c, heads); held != "" {
			say(stderr, "%s\n", held)
			continue
		}
		if err := o.GhQuiet("issue", "edit", c.issue, "--remove-label", "dispatched"); err == nil {
			say(stderr, "crew adopt: released the dispatched label on #%s (%s)\n", c.issue, c.branch)
			continue
		}
		say(stderr, "crew adopt: could not remove the dispatched label from #%s (%s)\n", c.issue, c.branch)
	}
}

// held is the arm's per-claim gate, answered with the line it prints to keep
// the claim, or "" when the label can go. Every branch the issue was claimed on
// is checked, not only this crew's: two crews working the same issue on two
// branches must not have the label pulled out from under the live one.
func (o Options) held(c claim, heads string) string {
	for _, cb := range c.branches {
		wt := roster.WorktreePath(o.Worktrees(), cb)
		if wt != "" && isDir(wt) && o.occupied(wt) {
			return fmt.Sprintf("crew adopt: keeping #%s — a worker still occupies %s", c.issue, cb)
		}
		if hasLine(heads, cb) {
			return fmt.Sprintf("crew adopt: keeping #%s — %s heads an open PR", c.issue, cb)
		}
	}
	return ""
}

// occupied is the arm's `[ "$(_occupants "$wt")" != '[]' ]`: a crewed window
// that is neither the dispatcher's nor the caller's own, rooted at exactly this
// path.
func (o Options) occupied(wt string) bool {
	self := ""
	if pane := o.LookupEnv("TMUX_PANE"); pane != "" {
		self = o.SelfWindow(pane)
	}
	for _, line := range strings.Split(o.Windows(), "\n") {
		// `IFS=$'\t' read -r wid nm path`: tab is IFS whitespace, so runs of it
		// collapse and the last field keeps whatever is left of the line.
		wid, nm, path := read3(line)
		if wid == "" || wid == self || path != wt || nm == "" || nm == "dispatcher" {
			continue
		}
		return true
	}
	return false
}

// claimRows is the arm's fold: the issues whose newest claim belongs to this
// crew, each with the branches it was claimed on. `jq -s` reads the whole log
// or nothing, so an unreadable or corrupt log is no claims at all — the arm's
// `2>/dev/null || true`.
func claimRows(paths bus.Paths, id string) []claim {
	events, err := bus.ReadEvents(paths.Log)
	if err != nil {
		return nil
	}
	raws := make([]jsonv.Value, len(events))
	for i, ev := range events {
		raws[i] = ev.Raw
	}
	out, err := jqrun.Run(claimsProgram, raws, 0, map[string]jsonv.Value{"id": jsonv.Str(id)})
	if err != nil {
		return nil
	}
	var adopted []claim
	for _, row := range out.Elems() {
		cols := row.Elems()
		// The fold emits [issue, branch, others…]; anything shorter is not a row.
		if len(cols) < 2 {
			continue
		}
		c := claim{issue: fieldText(cols[0]), branch: fieldText(cols[1])}
		for _, b := range cols[1:] {
			c.branches = append(c.branches, fieldText(b))
		}
		adopted = append(adopted, c)
	}
	return adopted
}

// pidfileLog is `_pidfile_log` for this run's caller identity.
func (o Options) pidfileLog(paths bus.Paths, action, outcome, crew, oldPID, newPID string) {
	crews.PidfileLog(paths, crews.Caller{
		Now: o.Now(), PID: o.PID, PPID: o.PPID, Pwd: o.Pwd, By: o.Args(o.PPID),
	}, action, outcome, crew, oldPID, newPID)
}

func (o Options) withDefaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Gh == nil {
		o.Gh = ghCombined
	}
	if o.GhQuiet == nil {
		o.GhQuiet = ghQuiet
	}
	if o.Worktrees == nil {
		o.Worktrees = worktreeList
	}
	if o.Windows == nil {
		o.Windows = windowList
	}
	if o.SelfWindow == nil {
		o.SelfWindow = selfWindow
	}
	if o.Args == nil {
		o.Args = PsArgs
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.Getenv
	}
	if o.Pwd == "" {
		o.Pwd, _ = os.Getwd()
	}
	return o
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// read3 is `IFS=$'\t' read -r wid nm path` over one line. Tab is IFS
// whitespace, so runs of it are one separator and the edges of the line are
// trimmed, while the last field keeps the rest of the line — a tab inside a
// path stays part of it.
func read3(line string) (wid, nm, path string) {
	wid, rest := cutTab(strings.TrimLeft(line, "\t"))
	nm, rest = cutTab(rest)
	return wid, nm, strings.TrimRight(rest, "\t")
}

// cutTab splits at the first tab and swallows the run of tabs it ended.
func cutTab(s string) (field, rest string) {
	field, rest, found := strings.Cut(s, "\t")
	if !found {
		return field, ""
	}
	return field, strings.TrimLeft(rest, "\t")
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// isNumber is the arm's `case "$cissue" in *[!0-9]*)`: digits, and not empty.
func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hasLine is the arm's `grep -qxF` over the PR head refs: whole-line equality,
// not a substring of a longer branch name.
func hasLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

// fieldText is the text `@tsv` wrote for one fold field.
func fieldText(v jsonv.Value) string {
	switch v.Kind() {
	case jsonv.KindString:
		s, _ := v.AsString()
		return s
	case jsonv.KindNumber:
		return v.NumberText()
	case jsonv.KindTrue:
		return "true"
	case jsonv.KindFalse:
		return "false"
	case jsonv.KindArray, jsonv.KindObject:
		return string(jsonv.Append(nil, v, jsonv.Options{}))
	case jsonv.KindNull:
	}
	return "null"
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
