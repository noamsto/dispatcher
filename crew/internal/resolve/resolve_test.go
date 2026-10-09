package resolve

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// tree is a bus without git: Run reads bus.Paths, so a temp dir stands in for
// the common dir.
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

// write seeds the bus; no rows means no log file at all, which is the helper's
// `[ -f "$log" ]` miss rather than an empty one.
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

// run is one arm invocation: stdout and stderr apart, since which stream a line
// takes is part of the contract.
func (tr *tree) run(t *testing.T, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	code = Run(args, tr.paths, &o, &e)
	return o.String(), e.String(), code
}

// dispatch is a `dispatch` row with the four columns the arm prints, plus any
// extra keys the caller needs (also_closes, a second session).
func dispatch(ts int64, crew, branch, name, host string, extra ...string) string {
	fields := fmt.Sprintf(`"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q,"name":%q,"host":%q`, ts, crew, branch, name, host)
	for _, x := range extra {
		fields += "," + x
	}
	return "{" + fields + "}"
}

func TestTargetShapes(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(1, "c1", "feat/9-gone", "sage", "h1"),
		dispatch(2, "c1", "eng-12-thing", "nova", "", `"also_closes":["ENG-13"]`),
		dispatch(3, "c1", "fix/10-b", "nova", "h2"),
		`{"ts":4,"crew_id":"c1","kind":"status","from":"worker:feat/9-gone#s1-1","body":{"state":"working"}}`,
	)
	want := "feat/9-gone\tsage\th1\tc1\n"
	for _, target := range []string{"#9", "9", "feat/9-gone", "sage", "worker:feat/9-gone#s1-2", "worker:feat/9-gone"} {
		out, errOut, code := tr.run(t, target)
		if code != 0 || out != want || errOut != "" {
			t.Errorf("%q: code %d, out %q, err %q", target, code, out, errOut)
		}
	}

	// A Linear id matches the branch and either side of `also_closes`, in either
	// case; the empty host stays an empty column, not a shifted row.
	for _, target := range []string{"ENG-12", "eng-12", "ENG-13", "eng-13"} {
		out, _, code := tr.run(t, target)
		if code != 0 || out != "eng-12-thing\tnova\t\tc1\n" {
			t.Errorf("%q: code %d, out %q", target, code, out)
		}
	}
	if out, _, code := tr.run(t, "fix/10-b"); code != 0 || out != "fix/10-b\tnova\th2\tc1\n" {
		t.Errorf("branch: code %d, out %q", code, out)
	}
}

// The codename is matched whole, so a branch that merely contains the digits is
// no issue match, and `#N` needs the branch shape `(^|/)N-`.
func TestTargetKinds(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(1, "c1", "feat/119-x", "ivy", "h1"),
		dispatch(2, "c1", "eng-14-y", "kai", "h1"),
	)
	if _, _, code := tr.run(t, "19"); code != exitFailure {
		t.Errorf("bare 19 against feat/119-x: code %d, want 1", code)
	}
	out, _, code := tr.run(t, "119")
	if code != 0 || out != "feat/119-x\tivy\th1\tc1\n" {
		t.Errorf("119: code %d, out %q", code, out)
	}
	// A bare `name-14` is not a Linear id: only `eng-14-y`'s own branch shape is.
	if _, _, code := tr.run(t, "y-14"); code != exitFailure {
		t.Errorf("y-14: code %d, want 1", code)
	}
	if out, _, code := tr.run(t, "kai"); code != 0 || out != "eng-14-y\tkai\th1\tc1\n" {
		t.Errorf("codename: code %d, out %q", code, out)
	}
}

// A target with a trailing newline is classified the way the arm's Oniguruma
// classified it (`^[0-9]+$` matched before one final newline), which resolve.jq
// mirrors with `\\n?\\z`. What the row pins is the outcome: refused, not
// resolved as a name.
func TestTrailingNewlineTarget(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(1, "c1", "feat/9-a", "sage", "h1"),
		dispatch(2, "c1", "eng-12-b", "nova", "h1"),
	)
	for _, target := range []string{"9\n", "ENG-12\n", "sage\n"} {
		if out, errOut, code := tr.run(t, target); code != exitFailure || out != "" ||
			errOut != fmt.Sprintf("crew: resolve-target: no worker matches '%s'\n", target) {
			t.Errorf("%q: code %d, out %q, err %q", target, code, out, errOut)
		}
	}
	// Without the newline both shapes resolve normally.
	if out, _, code := tr.run(t, "9"); code != 0 || out != "feat/9-a\tsage\th1\tc1\n" {
		t.Errorf("issue: code %d, out %q", code, out)
	}
	if out, _, code := tr.run(t, "ENG-12"); code != 0 || out != "eng-12-b\tnova\th1\tc1\n" {
		t.Errorf("linear: code %d, out %q", code, out)
	}
}

// group_by(.branch) sorts, and max_by(.ts) keeps one row per branch: the order
// is the ambiguity list's order, and the newest dispatch row's columns win.
func TestNewestRowPerBranchInBranchOrder(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(5, "c1", "feat/b", "old-name", "old-host"),
		dispatch(9, "c1", "feat/b", "nova", "new-host"),
		dispatch(1, "c1", "feat/a", "nova", "h1"),
	)
	out, errOut, code := tr.run(t, "nova")
	want := "crew: resolve-target: ambiguous target 'nova' — matches: feat/a, feat/b; pass a branch\n"
	if code != exitAmbiguous || out != "" || errOut != want {
		t.Errorf("code %d, out %q, err %q, want %q", code, out, errOut, want)
	}

	// The newest row's own columns, not the first row's.
	tr.write(t,
		dispatch(5, "c1", "feat/b", "old-name", "old-host"),
		dispatch(9, "c1", "feat/b", "sage", "new-host"),
	)
	if out, _, code := tr.run(t, "sage"); code != 0 || out != "feat/b\tsage\tnew-host\tc1\n" {
		t.Errorf("newest: code %d, out %q", code, out)
	}
	// And a codename only an older row carried is no match: the branch's newest
	// row is the one the fold keeps.
	if _, _, code := tr.run(t, "old-name"); code != exitFailure {
		t.Errorf("superseded codename: code %d, want 1", code)
	}
}

// `paste -sd, -` then `sed 's/,/, /g'` spaces every comma of the joined line, so
// a comma inside a branch name (a legal ref character) splits there too: the
// list reads "feat/a, b, feat/c", not one entry per branch.
func TestAmbiguitySpacesEveryComma(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(1, "c1", "feat/c", "nova", "h1"),
		dispatch(5, "c1", "feat/a,b", "nova", "h1"),
	)
	out, errOut, code := tr.run(t, "nova")
	want := "crew: resolve-target: ambiguous target 'nova' — matches: feat/a, b, feat/c; pass a branch\n"
	if code != exitAmbiguous || out != "" || errOut != want {
		t.Errorf("code %d, out %q, err %q, want %q", code, out, errOut, want)
	}
}

// `--crew` is the only crew filter this arm has: with none, a row of any crew
// matches, and with one, a row of another does not.
func TestCrewFilter(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(1, "c1", "feat/x", "sage", "h1"),
		dispatch(2, "c2", "feat/y", "nova", "h2"),
	)
	if out, _, code := tr.run(t, "sage"); code != 0 || out != "feat/x\tsage\th1\tc1\n" {
		t.Errorf("no --crew: code %d, out %q", code, out)
	}
	if _, errOut, code := tr.run(t, "sage", "--crew", "c2"); code != exitFailure ||
		errOut != "crew: resolve-target: no worker matches 'sage'\n" {
		t.Errorf("--crew c2: code %d, err %q", code, errOut)
	}
	if out, _, code := tr.run(t, "--crew", "c2", "nova"); code != 0 || out != "feat/y\tnova\th2\tc2\n" {
		t.Errorf("--crew c2 nova: code %d, out %q", code, out)
	}
}

func TestRefusals(t *testing.T) {
	tr := setup(t)
	tr.write(t, dispatch(1, "c1", "feat/x", "sage", "h1"))
	cases := []struct {
		args []string
		want string
	}{
		{nil, "usage: crew resolve-target <target> [--crew ID]\n"},
		{[]string{"--crew"}, "crew: resolve-target: --crew needs an id\n"},
		{[]string{"--crew", ""}, "crew: resolve-target: --crew needs an id\n"},
		{[]string{"--bogus", "sage"}, "crew: resolve-target: unknown flag '--bogus'\n"},
		{[]string{"sage", "again"}, "crew: resolve-target: one target only\n"},
		{[]string{"nobody"}, "crew: resolve-target: no worker matches 'nobody'\n"},
	}
	for _, c := range cases {
		out, errOut, code := tr.run(t, c.args...)
		if code != exitFailure || out != "" || errOut != c.want {
			t.Errorf("%v: code %d, out %q, err %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
}

// The arm ran jq under `2>/dev/null || true`, so a bus it cannot use is "nothing
// matches": exit 1 and its own line, never jq's exit 2 or 5. A torn line is
// skipped and its neighbours still resolve.
func TestBusFailuresAreNoMatch(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		dispatch(1, "c1", "feat/x", "sage", "h1"),
		`{"ts":2,"crew_id":"c1","kind":"dispatch","broken`,
		dispatch(3, "c1", "feat/y", "nova", "h2"),
	)
	if out, _, code := tr.run(t, "sage"); code != 0 || out != "feat/x\tsage\th1\tc1\n" {
		t.Errorf("torn line: code %d, out %q", code, out)
	}
	if out, _, code := tr.run(t, "nova"); code != 0 || out != "feat/y\tnova\th2\tc1\n" {
		t.Errorf("neighbour row: code %d, out %q", code, out)
	}

	tr = setup(t)
	if _, errOut, code := tr.run(t, "sage"); code != exitFailure ||
		errOut != "crew: resolve-target: no worker matches 'sage'\n" {
		t.Errorf("no log at all: code %d, err %q", code, errOut)
	}

	tr = setup(t)
	if err := os.MkdirAll(tr.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr.paths.Log, []byte("not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := tr.run(t, "sage"); code != exitFailure ||
		errOut != "crew: resolve-target: no worker matches 'sage'\n" {
		t.Errorf("wholly corrupt log: code %d, err %q", code, errOut)
	}

	// A directory where the log is: `[ -f ]` misses, so no rows and no error.
	tr = setup(t)
	if err := os.MkdirAll(tr.paths.Log, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, code := tr.run(t, "sage"); code != exitFailure {
		t.Errorf("log is a directory: code %d, want 1", code)
	}
}

// Rows is the arm's read: a line that is not exactly one JSON value is skipped,
// including a blank line and a line of two values.
func TestRowsSkipsWhatItCannotDecode(t *testing.T) {
	tr := setup(t)
	if err := os.MkdirAll(tr.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := dispatch(1, "c1", "feat/x", "sage", "h1") + "\n\n" +
		`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/two","name":"two","host":"h"}{"ts":3}` + "\n"
	if err := os.WriteFile(tr.paths.Log, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := Rows(tr.paths.Log)
	if len(rows) != 1 {
		t.Fatalf("rows %d, want 1 (the blank line and the two-value line are skipped)", len(rows))
	}
}

// A row whose branch is not a string is no row at all, and a dispatch row with
// no name still matches by branch with an empty codename column.
func TestRowShapes(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":404,"name":"sage"}`,
		`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/nb"}`,
	)
	if _, _, code := tr.run(t, "sage"); code != exitFailure {
		t.Errorf("non-string branch: code %d, want 1", code)
	}
	if out, _, code := tr.run(t, "feat/nb"); code != 0 || out != "feat/nb\t\t\tc1\n" {
		t.Errorf("name-less row: code %d, out %q", code, out)
	}
}
