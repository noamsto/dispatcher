package where

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	code = Run(args, tr.paths, &o, &e, Options{CrewID: crewID, Probes: p})
	return o.String(), e.String(), code
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

func TestFlagRefusals(t *testing.T) {
	tr := setup(t)
	const usage = "crew: where: usage: crew where <codename|branch|%id> [--crew ID]"
	cases := []struct {
		args []string
		want string
	}{
		{nil, usage + "\n"},
		{[]string{"--crew"}, "crew: where: --crew needs an id (usage: crew where <codename|branch|%id> [--crew ID])\n"},
		{[]string{"--crew", "", "nova"}, "crew: where: --crew needs an id (usage: crew where <codename|branch|%id> [--crew ID])\n"},
		{[]string{"--bogus", "nova"}, "crew: where: unknown flag '--bogus' (usage: crew where <codename|branch|%id> [--crew ID])\n"},
		{[]string{"nova", "iris"}, "crew: where: one target only (usage: crew where <codename|branch|%id> [--crew ID])\n"},
	}
	for _, c := range cases {
		// A refused argument never reaches tmux, so poisoned probes are fine.
		poison := Probes{
			Windows: func() (string, error) { return "", errors.New("must not run") },
			Panes:   func() (string, error) { return "", errors.New("must not run") },
		}
		out, errOut, code := tr.runProbes(t, poison, crewIs("c1"), c.args...)
		if code != exitFailure || out != "" || errOut != c.want {
			t.Errorf("%v: code %d, out %q, err %q, want %q", c.args, code, out, errOut, c.want)
		}
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
