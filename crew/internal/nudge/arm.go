// arm.go is the `crew nudge` command: resolve a dispatcher's target to one live lead
// pane, then type into it once or wait for the lead to come idle. The decision it makes
// about the pane itself is in nudge.go.

package nudge

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
)

//go:embed already.jq
var alreadyProgram string

//go:embed joined.jq
var joinedProgram string

const Usage = "crew nudge <codename|branch|worker:<branch>#s…> [--crew ID] [--wait [SECONDS]]"

// waitTailLines is the arm's `tail -n 2000` over the log for both --wait folds.
const waitTailLines = 2000

// exitType is jq's status for a fold that fails on a row it cannot use — the arm's
// `_sessions | jq` under `set -e`.
const exitType = 5

// Options is everything Run reaches beyond the bus and tmux. Deps carries the pane's
// side effects (and its clock), so a test drives the whole arm with fakes.
type Options struct {
	// Deps.Getenv is also the arm's environment: the two ids that mark a worker
	// session, plus CREW_NUDGE_WAIT_INTERVAL.
	Deps Deps
	// CrewID is `_crew_id`: the checkout's WORKER_TASK.md crew_id, else $CREW_ID.
	CrewID func() string
	// Git finds the checkout and Exists is the arm's `[ -f "$top/WORKER_TASK.md" ]`:
	// a checkout with its own task file is a worker's, which is as much of a
	// worker as an id in the environment is.
	Git    git.Client
	Exists func(path string) bool
	// Unread is the lead's unread-reason scan, `"<oldest-ts> <newest-ts>
	// role|dispatcher"`: the in-process `internal/unread` scan in Any mode, the
	// way the arm ran `_unread_scan <crew> <branch> worker:<branch> <sid> 0
	// any`. The newest ts is what the nudge row points at; an empty scan is the
	// "post it first" refusal, never a failure.
	Unread func(ctx context.Context, crew, branch, sid string) string
	// Rows reads the log's last maxLines lines with each line decoded on its own — the
	// arm's `tail -n N | jq -R 'fromjson?'`. The engine lookup reads the whole log
	// (maxLines 0, where the arm's `jq -Rr inputs` reads all of it) and the two --wait
	// folds read its last 2000 lines.
	Rows func(maxLines int) ([]any, bool)
	// PID is the lock owner, the arm's `$$`.
	PID int
	// Remote runs argv on host and returns its status: how a nudge reaches a
	// worker dispatched there, whose pane is on that host's tmux. Nil routes nothing.
	Remote func(host string, argv []string) int
}

// Request is the typed nudge call. Crew "" is the checkout's crew; Wait "" is no
// wait, otherwise the seconds to wait.
type Request struct {
	Crew, Target, Wait string
}

// NewRequest folds the typed argv into a Request. A --wait value is a number of
// seconds only when it is all digits: `--wait nova` waits the default and leaves
// `nova` the target, and `nova --wait abc` has no such target to give, so ok is false.
func NewRequest(crew, target string, wait *string) (r Request, ok bool) {
	r = Request{Crew: crew, Target: target}
	switch {
	case wait == nil:
	case *wait == "":
		r.Wait = "1800"
	case ascii.IsDigits(*wait):
		r.Wait = *wait
	case target != "":
		return r, false
	default:
		r.Target, r.Wait = *wait, "1800"
	}
	return r, true
}

// Run is the arm. Nothing about the target is typed: the pane comes from the window's
// dispatcher-anchored @crew_* stamps, and the text is the package's own constant.
func Run(ctx context.Context, r Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	// Typing into a sibling's pane is the dispatcher's call alone: a session that
	// carries a worker or role id, or sits in a checkout with its own WORKER_TASK.md,
	// is a worker. Note this refusal prints without the `crew: nudge:` prefix and
	// exits 2, unlike every other failure.
	top := strings.TrimSpace(git.Toplevel(ctx, o.Git, ""))
	if o.Deps.Getenv("CREW_WORKER_ID") != "" || o.Deps.Getenv("CREW_ROLE_ID") != "" ||
		(top != "" && o.Exists(filepath.Join(top, "WORKER_TASK.md"))) {
		say(stderr, "crew: nudge is a dispatcher command\n")
		return exitRefused
	}

	a := &arm{ctx: ctx, paths: paths, o: o, stdout: stdout, stderr: stderr}
	a.crew = r.Crew
	if a.crew == "" {
		a.crew = o.CrewID()
	}
	target, wait := r.Target, r.Wait
	if target == "" {
		return fail(stderr, "usage: "+Usage)
	}
	if a.crew == "" {
		return fail(stderr, "CREW_ID not set and no WORKER_TASK.md crew_id — pass --crew <id>")
	}

	want := ""
	if strings.HasPrefix(target, "%") {
		return fail(stderr, "pass a codename, branch or worker id — a pane id is not an anchored address")
	}
	if strings.HasPrefix(target, "worker:") {
		target = strings.TrimPrefix(target, "worker:")
		if bus.IsSessionID(target) {
			want, target = "worker:"+target, branchOf(target)
		}
	}

	wins, err := o.Deps.Tmux.Run(ctx, "list-windows", "-a", "-F",
		"#{window_id}\t#{@crew_branch}\t#{@crew_dir}\t#{@crew_id}\t#{@crew_name}")
	if err != nil {
		return fail(stderr, "cannot read tmux windows (is a tmux server running?)")
	}
	panes, err := o.Deps.Tmux.Run(ctx, "list-panes", "-a", "-F",
		"#{window_id}\t#{pane_id}\t#{@crew_role}\t#{pane_current_command}")
	if err != nil {
		return fail(stderr, "cannot read tmux panes (is a tmux server running?)")
	}

	// Only this checkout's crew, and only windows that carry a branch: an unanchored
	// window is somebody's shell, never a worker's.
	var cwins [][]string
	for _, r := range tsvRows(wins) {
		if len(r) >= 5 && r[1] != "" && r[2] == paths.Dir && r[3] == a.crew {
			cwins = append(cwins, r)
		}
	}
	win := firstWhere(cwins, func(r []string) bool { return field(r, 1) == target })
	var matches [][]string
	if win == nil {
		for _, r := range cwins {
			if field(r, 4) == target {
				matches = append(matches, r)
			}
		}
	}
	var local []string
	if win != nil {
		local = []string{field(win, 1)}
	}
	for _, r := range matches {
		local = append(local, field(r, 1))
	}
	if code, routed := a.route(target, want, wait, strings.Join(local, ","), stderr); routed {
		return code
	}
	if win == nil {
		if len(matches) > 1 {
			return fail(stderr, "ambiguous codename '"+target+"' — matches "+
				strings.Join(local, ",")+"; pass a branch")
		}
		win = first(matches)
	}
	if win == nil {
		return a.resolveFallback(target, stderr)
	}

	a.branch = field(win, 1)
	a.pane = field(firstWhere(tsvRows(panes),
		func(r []string) bool { return field(r, 0) == field(win, 0) && field(r, 2) == "lead" }), 1)
	if a.pane == "" {
		a.pane = field(firstWhere(tsvRows(panes), func(r []string) bool { return field(r, 0) == field(win, 0) }), 1)
	}
	if a.pane == "" {
		return fail(stderr, "no pane in the window for '"+target+"'")
	}

	if code := a.checkSession(want, stderr); code != 0 {
		return code
	}
	if code := a.checkEngine(stderr); code != 0 {
		return code
	}
	if code := a.checkUnread(stderr); code != 0 {
		return code
	}

	if wait == "" {
		out, rc := Pane(ctx, a.request("dispatcher:"+a.crew), a.o.Deps)
		if rc == exitRefused {
			return refuse(stderr, "refused — "+out)
		}
		say(stdout, "%s\n", out)
		return rc
	}
	return a.wait(ctx, wait)
}

// arm is one resolved nudge: the target's address, plus the wait loop's state.
type arm struct {
	ctx    context.Context
	paths  bus.Paths
	o      Options
	stdout io.Writer
	stderr io.Writer
	crew   string
	branch string
	sid    string
	pane   string
	engine string
	ts     string
	// waiting is the arm's nudge_waiting: once a `waiting` row exists, every end
	// records a `resolved` row so a joiner can report the outcome.
	waiting bool
}

func (a *arm) request(actor string) PaneRequest {
	return PaneRequest{
		Pane: a.pane, Engine: a.engine, Session: a.sid, Crew: a.crew, Actor: actor, MsgTS: a.ts,
	}
}

// route sends the nudge to the host its target was dispatched on, when the bus
// says it is remote and no local window claims the same target. The remote runs
// its own session, engine, unread and frame gates against its own tmux. localBranch
// is what the local windows matched, "" for nothing; routed is false when the local
// path should go on.
func (a *arm) route(target, want, wait, localBranch string, stderr io.Writer) (code int, routed bool) {
	if a.o.Remote == nil {
		return 0, false
	}
	host, branch, err := remote.ResolveTarget(resolve.Rows(a.paths.Log), target, a.crew, localBranch)
	if err != nil {
		return fail(stderr, err.Error()), true
	}
	if host == "" {
		return 0, false
	}
	argv := []string{"crew", "nudge", "--crew", a.crew}
	if wait != "" {
		argv = append(argv, "--wait", wait)
	}
	return a.o.Remote(host, append(argv, "--", cmp.Or(want, branch))), true
}

// resolveFallback is the arm's `_resolve_target` step: the target names a branch on the
// bus but no live window carries it.
func (a *arm) resolveFallback(target string, stderr io.Writer) int {
	var branches []string
	for _, r := range resolve.Resolve(resolve.Rows(a.paths.Log), target, a.crew) {
		branches = append(branches, r.Branch)
	}
	joined := strings.Join(branches, ",")
	switch {
	case strings.Contains(joined, ","):
		return fail(stderr, "ambiguous target '"+target+"' — matches "+joined+"; pass a branch")
	case joined != "":
		return fail(stderr, "no live pane for '"+target+"' (branch "+joined+") — its window is gone")
	}
	return fail(stderr, "no worker matches '"+target+"'")
}

// checkSession is the arm's `_sessions` block: the branch's newest session has to be a
// live, started, sessioned one, and the caller's own when it named one.
func (a *arm) checkSession(want string, stderr io.Writer) int {
	last, err := foldNewest(a.o.Deps, a.branch, a.crew)
	if err != nil {
		say(stderr, "crew: nudge: %s: %v\n", a.paths.Log, err)
		return exitType
	}
	if last == nil {
		return refuse(stderr, "no session on "+a.branch+" — dispatch a worker before nudging one")
	}
	if isTrue(last, "terminal") {
		return refuse(stderr, "newest session on "+a.branch+" is "+rowText(last, "state")+
			" — a stopped session never reads its inbox; re-dispatch with the context baked in")
	}
	if isNull(last, "state") {
		return refuse(stderr, "lead has not posted its first status yet — wait for it to start")
	}
	if isNull(last, "session") {
		return refuse(stderr, a.branch+
			" has no session id on the bus — a branch-only address can never reach a live worker's inbox; re-dispatch")
	}
	a.sid = rowText(last, "worker_id")
	if want != "" && want != a.sid {
		return refuse(stderr, want+" is not the newest session on "+a.branch+" ("+a.sid+")")
	}
	return 0
}

// checkEngine is the arm's dispatch/resume lookup. The row is read tolerantly: a torn
// tail line is skipped, exactly as the arm's `jq -Rr 'inputs | fromjson?'` skips it.
func (a *arm) checkEngine(stderr io.Writer) int {
	rows, ok := a.o.Rows(0)
	if ok {
		for _, r := range rows {
			kind := rowText(r, "kind")
			if rowText(r, "crew_id") == a.crew && (kind == "dispatch" || kind == "resume") &&
				rowText(r, "worker_id") == a.sid {
				a.engine = rowText(r, "engine")
			}
		}
	}
	if a.engine == "" {
		return refuse(stderr, "no dispatch or resume row records "+a.sid+"'s engine")
	}
	return 0
}

// checkUnread is the arm's unread scan: it accepts a dispatcher directive or an
// overdue role/critic msg, and the newest unread reason is what the nudge row will
// point at. An empty scan is the "post it first" refusal.
func (a *arm) checkUnread(stderr io.Writer) int {
	out := a.o.Unread(a.ctx, a.crew, a.branch, a.sid)
	f := strings.Fields(out)
	if len(f) < 3 {
		return refuse(stderr, "no unread msg from dispatcher:"+a.crew+" or a role to "+a.sid+
			" — post the directive first (crew reply), then nudge")
	}
	a.ts = f[1]
	return 0
}

// unread is the same scan's oldest unread ts — the arm reads the first field, which is
// empty when the lead has read the reason meanwhile.
func (a *arm) unread() string {
	out := a.o.Unread(a.ctx, a.crew, a.branch, a.sid)
	f := strings.Fields(out)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// wait is the `--wait` loop: one waiter per lead session, and a joiner reports the
// first waiter's outcome only when it covers the caller's own msg.
func (a *arm) wait(ctx context.Context, wait string) int {
	waitSecs, _ := strconv.Atoi(wait)
	interval := a.o.Deps.Getenv("CREW_NUDGE_WAIT_INTERVAL")
	if interval == "" {
		interval = "15"
	}
	start := a.o.Deps.Clock.Seconds()
	startMS := a.o.Deps.Clock.NowMS()

	dir := filepath.Join(a.paths.Dir, "nudge-wait")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(a.stderr, err.Error())
	}
	lockDir := filepath.Join(dir, lockName(a.sid))
	owner := strconv.Itoa(a.o.PID)
	joining := false
	for !lock.Acquire(lockDir, owner) {
		holder := lock.Holder(lockDir)
		if !joining {
			joining = true
			say(a.stdout, "nudge joining: %s %s — another crew nudge --wait (pid %s) owns this lead's wait\n",
				a.pane, a.sid, holder)
		}
		if a.o.Deps.Clock.Seconds() >= start+int64(waitSecs) {
			return refuse(a.stderr, "refused — still waiting on another crew nudge --wait (pid "+holder+")")
		}
		if err := a.o.Deps.Sleep(ctx, interval); err != nil {
			return exitFailure
		}
	}
	defer lock.Release(lockDir)

	if joining {
		if prev, ok := a.fold(joinedProgram, map[string]any{
			"t": json.Number(strconv.FormatInt(startMS, 10)), "m": json.Number(strconv.FormatInt(num(a.ts), 10)),
		}); ok {
			res := rowText(prev, "result") + ": " + rowText(prev, "detail")
			return a.end("joined", int(numOf(prev, "rc")), res, "nudge joined: "+a.pane+" "+a.sid+" — "+res)
		}
	}

	for {
		if prev, ok := a.fold(alreadyProgram, map[string]any{"m": json.Number(strconv.FormatInt(num(a.ts), 10))}); ok {
			res := rowText(prev, "result")
			rc := exitTyped
			if res == "accepted" {
				rc = 0
			}
			detail := res + ": " + rowText(prev, "detail")
			return a.end("already", rc, detail, "nudge already typed: "+a.pane+" "+a.sid+" — "+detail)
		}
		oldest := a.unread()
		if oldest == "" || num(oldest) > num(a.ts) {
			return a.end("read", 0, "the lead read the msg during the wait; nothing typed",
				"nudge not needed: "+a.pane+" "+a.sid+" — the lead read the msg during the wait; nothing typed")
		}

		out, rc := Pane(ctx, a.request("dispatcher:"+a.crew), a.o.Deps)
		if rc != exitRefused {
			res := strings.TrimPrefix(out, "nudge ")
			_, detail, _ := strings.Cut(out, " — ")
			return a.end(strings.SplitN(res, ":", 2)[0], rc, detail, out)
		}
		if out != "live turn or no idle input box" {
			return a.end("refused", exitRefused, out, "refused — "+out)
		}
		if !a.waiting {
			// Unlike the nudge row, a waiting row that cannot be written ends the wait at
			// 1 with nothing printed: a joiner must never be told an outcome no row records.
			if err := a.waitRow("waiting", "", "", "live turn"); err != nil {
				return fail(a.stderr, err.Error())
			}
			a.waiting = true
			say(a.stdout, "nudge waiting: %s %s — live turn; re-checking every %ss for up to %ss\n",
				a.pane, a.sid, interval, wait)
		}
		waited := a.o.Deps.Clock.Seconds() - start
		if waited >= int64(waitSecs) {
			return a.end("timeout", exitRefused, out+" (waited "+strconv.FormatInt(waited, 10)+"s)",
				"refused — "+out+" (waited "+strconv.FormatInt(waited, 10)+"s)")
		}
		if err := a.o.Deps.Sleep(ctx, interval); err != nil {
			// The arm's `_clock_sleep` runs under `set -e`: a sleep that fails, or a
			// signal that ends the command mid-wait, leaves with 1.
			return exitFailure
		}
	}
}

// end is the arm's `_nudge_wait_end`: it records the outcome only once a waiting row
// exists, and a refusal goes to stderr.
func (a *arm) end(result string, rc int, detail, line string) int {
	if a.waiting {
		if err := a.waitRow("resolved", result, strconv.Itoa(rc), detail); err != nil {
			return fail(a.stderr, err.Error())
		}
	}
	if rc == exitRefused {
		return refuse(a.stderr, line)
	}
	say(a.stdout, "%s\n", line)
	return rc
}

// waitRow is the `nudge_wait` bookkeeping row. Its `ts` follows CREW_CLOCK while the
// `nudge` row's stamp stays real time, and `rc` is absent until it is a number. A write
// failure is returned rather than swallowed: a joiner must never be told an outcome no
// row records.
func (a *arm) waitRow(state, result, rc, detail string) error {
	row := waitRowDoc{
		TS:     a.o.Deps.Clock.NowMS(),
		CrewID: a.crew,
		Kind:   "nudge_wait",
		From:   "dispatcher:" + a.crew,
		To:     a.sid,
		Branch: a.branch,
		Pane:   a.pane,
		MsgTS:  json.Number(digitsLiteral(a.ts)),
		State:  state,
		Result: result,
		Detail: detail,
	}
	if rc != "" {
		row.RC = json.Number(rc)
	}
	return a.o.Deps.Append(row)
}

// waitRowDoc is the `nudge_wait` bus row; RC is omitted until it is a number.
type waitRowDoc struct {
	TS     int64       `json:"ts"`
	CrewID string      `json:"crew_id"`
	Kind   string      `json:"kind"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Branch string      `json:"branch"`
	Pane   string      `json:"pane"`
	MsgTS  json.Number `json:"msg_ts"`
	State  string      `json:"state"`
	Result string      `json:"result"`
	Detail string      `json:"detail"`
	RC     json.Number `json:"rc,omitempty"`
}

// fold runs one of the arm's two wait-loop programs over the log's tail. A missing row
// is the fold's own `last // empty`: none, not a failure.
func (a *arm) fold(prog string, vars map[string]any) (map[string]any, bool) {
	rows, ok := a.o.Rows(waitTailLines)
	if !ok {
		return nil, false
	}
	vars["c"] = a.crew
	vars["to"] = a.sid
	out, err := jqrun.RunAny(prog, rows, 0, vars)
	if err != nil {
		return nil, false
	}
	m, ok := out.(map[string]any)
	return m, ok
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func fail(w io.Writer, msg string) int {
	say(w, "crew: nudge: %s\n", msg)
	return exitFailure
}

func refuse(w io.Writer, msg string) int {
	say(w, "crew: nudge: %s\n", msg)
	return exitRefused
}

// lockName is the arm's lock dir: the session id with every character `tr -c
// 'A-Za-z0-9._-' '_'` would replace, then the POSIX cksum that keeps two ids differing
// only in that replaced set apart.
func lockName(sid string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, sid)
	return name + "." + strconv.FormatUint(uint64(identity.CKsum([]byte(sid))), 10) + ".d"
}

// num is the arm's `$((x))` on a ts it has already checked is digits; anything else is
// zero, which is what an unset msg_ts compares as.
func num(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func numOf(v any, key string) int64 {
	m, _ := v.(map[string]any)
	switch n := m[key].(type) {
	case json.Number:
		f, _ := n.Float64()
		return int64(f)
	case float64:
		return int64(n)
	}
	return 0
}

// rowText is textOf over a gojq-form row: the string value of key, else "".
func rowText(v any, key string) string {
	m, _ := v.(map[string]any)
	s, _ := m[key].(string)
	return s
}

// PID is on Options because the arm's `$$` is the lock owner's text a joiner prints.

func isNull(v any, key string) bool {
	m, _ := v.(map[string]any)
	return m[key] == nil
}

func first(rows [][]string) []string {
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}
