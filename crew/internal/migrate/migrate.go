// Package migrate is `crew migrate stop|snapshot|mark`, the dispatcher's
// primitives for moving a running worker to another host. The claim-release
// rule those marks feed is package release.
//
// Every command is dispatcher-only (the sender check for dispatcher:<crew>)
// and runs its git from the trusted cwd. A branch reaches its worktree through
// `git worktree list` of the anchored common dir, and only when the worktree's
// dispatcher record agrees on path, crew dir, branch and admin dir; no bus row
// names a path here.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/migrate/release"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// Options are the side effects crew migrate makes.
type Options struct {
	Tmux tmux.Client
	// Proc lists the processes rooted in the worktree and signals them.
	Proc proc.Table
	// Git runs the check-ref-format read from the trusted cwd.
	Git    git.Client
	Sender sender.Probes
	Now    func() time.Time
	Sleep  func(time.Duration)
	// Append is `_bus_append`.
	Append func(path, line string) error
}

// Request names the crew the dispatcher posts as and the worker's branch.
type Request struct {
	Crew, Branch string
}

// StopRequest is `crew migrate stop`: Timeout is how many seconds the engine
// gets to exit after its windows are killed.
type StopRequest struct {
	Request
	Timeout int
}

// MarkRequest is `crew migrate mark`: the snapshot the release carries and the
// host it releases the branch to.
type MarkRequest struct {
	Request
	ToHost, Mode, Tip, Tree, Dir, Patch string
}

var (
	hostRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	oidRe   = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	stampRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)
)

// bundleStamp is the UTC time format naming a bundle dir.
const bundleStamp = "20060102T150405Z"

const panesFormat = "#{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"

// run is one crew migrate call past its shared prelude.
type run struct {
	action         string
	req            Request
	paths          bus.Paths
	crew           bus.Crew
	cwd, trusted   string
	stdout, stderr io.Writer
	o              Options
}

// refuse prints why the action is refused and returns its exit status.
func (r *run) refuse(why string) int {
	_, _ = fmt.Fprintf(r.stderr, "crew migrate: refusing %s %s — %s\n", r.action, r.req.Branch, why)
	return 1
}

// prelude is every action's gate: a crew id, the dispatcher sender check, the
// trusted cwd and a plain branch name. A non-zero code has printed its reason.
func prelude(ctx context.Context, action string, req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) (*run, int) {
	r := &run{action: action, req: req, paths: paths, cwd: cwd, stdout: stdout, stderr: stderr, o: o}
	if req.Crew == "" {
		return nil, r.refuse("no crew id — pass --crew or set CREW_ID")
	}
	c, err := paths.Crew(req.Crew)
	if err != nil {
		return nil, r.refuse("invalid crew id '" + req.Crew + "'")
	}
	r.crew = c
	if why := sender.Check(ctx, "dispatcher:"+req.Crew, req.Crew, o.Sender); why != "" {
		return nil, r.refuse(why)
	}
	trusted, err := wtgit.TrustedCwd(paths.Common, cwd, stderr)
	if err != nil {
		if !errors.Is(err, gitguard.ErrRefused) {
			_, _ = fmt.Fprintf(stderr, "crew migrate: %v\n", err)
		}
		return nil, 1
	}
	r.trusted = trusted
	// --branch also expands @{-N}, so the name must come back unchanged.
	if canon, err := o.Git.Run(ctx, trusted, "check-ref-format", "--branch", req.Branch); err != nil || canon != req.Branch {
		return nil, r.refuse("not a valid branch name")
	}
	return r, 0
}

// worktree resolves the branch to its worktree and admin dir, or says why not.
func (r *run) worktree(ctx context.Context) (wt, admin, why string) {
	cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: r.stderr}, "worktree", "list", "--porcelain")
	if err != nil {
		return "", "", "git config drift"
	}
	out, err := cmd.Output()
	if err != nil {
		return "", "", "could not list the worktrees"
	}
	wt = roster.WorktreePath(strings.TrimRight(string(out), "\n"), r.req.Branch)
	if wt == "" || strings.Contains(wt, "\n") || !filekind.IsDir(wt) {
		return "", "", "no worktree for it"
	}
	admin, err = wtgit.AdminDir(r.paths.Common, wt)
	if err != nil {
		return "", "", "no git admin dir for " + wt
	}
	return wt, admin, r.anchored(wt, admin)
}

// anchored is dispatch-resume's record check: the record keyed by wt's
// realpath must name wt, this crew dir, the branch and admin.
func (r *run) anchored(wt, admin string) string {
	rec, err := wtgit.AnchorPath(wt)
	if err != nil {
		return "no dispatcher record for " + wt
	}
	if fi, err := os.Lstat(rec); err != nil || !fi.Mode().IsRegular() {
		return "no dispatcher record for " + wt
	}
	data, err := os.ReadFile(rec)
	if err != nil {
		return "no dispatcher record for " + wt
	}
	lines := append(strings.Split(string(data), "\n"), "", "", "", "")
	realWt, _ := filepath.EvalSymlinks(wt)
	crewDir, _ := gitguard.RealpathMissing(r.paths.Dir)
	for _, f := range []struct{ label, got, want string }{
		{"worktree path", lines[0], realWt},
		{"crew dir", lines[1], crewDir},
		{"branch", lines[2], r.req.Branch},
		{"git dir", lines[3], admin},
	} {
		if f.got != f.want || f.want == "" {
			return "its dispatcher record does not match (" + f.label + ")"
		}
	}
	return ""
}

// guardOK reports a gitguard call: a refusal has printed its reason, any other
// error is printed here.
func (r *run) guardOK(err error) bool {
	if err == nil {
		return true
	}
	if !errors.Is(err, gitguard.ErrRefused) {
		_, _ = fmt.Fprintf(r.stderr, "refusing git: %v\n", err)
	}
	return false
}

// enginePane is the first pane running an engine at exactly wt, as
// "pane <id> (<cmd>)", else "".
func (r *run) enginePane(ctx context.Context, wt string) string {
	for line := range strings.SplitSeq(tmux.Output(ctx, r.o.Tmux, "list-panes", "-a", "-F", panesFormat), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) == 4 && f[3] == wt && roster.IsEngineCmd(f[2]) {
			return "pane " + f[1] + " (" + f[2] + ")"
		}
	}
	return ""
}

// Stop kills the worker's windows and the processes rooted in its worktree,
// then waits up to req.Timeout seconds for none to be left. It returns the
// exit status: 1 names the survivor.
func Stop(ctx context.Context, req StopRequest, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	r, code := prelude(ctx, "stop", req.Request, paths, cwd, stdout, stderr, o)
	if r == nil {
		return code
	}
	wt, _, why := r.worktree(ctx)
	if why != "" {
		return r.refuse(why)
	}
	reap.KillWorkerWindows(ctx, o.Tmux, o.Proc, wt, func(msg string) {
		_, _ = fmt.Fprintf(stdout, "crew migrate: %s\n", msg)
	})
	deadline := o.Now().Add(time.Duration(req.Timeout) * time.Second)
	for {
		survivor := r.survivor(ctx, wt)
		if survivor == "" {
			return 0
		}
		if !o.Now().Before(deadline) {
			_, _ = fmt.Fprintf(stderr, "crew migrate: stop %s: %s still runs in %s after %ds\n", req.Branch, survivor, wt, req.Timeout)
			return 1
		}
		o.Sleep(time.Second)
	}
}

// survivor names an engine pane at wt or a process rooted in it, this process
// and its ancestors excepted; "" when none is left. Any process, not only an
// engine: what still runs there can still write the tree the snapshot reads.
func (r *run) survivor(ctx context.Context, wt string) string {
	if p := r.enginePane(ctx, wt); p != "" {
		return p
	}
	anc := proc.Ancestors(r.o.Proc)
	for _, c := range r.o.Proc.Cwds(ctx) {
		if !strings.HasPrefix(c.Dir+"/", wt+"/") || c.PID == os.Getpid() || anc[c.PID] {
			continue
		}
		name := "pid " + strconv.Itoa(c.PID)
		if comm, ok := r.o.Proc.Comm(c.PID); ok {
			name += " (" + comm + ")"
		}
		return name
	}
	return ""
}

// snapshotLine is snapshot's one JSON line.
type snapshotLine struct {
	Dir   string `json:"dir"`
	Patch string `json:"patch"`
	Tree  string `json:"tree"`
	Tip   string `json:"tip"`
}

// Snapshot saves the worker's uncommitted state, gitignored root scaffold
// included, as a patch against its branch tip in the bundle dir
// <crew_dir>/migrate/<branch>/<UTC>, and prints {"dir","patch","tree","tip"}.
// The bundle dir lives outside the worker-writable artifacts tree and is
// always made here, a clean tree's (patch "") included; one already there is
// refused. It refuses on everything reap --discard refuses on, and never
// changes the worktree or its index.
func Snapshot(ctx context.Context, req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	r, code := prelude(ctx, "snapshot", req, paths, cwd, stdout, stderr, o)
	if r == nil {
		return code
	}
	wt, admin, why := r.worktree(ctx)
	if why != "" {
		return r.refuse(why)
	}
	if r.enginePane(ctx, wt) != "" {
		return r.refuse("an engine is running there")
	}
	if strings.HasPrefix(cwd+"/", wt+"/") {
		return r.refuse("it is the current worktree")
	}
	if why := r.gates(ctx, wt, admin); why != "" {
		return r.refuse(why)
	}
	// $dir is ours, so the temp dir and the patch built in it are too.
	tmp, err := os.MkdirTemp(paths.Dir, "migrate.")
	if err != nil {
		return r.refuse("could not save uncommitted state")
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	root := paths.Dir + "/migrate"
	dir := req.Branch + "/" + o.Now().UTC().Format(bundleStamp)
	snap, err := reap.Snapshot(ctx, reap.SnapshotRequest{
		Admin: admin, WtPath: wt, Branch: req.Branch, Tmp: tmp,
		Root: root, Dir: dir, Name: "patch", FreshDir: true, IgnoredScaffold: true, Stderr: stderr,
	})
	if errors.Is(err, reap.ErrDirExists) {
		return r.refuse("bundle dir already exists: " + root + "/" + dir)
	}
	if err != nil {
		return r.refuse(err.Error())
	}
	var b strings.Builder
	_ = jsonval.Encode(&b, snapshotLine{Dir: root + "/" + dir, Patch: snap.Patch, Tree: snap.Tree, Tip: snap.Tip}, false)
	_, _ = io.WriteString(stdout, b.String())
	return 0
}

// gates are reap --discard's tree gates: the gitlink points at admin, no
// config.worktree, no submodules, no git-config drift. It returns why the
// branch is refused, "" to go on.
func (r *run) gates(ctx context.Context, wt, admin string) string {
	if !wtgit.GitlinkOK(admin, wt) {
		return "its .git does not point at its git admin dir"
	}
	if _, err := os.Lstat(admin + "/config.worktree"); err == nil {
		return admin + "/config.worktree exists"
	}
	cmd, err := wtgit.Command(ctx, admin, wt, wtgit.Opts{Stderr: r.stderr}, "ls-files", "--stage")
	if err != nil {
		return "git config drift"
	}
	staged, err := cmd.Output()
	if err != nil {
		return "could not read its index"
	}
	for line := range strings.SplitSeq(string(staged), "\n") {
		if strings.HasPrefix(line, "160000 ") {
			return "it has submodules"
		}
	}
	if !r.guardOK(gitguard.Check(r.paths.Common, admin, r.stderr)) ||
		!r.guardOK(gitguard.CheckCwd(r.paths.Common, r.trusted, r.stderr)) {
		return "git config drift"
	}
	return ""
}

type markRow struct {
	TS     int64    `json:"ts"`
	CrewID string   `json:"crew_id"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	Kind   string   `json:"kind"`
	Body   markBody `json:"body"`
}

type markBody struct {
	State  string `json:"state"`
	ToHost string `json:"to_host"`
	Source string `json:"source"`
	Mode   string `json:"mode"`
	Tip    string `json:"tip"`
	Tree   string `json:"tree"`
	Dir    string `json:"dir"`
	Patch  string `json:"patch"`
	Detail string `json:"detail"`
}

// Mark appends the releasing `migrated` row, source "migrate", as the branch's
// newest worker session: from then on the branch is released to req.ToHost.
func Mark(ctx context.Context, req MarkRequest, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	r, code := prelude(ctx, "mark", req.Request, paths, cwd, stdout, stderr, o)
	if r == nil {
		return code
	}
	switch {
	case !hostRe.MatchString(req.ToHost):
		return r.refuse("invalid --to-host '" + req.ToHost + "'")
	case req.Mode != "coop" && req.Mode != "now":
		return r.refuse("--mode must be coop or now (got '" + req.Mode + "')")
	case !oidRe.MatchString(req.Tip):
		return r.refuse("--tip must be a full commit id (got '" + req.Tip + "')")
	case !oidRe.MatchString(req.Tree):
		return r.refuse("--tree must be a full tree id (got '" + req.Tree + "')")
	case !r.bundleDir(req.Dir):
		return r.refuse("--dir " + req.Dir + " is not a migrate bundle dir of " + req.Branch)
	case req.Patch != "" && !migratePatch(req.Dir, req.Patch):
		return r.refuse("--patch " + req.Patch + " is not a migrate patch of " + req.Branch)
	}
	evs, err := bus.ReadEvents(r.crew.Log)
	if err != nil && !errors.Is(err, bus.ErrNoLog) {
		return r.refuse("could not read the bus")
	}
	worker := release.NewestWorker(evs, req.Branch)
	if worker == "" {
		return r.refuse("no dispatch row for it")
	}
	var b strings.Builder
	_ = jsonval.Encode(&b, markRow{
		TS: o.Now().UnixMilli(), CrewID: req.Crew, From: worker, To: "dispatcher:" + req.Crew, Kind: "status",
		Body: markBody{
			State: string(bus.StateMigrated), ToHost: req.ToHost, Source: "migrate", Mode: req.Mode,
			Tip: req.Tip, Tree: req.Tree, Dir: req.Dir, Patch: req.Patch, Detail: "moved to " + req.ToHost,
		},
	}, false)
	if err := o.Append(r.crew.Log, strings.TrimSuffix(b.String(), "\n")); err != nil {
		_, _ = fmt.Fprintf(stderr, "crew migrate: could not append to %s: %v\n", r.crew.Log, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "crew migrate: %s released to host %s\n", req.Branch, req.ToHost)
	return 0
}

// bundleDir is a real directory at <crew_dir>/migrate/<branch>/<UTC>, where
// snapshot makes one: no symlink anywhere from the crew dir down.
func (r *run) bundleDir(dir string) bool {
	base := r.paths.Dir + "/migrate/" + r.req.Branch
	if filepath.Clean(dir) != dir || filepath.Dir(dir) != base || !stampRe.MatchString(filepath.Base(dir)) {
		return false
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return false
	}
	resolved, err := filepath.EvalSymlinks(dir)
	crewDir, crewErr := filepath.EvalSymlinks(r.paths.Dir)
	return err == nil && crewErr == nil && resolved == crewDir+strings.TrimPrefix(dir, r.paths.Dir)
}

// migratePatch is the regular file <dir>/patch, where snapshot writes one.
func migratePatch(dir, p string) bool {
	if p != dir+"/patch" {
		return false
	}
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}
