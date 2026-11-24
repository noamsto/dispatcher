package reap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const (
	l2Windows = "list-windows -a -F #{window_id} #{pane_current_path} #{@worktree}"
	l2Live    = "list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"
)

// l2fx is newFx with a layout-2 crew c2 indexed for the repo and no repo log:
// c2 dispatched rb here, its worker is done with a merged PR, and rb's
// worktree record names this repo and c2.
type l2fx struct {
	*fx
	tag, crewLog string
	// rows are the appends of the last reap, as path and line.
	rows [][2]string
	// tmuxHook, when set, sees every tmux call before the fake answers it.
	tmuxHook func(key string)
}

func newL2(t *testing.T) *l2fx {
	t.Helper()
	f := newFx(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.root, "xdgstate"))
	f.paths = f.paths.WithState(bus.StateRoot(os.Getenv))
	tag, err := bus.RepoTag(f.paths.Common)
	if err != nil {
		t.Fatal(err)
	}
	l := &l2fx{fx: f, tag: tag}
	if err := bus.CreateMeta(f.paths.State, bus.Meta{ID: "c2", CreatedBy: "register", HomeRepo: &tag}); err != nil {
		t.Fatal(err)
	}
	l.index("c2")
	c, err := f.paths.Crew("c2")
	if err != nil || c.Layout != 2 {
		t.Fatalf("crew = %+v, %v", c, err)
	}
	l.crewLog = c.Log
	l.crewRows(repoDispatchRow(rb, "c2", tag, int(nowUnix-100)*1000), l2Status(rb, "s1", "done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	l.record(f.wt, l.crewDir(), "c2")
	return l
}

func (l *l2fx) index(id string) {
	l.t.Helper()
	if _, err := bus.IndexAdd(l.paths.State, l.tag, "o/r", id, time.Now()); err != nil {
		l.t.Fatal(err)
	}
}

func (l *l2fx) crewRows(rows ...string) {
	l.t.Helper()
	writeLog(l.t, l.crewLog, rows...)
}

// crewDir is the record's line 2 for this repo.
func (l *l2fx) crewDir() string {
	l.t.Helper()
	want, err := gitguard.RealpathMissing(l.paths.Common + "/crew")
	if err != nil {
		l.t.Fatal(err)
	}
	return want
}

// record writes wt's worktree record with line 2 dir and line 5 crew.
func (l *l2fx) record(wt, dir, crew string) {
	l.t.Helper()
	anchor, err := wtgit.AnchorPath(wt)
	if err != nil {
		l.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte(wt+"\n"+dir+"\n\n\n"+crew+"\n"), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// window puts @5 on rb's worktree, stamped @crew_id crew.
func (l *l2fx) window(crew string) {
	l.tmux[l2Windows] = "@5 " + l.wt + " " + l.wt
	l.tmux["display-message -p -t @5 #{@crew_id}"] = crew
}

// l2Status is a status row of c2's worker on branch, ageSec before the fake now.
func l2Status(branch, session, state, pr string, ageSec int64) string {
	body := `{"state":"` + state + `"`
	if pr != "" {
		body += `,"pr_url":"` + pr + `"`
	}
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c2","from":"worker:%s#%s","to":"dispatcher:c2","kind":"status","body":%s}}`,
		(nowUnix-ageSec)*1000, branch, session, body)
}

func (l *l2fx) reap(req Request) int {
	l.t.Helper()
	return Run(context.Background(), req, l.paths, l.main, &l.stdout, &l.stderr, l.options())
}

// options are f.opts() recording appends in rows and passing tmux calls
// through tmuxHook; they reset the last run's rows and streams.
func (l *l2fx) options() Options {
	o := l.opts()
	app := o.Probes.Append
	o.Probes.Append = func(path, line string) error {
		l.rows = append(l.rows, [2]string{path, line})
		return app(path, line)
	}
	tm := o.Probes.Tmux
	o.Probes.Tmux = &tmuxtest.Fake{Func: func(args []string) (string, error) {
		if l.tmuxHook != nil {
			l.tmuxHook(strings.Join(args, " "))
		}
		return tm.Run(context.Background(), args...)
	}}
	l.rows = nil
	l.stdout.Reset()
	l.stderr.Reset()
	return o
}

// rowsOf are the appended rows of kind for branch, as path and line.
func (l *l2fx) rowsOf(kind, branch string) [][2]string {
	var out [][2]string
	for _, r := range l.rows {
		if strings.Contains(r[1], `"kind":"`+kind+`"`) && strings.Contains(r[1], `"branch":"`+branch+`"`) {
			out = append(out, r)
		}
	}
	return out
}

func (l *l2fx) wantStdout(sub string) {
	l.t.Helper()
	if !strings.Contains(l.stdout.String(), sub) {
		l.t.Fatalf("stdout:\n%s\nwant it to contain %q\nstderr:\n%s", l.stdout.String(), sub, l.stderr.String())
	}
}

func wantDir(t *testing.T, dir string, present bool) {
	t.Helper()
	_, err := os.Stat(dir)
	if present && err != nil {
		t.Fatalf("%s is gone: %v", dir, err)
	}
	if !present && !os.IsNotExist(err) {
		t.Fatalf("%s still there (%v)", dir, err)
	}
}

// keptUnvalidated is a gate refusal: kept with the reason, worktree and
// windows untouched.
func (l *l2fx) keptUnvalidated() {
	l.t.Helper()
	l.wantStdout("crew reap: keeping " + rb + " — layout-2 row not validated: ")
	wantDir(l.t, l.wt, true)
	l.noKill()
	if rows := l.rowsOf("reap", rb); len(rows) != 0 {
		l.t.Fatalf("reap rows %q", rows)
	}
}

// (a)
func TestLayout2MergedIsReaped(t *testing.T) {
	l := newL2(t)
	l.window("c2")
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	l.wantStdout("crew reap: reaped " + rb + " (MERGED)")
	wantDir(t, l.wt, false)
	rows := l.rowsOf("reap", rb)
	if len(rows) != 1 || rows[0][0] != l.crewLog || !strings.Contains(rows[0][1], `"crew_id":"c2"`) {
		t.Fatalf("reap rows %q, want one on %s", rows, l.crewLog)
	}
	if !slices.Contains(l.calls, "tmux kill-window -t @5") {
		t.Fatalf("calls %q, want @5 killed", l.calls)
	}
}

// (b)
func TestLayout2UncommittedKept(t *testing.T) {
	l := newL2(t)
	l.window("c2")
	if err := os.WriteFile(filepath.Join(l.wt, "src.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.reap(Request{})
	l.wantStdout("crew reap: keeping " + rb + " — uncommitted changes")
	wantDir(t, l.wt, true)
	l.noKill()
	if rows := l.rowsOf("reap", rb); len(rows) != 0 {
		t.Fatalf("reap rows %q", rows)
	}
}

// (c)
func TestLayout2OpenPRKept(t *testing.T) {
	l := newL2(t)
	l.gh["pr view "+prURL+" --json state --jq .state"] = "OPEN"
	l.reap(Request{})
	l.wantStdout("crew reap: keeping " + rb)
	wantDir(t, l.wt, true)
	if rows := l.rowsOf("reap", rb); len(rows) != 0 {
		t.Fatalf("reap rows %q", rows)
	}
}

// (d)
func TestLayout2DispatchForOtherRepoIgnored(t *testing.T) {
	l := newL2(t)
	l.crewRows(repoDispatchRow(rb, "c2", "/other/.git", int(nowUnix-100)*1000), l2Status(rb, "s1", "done", prURL, 0))
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	wantDir(t, l.wt, true)
	if strings.Contains(l.stdout.String(), rb) {
		t.Fatalf("stdout %q names %s", l.stdout.String(), rb)
	}
}

// (e)
func TestLayout2UnindexedCrewIgnored(t *testing.T) {
	l := newL2(t)
	if err := os.RemoveAll(filepath.Dir(bus.IndexPath(l.paths.State, l.tag))); err != nil {
		t.Fatal(err)
	}
	l.index("c3")
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	wantDir(t, l.wt, true)
	if strings.Contains(l.stdout.String(), rb) {
		t.Fatalf("stdout %q names %s", l.stdout.String(), rb)
	}
}

// (f)
func TestLayout2RecordOtherCrewKept(t *testing.T) {
	l := newL2(t)
	l.window("c2")
	l.record(l.wt, l.crewDir(), "c9")
	l.reap(Request{})
	l.keptUnvalidated()
}

// (g)
func TestLayout2RecordOtherRepoKept(t *testing.T) {
	l := newL2(t)
	l.window("c2")
	l.record(l.wt, filepath.Join(l.root, "other", ".git", "crew"), "c2")
	l.reap(Request{})
	l.keptUnvalidated()

	// A missing record, or one that is not a regular file, keeps too.
	anchor, _ := wtgit.AnchorPath(l.wt)
	if err := os.Remove(anchor); err != nil {
		t.Fatal(err)
	}
	l.reap(Request{})
	l.keptUnvalidated()
	if err := os.Symlink(filepath.Join(l.root, "elsewhere"), anchor); err != nil {
		t.Fatal(err)
	}
	l.reap(Request{})
	l.keptUnvalidated()
}

// (h)
func TestLayout2WindowOtherCrewKept(t *testing.T) {
	l := newL2(t)
	l.window("c9")
	l.reap(Request{})
	l.keptUnvalidated()
}

// The kill-time re-check: a window stamped for another crew that appears
// after the candidate gate passed keeps the branch and kills nothing.
func TestLayout2KillTimeWindowRecheck(t *testing.T) {
	l := newL2(t)
	listed := 0
	l.tmuxHook = func(key string) {
		if key != l2Windows {
			return
		}
		if listed++; listed == 2 {
			l.window("c9")
		}
	}
	l.reap(Request{})
	l.keptUnvalidated()
}

// (i)
func TestLayout2IdleReleaseChecksCrewID(t *testing.T) {
	l := newL2(t)
	l.crewRows(repoDispatchRow(rb, "c2", l.tag, int(nowUnix-1000)*1000), l2Status(rb, "s1", "done", "", 600))
	l.ghFail["pr list --head "+rb+" --state all --json url,state,headRefOid,isCrossRepository"] = true
	occupied := func(crew string) {
		l.tmux["list-windows -a -F #{window_id}\t#{@crew_name}\t#{pane_current_path}"] = "@5\tworker\t" + l.wt
		l.tmux["list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}"] = "@5\t%5\tzsh"
		l.tmux["display-message -p -t @5 #{@crew_id}"] = crew
	}
	occupied("c9")
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	l.wantStdout("crew reap: keeping " + rb + " — layout-2 row not validated: ")
	l.noKill()
	if rows := l.rowsOf("release", rb); len(rows) != 0 {
		t.Fatalf("release rows %q", rows)
	}

	occupied("c2")
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	l.wantStdout("crew reap: released @5 at " + l.wt + " (" + rb + " done)")
	if rows := l.rowsOf("release", rb); len(rows) != 1 || rows[0][0] != l.crewLog {
		t.Fatalf("release rows %q, want one on %s", rows, l.crewLog)
	}
}

// (j) A layout-1 worker in the repo log reaps exactly as it does without the
// layout-2 crew beside it.
func TestLayout2MixedRepoLayout1Unchanged(t *testing.T) {
	layout1 := func(f *fx) {
		f.bus(status("done", prURL, 0))
		f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	}
	base := newFx(t)
	layout1(base)
	if rc := base.reap(base.main, Request{}); rc != 0 {
		t.Fatalf("baseline rc=%d stderr=%q", rc, base.stderr.String())
	}
	baseRows := rbLines(base.appended, base.root)
	baseLines := rbLines(strings.Split(base.stdout.String(), "\n"), base.root)

	l := newL2(t)
	layout1(l.fx)
	const xb, xpr = "feat/x", "https://github.com/o/r/pull/8"
	xwt := filepath.Join(l.root, "wtx")
	runGit(t, l.main, "worktree", "add", "-q", "-b", xb, xwt)
	if err := os.Remove(l.record0()); err != nil {
		t.Fatal(err)
	}
	l.record(xwt, l.crewDir(), "c2")
	l.crewRows(repoDispatchRow(xb, "c2", l.tag, int(nowUnix-100)*1000), l2Status(xb, "s1", "done", xpr, 0))
	l.gh["pr view "+xpr+" --json state --jq .state"] = "MERGED"
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	if lines := rbLines(strings.Split(l.stdout.String(), "\n"), l.root); !slices.Equal(lines, baseLines) {
		t.Fatalf("layout-1 lines %q, want %q", lines, baseLines)
	}
	var rows []string
	for _, r := range l.rows {
		if !strings.Contains(r[1], rb) {
			continue
		}
		if r[0] != l.paths.Log {
			t.Fatalf("layout-1 row %q went to %s", r[1], r[0])
		}
		rows = append(rows, r[1])
	}
	if rows := rbLines(rows, l.root); !slices.Equal(rows, baseRows) {
		t.Fatalf("layout-1 rows %q, want %q", rows, baseRows)
	}
	wantDir(t, l.wt, false)
	wantDir(t, xwt, false)
	if xr := l.rowsOf("reap", xb); len(xr) != 1 || xr[0][0] != l.crewLog {
		t.Fatalf("layout-2 reap rows %q, want one on %s", xr, l.crewLog)
	}
}

// rbLines are the lines naming rb, the scenario's temp root made neutral.
func rbLines(lines []string, root string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, rb) {
			out = append(out, strings.ReplaceAll(line, root, "<root>"))
		}
	}
	return out
}

// record0 is rb's worktree record path.
func (l *l2fx) record0() string {
	l.t.Helper()
	anchor, err := wtgit.AnchorPath(l.wt)
	if err != nil {
		l.t.Fatal(err)
	}
	return anchor
}

// (k)
func TestLayout2DiscardRefusesUnvalidated(t *testing.T) {
	l := newL2(t)
	l.record(l.wt, l.crewDir(), "c9")
	if err := os.WriteFile(filepath.Join(l.wt, "src.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := l.reap(Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d, want 1; stdout=%q", rc, l.stdout.String())
	}
	if got := l.stderr.String(); !strings.HasPrefix(got, "crew reap: refusing --discard "+rb+" — layout-2 row not validated: ") {
		t.Fatalf("stderr %q", got)
	}
	wantDir(t, l.wt, true)
	if _, err := os.Stat(filepath.Join(l.wt, "src.go")); err != nil {
		t.Fatalf("src.go: %v", err)
	}
	if len(l.rows) != 0 {
		t.Fatalf("rows %q", l.rows)
	}

	// A valid record with a window stamped for another crew: remove refuses
	// before it trashes, kills or removes anything.
	l.record(l.wt, l.crewDir(), "c2")
	l.window("c9")
	if rc := l.reap(Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d, want 1; stdout=%q stderr=%q", rc, l.stdout.String(), l.stderr.String())
	}
	l.keptUnvalidated()
	if _, err := os.Stat(filepath.Join(l.wt, "src.go")); err != nil {
		t.Fatalf("src.go: %v", err)
	}
}

// (l)
func TestLayout2OrphanWindowChecksCrewID(t *testing.T) {
	l := newL2(t)
	runGit(t, l.main, "worktree", "remove", "--force", l.wt)
	const orphans = "list-windows -a -F #{window_id}\t#{@crew_branch}\t#{@crew_dir}"
	l.gh["pr list --head "+rb+" --state all --json url,state,headRefOid,isCrossRepository"] =
		`[{"url":"` + prURL + `","state":"MERGED","headRefOid":"abc","isCrossRepository":false}]`
	stamp := func(crew string) {
		l.tmux[orphans] = "@9\t" + rb + "\t" + l.paths.Dir
		l.tmux["display-message -p -t @9 #{@crew_id}"] = crew
	}
	stamp("c9")
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	l.wantStdout("layout-2 window not validated: ")
	l.noKill()

	stamp("c2")
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	l.wantStdout("crew reap: closed window @9 (" + rb + ", PR #7 MERGED, worktree already gone)")
	if !slices.Contains(l.calls, "tmux kill-window -t @9") {
		t.Fatalf("calls %q, want @9 killed", l.calls)
	}
}

// (m) A layout-1 candidate whose branch a layout-2 crew dispatches after the
// run started is validated as layout 2: provenance is not cached.
func TestLayout2ProvenanceRecomputedAtGate(t *testing.T) {
	l := newL2(t)
	const xb = "feat/x"
	l.crewRows(repoDispatchRow(xb, "c2", l.tag, int(nowUnix-100)*1000))
	l.bus(status("done", prURL, 0))
	writeRecord(t, l.wt, "c1")
	l.tmuxHook = func(key string) {
		if key == l2Live {
			l.crewRows(repoDispatchRow(xb, "c2", l.tag, int(nowUnix-100)*1000), repoDispatchRow(rb, "c2", l.tag, int(nowUnix-50)*1000))
		}
	}
	l.reap(Request{})
	l.keptUnvalidated()
}

// `crew reap --fleet` finds a repo with no repo log through the repo index and
// reaps its merged layout-2 worker with the real Run.
func TestLayout2FleetReapsIndexOnlyRepo(t *testing.T) {
	l := newL2(t)
	l.window("c2")
	t.Setenv("CREW_SWEEP_ROOTS", t.TempDir())
	registry := filepath.Join(t.TempDir(), "repos")
	if err := os.WriteFile(registry, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := RunFleet(context.Background(), Request{}, &l.stdout, &l.stderr, l.options(), FleetOptions{Getenv: os.Getenv, Registry: registry}); rc != 0 {
		t.Fatalf("rc=%d stdout=%q stderr=%q", rc, l.stdout.String(), l.stderr.String())
	}
	l.wantStdout("crew reap: reaped " + rb + " (MERGED)")
	wantDir(t, l.wt, false)
	if rows := l.rowsOf("reap", rb); len(rows) != 1 || rows[0][0] != l.crewLog {
		t.Fatalf("reap rows %q, want one on %s", rows, l.crewLog)
	}
}

// A status row whose worker From names another branch than its branch field
// is not admitted: the folds would file it under the From's branch.
func TestLayout2MisfiledStatusIgnored(t *testing.T) {
	l := newL2(t)
	const xb = "feat/x"
	l.crewRows(repoDispatchRow(xb, "c2", l.tag, int(nowUnix-100)*1000),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c2","branch":%q,"from":"worker:%s#s1","to":"dispatcher:c2","kind":"status","body":{"state":"done","pr_url":%q}}`,
			nowUnix*1000, xb, rb, prURL))
	l.window("c2")
	if rc := l.reap(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, l.stderr.String())
	}
	wantDir(t, l.wt, true)
	l.noKill()
	if rows := l.rowsOf("reap", rb); len(rows) != 0 {
		t.Fatalf("reap rows %q", rows)
	}
}

// A torn crew log fails the fold the way a torn repo log does.
func TestLayout2TornCrewLogFailsFold(t *testing.T) {
	l := newL2(t)
	l.window("c2")
	l.crewRows(repoDispatchRow(rb, "c2", l.tag, int(nowUnix-100)*1000), l2Status(rb, "s1", "done", prURL, 0), "torn{")
	if rc := l.reap(Request{}); rc != 5 {
		t.Fatalf("rc=%d, want 5; stdout=%q stderr=%q", rc, l.stdout.String(), l.stderr.String())
	}
	wantDir(t, l.wt, true)
	l.noKill()
	if len(l.rows) != 0 {
		t.Fatalf("rows %q", l.rows)
	}
}

// A layout-2 claim that vanishes, or changes crew, between the fold and the
// gate keeps the branch: it is not reaped as layout 1.
func TestLayout2ClaimChangedBeforeGate(t *testing.T) {
	for name, change := range map[string]func(l *l2fx){
		"claim vanished": func(l *l2fx) {
			if err := os.RemoveAll(filepath.Dir(bus.IndexPath(l.paths.State, l.tag))); err != nil {
				l.t.Fatal(err)
			}
		},
		"owner changed": func(l *l2fx) {
			if err := bus.CreateMeta(l.paths.State, bus.Meta{ID: "c3", CreatedBy: "register", HomeRepo: &l.tag}); err != nil {
				l.t.Fatal(err)
			}
			l.index("c3")
			c, err := l.paths.Crew("c3")
			if err != nil {
				l.t.Fatal(err)
			}
			writeLog(l.t, c.Log, repoDispatchRow(rb, "c3", l.tag, int(nowUnix-10)*1000))
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newL2(t)
			l.window("c2")
			changed := false
			l.tmuxHook = func(key string) {
				if key == l2Live && !changed {
					changed = true
					change(l)
				}
			}
			l.reap(Request{})
			l.keptUnvalidated()
			l.wantStdout("layout-2 row not validated: layout-2 " + name)
		})
	}
}
