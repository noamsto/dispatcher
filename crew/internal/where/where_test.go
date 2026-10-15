package where

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/identity"
)

// tree is a bus without git: Run reads bus.Paths for the crew dir it anchors on
// and for the resolver's log, so a temp dir stands in for the common dir.
type tree struct {
	paths bus.Paths
}

func setup(t *testing.T) *tree {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tree{paths: bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}}
}

// write seeds the bus the resolver falls back to; no rows means no log file.
func (tr *tree) write(t *testing.T, rows ...string) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	if err := os.MkdirAll(tr.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr.paths.Log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run is one arm invocation over stubbed tmux. crewID is the `_crew_id` default;
// "" is a repo with neither CREW_ID nor a WORKER_TASK.md crew_id.
func (tr *tree) run(t *testing.T, wins, panes string, crewID func() string, args ...string) (out, errOut string, code int) {
	t.Helper()
	return tr.runProbes(t, okProbes(wins, panes), crewID, args...)
}

func (tr *tree) runProbes(t *testing.T, p Probes, crewID func() string, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	code = Run(request(args), tr.paths, &o, &e, Options{CrewID: crewID, Probes: p})
	return o.String(), e.String(), code
}

// request reads the test's `[--crew ID] target` shorthand; the argv grammar
// itself is main's (TestWhereRefusals).
func request(args []string) Request {
	var r Request
	if len(args) == 3 && args[0] == "--crew" {
		r.Crew, args = args[1], args[2:]
	}
	r.Target = args[0]
	return r
}

func okProbes(wins, panes string) Probes {
	return Probes{
		Windows: func() (string, error) { return wins, nil },
		Panes:   func() (string, error) { return panes, nil },
	}
}

// win is one `list-windows` line, in the arm's eight-field format: window id,
// branch, crew dir, crew id, codename, session, window index, window name.
func winLine(dir, id, branch, crew, name, session, index, winName string) string {
	return strings.Join([]string{id, branch, dir, crew, name, session, index, winName}, "\t")
}

// pane is one `list-panes` line: window id, pane id, role, pane index.
func paneLine(winID, paneID, role, index string) string {
	return strings.Join([]string{winID, paneID, role, index}, "\t")
}

func dispatch(ts int64, crew, branch, name string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q,"name":%q,"host":"h1"}`, ts, crew, branch, name)
}

func crewIs(crew string) func() string { return func() string { return crew } }

// The line people and agents read, and the dispatcher relays: byte-exact, em
// dash and the three spaces before `jump:` included.
func TestLeadPaneLine(t *testing.T) {
	tr := setup(t)
	wins := winLine(tr.paths.Dir, "@624", "feat/618-x", "c1", "nova", "sess", "3", "win-name")
	panes := paneLine("@624", "%204", "lead", "1") + "\n" + paneLine("@624", "%205", "plan-critic", "2")
	want := `nova — sess:3.1 "win-name" (lead pane)   jump: ! tmux switch-client -t %204` + "\n"

	for _, target := range []string{"nova", "feat/618-x"} {
		out, errOut, code := tr.run(t, wins, panes, crewIs("c1"), target)
		if code != 0 || out != want || errOut != "" {
			t.Errorf("%q: code %d, out %q, err %q", target, code, out, errOut)
		}
	}
	// A pane id keeps that pane and its own role label.
	out, _, code := tr.run(t, wins, panes, crewIs("c1"), "%205")
	wantRole := `nova — sess:3.2 "win-name" (plan-critic pane)   jump: ! tmux switch-client -t %205` + "\n"
	if code != 0 || out != wantRole {
		t.Errorf("%%205: code %d, out %q", code, out)
	}
}

// A branch name may carry a double quote (`git check-ref-format --branch
// 'feat/a"b'` passes) and dispatch names the window from it, so the printed
// name is the raw one between literal quotes.
func TestWindowNamePrintsRaw(t *testing.T) {
	tr := setup(t)
	name := `win"x\y`
	wins := winLine(tr.paths.Dir, "@624", `feat/a"b`, "c1", "nova", "sess", "3", name)
	panes := paneLine("@624", "%204", "lead", "1")
	want := `nova — sess:3.1 "` + name + `" (lead pane)   jump: ! tmux switch-client -t %204` + "\n"

	out, errOut, code := tr.run(t, wins, panes, crewIs("c1"), `feat/a"b`)
	if code != 0 || out != want || errOut != "" {
		t.Errorf("code %d, out %q, err %q, want %q", code, out, errOut, want)
	}
}

// A plain (non-grid) dispatch window stamps no @crew_role: its lone pane is
// lead, and a window with no pane at all is refused.
func TestRoleLessWindowIsLead(t *testing.T) {
	tr := setup(t)
	wins := winLine(tr.paths.Dir, "@624", "feat/618-x", "c1", "nova", "sess", "3", "win-name")
	out, _, code := tr.run(t, wins, paneLine("@624", "%204", "", "1"), crewIs("c1"), "nova")
	want := `nova — sess:3.1 "win-name" (lead pane)   jump: ! tmux switch-client -t %204` + "\n"
	if code != 0 || out != want {
		t.Errorf("code %d, out %q", code, out)
	}

	// No pane in the window at all.
	_, errOut, code := tr.run(t, wins, paneLine("@700", "%900", "lead", "1"), crewIs("c1"), "nova")
	if code != exitFailure || errOut != "crew: where: no pane in the window for 'nova'\n" {
		t.Errorf("code %d, err %q", code, errOut)
	}
}

// An older window stamped no @crew_name: the pool identity the dispatch would
// have stamped, which is the codename `_identity` printed.
func TestPoolNameFallback(t *testing.T) {
	tr := setup(t)
	wins := winLine(tr.paths.Dir, "@624", "feat/618-x", "c1", "", "sess", "3", "win-name")
	out, _, code := tr.run(t, wins, paneLine("@624", "%204", "lead", "1"), crewIs("c1"), "feat/618-x")
	id, _ := identity.At(identity.Slot("feat/618-x")).Get("name")
	pool, _ := id.AsString()
	want := fmt.Sprintf("%s — sess:3.1 %q (lead pane)   jump: ! tmux switch-client -t %%204\n", pool, "win-name")
	if code != 0 || out != want {
		t.Errorf("code %d, out %q, want %q", code, out, want)
	}
}

// The crew anchor: an empty crew id filters nothing, a window carrying no crew
// belongs to any crew, and a named crew drops another crew's window — and its
// pane refuses as what it is, not as "no pane".
func TestCrewAnchor(t *testing.T) {
	tr := setup(t)
	wins := winLine(tr.paths.Dir, "@624", "feat/1-a", "c1", "nova", "sess", "3", "win-a") + "\n" +
		winLine(tr.paths.Dir, "@625", "feat/2-b", "c2", "iris", "sess", "4", "win-b") + "\n" +
		winLine(tr.paths.Dir, "@626", "feat/3-c", "", "juniper", "sess", "5", "win-c")
	panes := paneLine("@624", "%204", "lead", "1") + "\n" +
		paneLine("@625", "%304", "lead", "1") + "\n" +
		paneLine("@626", "%404", "lead", "1")

	// A named crew sees its own window and the crew-less one, not the other's.
	if out, _, code := tr.run(t, wins, panes, crewIs("c1"), "nova"); code != 0 ||
		out != `nova — sess:3.1 "win-a" (lead pane)   jump: ! tmux switch-client -t %204`+"\n" {
		t.Errorf("own window: code %d, out %q", code, out)
	}
	if out, _, code := tr.run(t, wins, panes, crewIs("c1"), "juniper"); code != 0 ||
		out != `juniper — sess:5.1 "win-c" (lead pane)   jump: ! tmux switch-client -t %404`+"\n" {
		t.Errorf("crew-less window: code %d, out %q", code, out)
	}
	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "iris"); code != exitFailure ||
		errOut != "crew: where: no worker matches 'iris'\n" {
		t.Errorf("other crew: code %d, err %q", code, errOut)
	}
	// An empty crew id is the arm's no-filter path: every window in the repo.
	if out, _, code := tr.run(t, wins, panes, crewIs(""), "iris"); code != 0 ||
		out != `iris — sess:4.1 "win-b" (lead pane)   jump: ! tmux switch-client -t %304`+"\n" {
		t.Errorf("no crew id: code %d, out %q", code, out)
	}
	// `--crew` overrides the default, in both directions.
	if out, _, code := tr.run(t, wins, panes, crewIs("c1"), "--crew", "c2", "iris"); code != 0 ||
		out != `iris — sess:4.1 "win-b" (lead pane)   jump: ! tmux switch-client -t %304`+"\n" {
		t.Errorf("--crew c2: code %d, out %q", code, out)
	}
	// A pane of another crew's window is named, not missing.
	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "%304"); code != exitFailure ||
		errOut != "crew: where: pane %304 is not a pane of this crew\n" {
		t.Errorf("other crew's pane: code %d, err %q", code, errOut)
	}
}

// A window of another repo, or one with no branch, is not this crew's: the arm
// anchored on the crew dir and a non-empty branch.
func TestAnchorExcludesOtherRepos(t *testing.T) {
	tr := setup(t)
	wins := winLine(tr.paths.Dir+"/elsewhere", "@624", "feat/1-a", "c1", "nova", "sess", "3", "win-a") + "\n" +
		winLine(tr.paths.Dir, "@625", "", "c1", "iris", "sess", "4", "win-b") + "\n" +
		"a short line"
	panes := paneLine("@624", "%204", "lead", "1") + "\n" + paneLine("@625", "%304", "lead", "1")
	for _, target := range []string{"nova", "iris"} {
		if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), target); code != exitFailure ||
			errOut != fmt.Sprintf("crew: where: no worker matches '%s'\n", target) {
			t.Errorf("%q: code %d, err %q", target, code, errOut)
		}
	}
}

func TestPaneIDRefusals(t *testing.T) {
	tr := setup(t)
	wins := winLine(tr.paths.Dir, "@624", "feat/1-a", "c1", "nova", "sess", "3", "win-a")
	panes := paneLine("@624", "%204", "lead", "1")

	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "%999"); code != exitFailure ||
		errOut != "crew: where: no pane %999\n" {
		t.Errorf("unknown pane: code %d, err %q", code, errOut)
	}
	// A target that starts with `%` but has no digit is a name, not a pane id.
	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "%lead"); code != exitFailure ||
		errOut != "crew: where: no worker matches '%lead'\n" {
		t.Errorf("not a pane id: code %d, err %q", code, errOut)
	}
}

// An ambiguous codename names every branch it matched, in window order.
func TestAmbiguousCodename(t *testing.T) {
	tr := setup(t)
	wins := winLine(tr.paths.Dir, "@624", "feat/1-a", "c1", "nova", "sess", "3", "win-a") + "\n" +
		winLine(tr.paths.Dir, "@625", "feat/2-b", "c1", "nova", "sess", "4", "win-b")
	panes := paneLine("@624", "%204", "lead", "1") + "\n" + paneLine("@625", "%304", "lead", "1")
	_, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "nova")
	want := "crew: where: ambiguous codename 'nova' — matches feat/1-a,feat/2-b; pass a branch or %pane\n"
	if code != exitFailure || errOut != want {
		t.Errorf("code %d, err %q, want %q", code, errOut, want)
	}
}

// A branch that matches exactly wins over the codename scan, and a target that
// is neither but the bus knows is refused as the gone window it is: the arm read
// the resolver's branch list, and its shape was its message.
func TestResolverBackedRefusals(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(1, "c1", "feat/9-gone", "sage"),
		dispatch(2, "c1", "feat/10-gone", "sage"),
		`{"ts":3,"kind":"dispatch","broken`,
	)
	wins := winLine(tr.paths.Dir, "@624", "feat/1-a", "c1", "nova", "sess", "3", "win-a")
	panes := paneLine("@624", "%204", "lead", "1")

	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "feat/9-gone"); code != exitFailure ||
		errOut != "crew: where: no live pane for 'feat/9-gone' (branch feat/9-gone) — its window is gone\n" {
		t.Errorf("gone window: code %d, err %q", code, errOut)
	}
	// The codename is gone too, and names both branches.
	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "sage"); code != exitFailure ||
		errOut != "crew: where: ambiguous target 'sage' — matches feat/10-gone,feat/9-gone; pass a branch or %pane\n" {
		t.Errorf("ambiguous target: code %d, err %q", code, errOut)
	}
	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "nobody"); code != exitFailure ||
		errOut != "crew: where: no worker matches 'nobody'\n" {
		t.Errorf("unknown target: code %d, err %q", code, errOut)
	}
	// A corrupt line costs the resolver nothing: its neighbours still resolve.
	if _, errOut, code := tr.run(t, wins, panes, crewIs("c1"), "feat/9-gone"); code != exitFailure ||
		!strings.Contains(errOut, "its window is gone") {
		t.Errorf("torn tail: code %d, err %q", code, errOut)
	}
}

// A tmux read failure is not "the target is gone": the arm named which read
// broke, windows first.
func TestTmuxFailure(t *testing.T) {
	tr := setup(t)
	boom := errors.New("no server running on /tmp/tmux-1000/default")

	_, errOut, code := tr.runProbes(t, Probes{
		Windows: func() (string, error) { return "", boom },
		Panes:   func() (string, error) { return "", boom },
	}, crewIs("c1"), "nova")
	if code != exitFailure || errOut != "crew: where: cannot read tmux windows (is a tmux server running?)\n" {
		t.Errorf("windows: code %d, err %q", code, errOut)
	}

	_, errOut, code = tr.runProbes(t, Probes{
		Windows: func() (string, error) { return "", nil },
		Panes:   func() (string, error) { return "", boom },
	}, crewIs("c1"), "nova")
	if code != exitFailure || errOut != "crew: where: cannot read tmux panes (is a tmux server running?)\n" {
		t.Errorf("panes: code %d, err %q", code, errOut)
	}
}

// mirrorLine is a `list-windows` line of a mirror window: the eight fields with
// no @crew_branch, then @bridge_crew_branch and @bridge_crew_id.
func mirrorLine(id, bridgeBranch, bridgeCrew, session, index, winName string) string {
	return strings.Join([]string{id, "", "", "", "", session, index, winName, bridgeBranch, bridgeCrew}, "\t")
}

func remoteDispatch(ts int64, branch, name, host string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":%q,"name":%q,"host":%q,"remote":true}`, ts, branch, name, host)
}

type remoteCall struct {
	host string
	argv []string
}

// runRemote is one invocation with Options.Remote answering as the host would.
func (tr *tree) runRemote(t *testing.T, wins, panes string, remote func(string, []string, io.Writer) int, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	code = Run(request(args), tr.paths, &o, &e, Options{CrewID: crewIs("c1"), Probes: okProbes(wins, panes), Remote: remote})
	return o.String(), e.String(), code
}

const farLine = `iris — mini-sess:2.1 "far-win" (lead pane)   jump: ! tmux switch-client -t %9`

func TestRemoteWorkerRoutesToItsHost(t *testing.T) {
	for _, target := range []string{"iris", "feat/far"} {
		t.Run(target, func(t *testing.T) {
			tr := setup(t)
			tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"))
			var calls []remoteCall
			out, errOut, code := tr.runRemote(t, "", "", func(host string, argv []string, stdout io.Writer) int {
				calls = append(calls, remoteCall{host, argv})
				_, _ = io.WriteString(stdout, farLine+"\n")
				return 0
			}, target)
			want := farLine + " [host mini]\n"
			if code != 0 || out != want || errOut != "" {
				t.Errorf("code %d, out %q, err %q; want %q", code, out, errOut, want)
			}
			wantCalls := []remoteCall{{"mini", []string{"crew", "where", "--crew", "c1", "feat/far"}}}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Errorf("remote calls = %v, want %v", calls, wantCalls)
			}
		})
	}
}

func TestRemoteFailureIsPassedThrough(t *testing.T) {
	tr := setup(t)
	tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"))
	out, errOut, code := tr.runRemote(t, "", "", func(string, []string, io.Writer) int { return 255 }, "iris")
	if code != 255 || out != "" || errOut != "" {
		t.Errorf("code %d, out %q, err %q; want 255 and nothing printed here", code, out, errOut)
	}
}

func TestMirrorWindowIsTheJumpTarget(t *testing.T) {
	tr := setup(t)
	tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"))
	panes := paneLine("@40", "%88", "", "1")
	remote := func(string, []string, io.Writer) int { t.Fatal("a mirrored worker was routed"); return 0 }
	want := `iris — local:4.1 "far-win" (lead pane)   jump: ! tmux switch-client -t %88 [host mini]` + "\n"
	for name, wins := range map[string]string{
		"crew id set":   mirrorLine("@40", "feat/far", "c1", "local", "4", "far-win"),
		"crew id unset": mirrorLine("@40", "feat/far", "", "local", "4", "far-win"),
	} {
		t.Run(name, func(t *testing.T) {
			out, errOut, code := tr.runRemote(t, wins, panes, remote, "iris")
			if code != 0 || out != want || errOut != "" {
				t.Errorf("code %d, out %q, err %q; want %q", code, out, errOut, want)
			}
		})
	}
}

func TestMirrorWindowOfAnotherBranchOrCrewIsNotAHit(t *testing.T) {
	tr := setup(t)
	tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"))
	wins := mirrorLine("@40", "feat/other", "c1", "local", "4", "w") + "\n" + mirrorLine("@41", "feat/far", "c2", "local", "5", "w")
	var called bool
	out, _, code := tr.runRemote(t, wins, paneLine("@40", "%88", "", "1"), func(_ string, _ []string, stdout io.Writer) int {
		called = true
		_, _ = io.WriteString(stdout, farLine+"\n")
		return 0
	}, "iris")
	if !called || code != 0 || out != farLine+" [host mini]\n" {
		t.Errorf("called %t, code %d, out %q; want the remote's answer", called, code, out)
	}
}

func TestTargetOnBothSidesIsAmbiguous(t *testing.T) {
	tr := setup(t)
	tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"))
	wins := winLine(tr.paths.Dir, "@624", "feat/here", "c1", "iris", "sess", "3", "w")
	out, errOut, code := tr.runRemote(t, wins, paneLine("@624", "%204", "lead", "1"),
		func(string, []string, io.Writer) int { t.Fatal("an ambiguous target was routed"); return 0 }, "iris")
	want := "crew: where: ambiguous target 'iris' — matches a local worker (feat/here) and a remote one on mini (feat/far); pass a branch\n"
	if code != 1 || out != "" || errOut != want {
		t.Errorf("code %d, out %q, err %q; want 1 and %q", code, out, errOut, want)
	}
}

func TestPaneIDIsNeverRouted(t *testing.T) {
	tr := setup(t)
	tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"))
	_, errOut, code := tr.runRemote(t, "", "",
		func(string, []string, io.Writer) int { t.Fatal("a pane id was routed"); return 0 }, "%9")
	if code != 1 || !strings.Contains(errOut, "no pane %9") {
		t.Errorf("code %d, err %q; want the local `no pane` refusal", code, errOut)
	}
}

// A local worker answers exactly as it did, whether or not the bus holds remote rows.
func TestLocalWorkerOutputIsUnchangedByRouting(t *testing.T) {
	tr := setup(t)
	tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"), dispatch(2, "c1", "feat/618-x", "nova"))
	wins := winLine(tr.paths.Dir, "@624", "feat/618-x", "c1", "nova", "sess", "3", "win-name")
	panes := paneLine("@624", "%204", "lead", "1")
	want := `nova — sess:3.1 "win-name" (lead pane)   jump: ! tmux switch-client -t %204` + "\n"
	remote := func(string, []string, io.Writer) int { t.Fatal("a local worker was routed"); return 0 }
	out, errOut, code := tr.runRemote(t, wins, panes, remote, "nova")
	if code != 0 || out != want || errOut != "" {
		t.Errorf("code %d, out %q, err %q", code, out, errOut)
	}
}
