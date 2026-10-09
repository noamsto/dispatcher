package reply

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

// now is the fixed clock every case runs on: 2023-11-14T22:13:20.5Z, so the
// row's `ts` is the deterministic 1700000000500.
var now = time.Unix(1700000000, 500_000_000).UTC()

const (
	tsText = `1700000000500`
	// elidedMarker is bus's `_ELIDED`, spelled out so this fails if the marker
	// the port copies ever changes.
	elidedMarker = " …[elided]"
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

// write seeds the bus; no rows means no log file at all, which is the arm's
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

// rawLog is the log as bytes, before any parse: the appended row's own text.
func (tr *tree) rawLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(tr.paths.Log)
	if err != nil {
		t.Fatalf("no log written: %v", err)
	}
	return string(data)
}

// rows are the bus lines, newest last.
func (tr *tree) rows(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(tr.rawLog(t), "\n"), "\n")
	return lines
}

// crewDir registers a crew's dispatcher pid, the file the cross-crew lookup
// probes. An empty pid still creates the (empty) file.
func (tr *tree) crewDir(t *testing.T, id, pid string) {
	t.Helper()
	dir := tr.paths.CrewDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/pid", []byte(pid), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run is one arm invocation. The crew defaults to c1 so the with-crew path reads
// like the arm's `CREW_ID=c1 crew reply …`; a case wanting the cross-crew path
// passes its own Options.
func (tr *tree) run(t *testing.T, o Options, args ...string) (string, int) {
	t.Helper()
	if o.CrewID == nil {
		o.CrewID = func() string { return "c1" }
	}
	if o.Clock.Now == nil {
		o.Clock.Now = func() time.Time { return now }
	}
	if o.Probes.Alive == nil {
		o.Probes = liveProbes(nil)
	}
	var errB bytes.Buffer
	code := Run(args, tr.paths, &errB, o)
	return errB.String(), code
}

// liveProbes is the arm's `kill -0` over a live set. Elapsed/Mtime stay
// unreadable, so _pid_recycled fails closed to not-recycled and liveness is
// exactly the set — the same reading crews_test drives.
func liveProbes(live map[int]bool) crews.Probes {
	return crews.Probes{
		Alive:   func(pid int) bool { return live[pid] },
		Elapsed: func(int) (int64, bool) { return 0, false },
		Mtime:   func(string) (int64, bool) { return 0, false },
		Parent:  func(int) (int, bool) { return 0, false },
	}
}

func status(ts int64, crew, from, state string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"%s","kind":"status","from":%q,"body":{"state":%q}}`, ts, crew, from, state)
}

func dispatch(ts int64, crew, branch, session string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"%s","kind":"dispatch","branch":%q,"session":%q,"body":{}}`, ts, crew, branch, session)
}

// row is the appended line with everything but the body spelled out.
func row(crew, to, body string) string {
	return fmt.Sprintf(`{"ts":%s,"crew_id":%q,"from":"dispatcher:%s","to":%q,"kind":"msg","body":%q}`, tsText, crew, crew, to, body)
}

// wantRow asserts the last bus line is value-equal to want (key order is
// engine-internal for a row whose body is plain text, #821).
func (tr *tree) wantRow(t *testing.T, want string) {
	t.Helper()
	rows := tr.rows(t)
	got := testjson.MustParse(t, rows[len(rows)-1])
	if gotJSON := testjson.Compact(got); gotJSON != testjson.Compact(testjson.MustParse(t, want)) {
		t.Fatalf("row\n got %s\nwant %s", gotJSON, testjson.Compact(testjson.MustParse(t, want)))
	}
}

func TestExplicitSessionIDAppendsArmOrderRow(t *testing.T) {
	tr := setup(t)
	tr.write(t, status(1785951000000, "c1", "worker:feat/x#s1-1", "done"))
	if _, code := tr.run(t, Options{}, "worker:feat/x#s1-1", "go"); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	// Byte-exact once, to pin the construction order the arm's jq -c wrote, and
	// that `ts` is jq's `now*1000|floor` rather than the virtual clock's seconds.
	if got, want := strings.TrimSpace(tr.rows(t)[1]), row("c1", "worker:feat/x#s1-1", "go"); got != want {
		t.Fatalf("row\n got %s\nwant %s", got, want)
	}
}

func TestBranchResolvesToNewestSession(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		status(1785951000000, "c1", "worker:feat/x#s1-1", "done"),
		status(1785951100000, "c1", "worker:feat/x#s2-2", "working"),
	)
	if _, code := tr.run(t, Options{}, "worker:feat/x", "go"); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	tr.wantRow(t, row("c1", "worker:feat/x#s2-2", "go"))
}

// A session-less watchdog row after the live session joins it (#173); after a
// terminal one it stays a null session and the address is refused.
func TestSessionlessRowJoinsTheSessionBeforeIt(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		status(1785951000000, "c1", "worker:feat/x#s1-1", "working"),
		status(1785951100000, "c1", "worker:feat/x", "blocked"),
	)
	if _, code := tr.run(t, Options{}, "worker:feat/x", "ship it"); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	tr.wantRow(t, row("c1", "worker:feat/x#s1-1", "ship it"))
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name, branch, line string
		rows               []string
	}{
		{
			name:   "newest terminal",
			branch: "feat/x",
			rows:   []string{status(1785951000000, "c1", "worker:feat/x#s1-1", "done")},
			line:   "crew: newest session on feat/x is done — a stopped session never reads its inbox; re-dispatch with the context baked in",
		},
		{
			name:   "no session on branch",
			branch: "feat/nope",
			rows:   []string{status(1785951000000, "c1", "worker:feat/y#s1-1", "working")},
			line:   "crew: no session on feat/nope — dispatch a worker before replying to one",
		},
		{
			name:   "null session id",
			branch: "feat/x",
			rows:   []string{status(1785951000000, "c1", "worker:feat/x", "working")},
			line:   "crew: feat/x has no session id on the bus — a branch-only address can never reach a live worker's inbox; re-dispatch",
		},
		{
			// The heartbeat joins no session (its predecessor is terminal), so the
			// newest entry is the null-session one rather than the finished one.
			name:   "session-less heartbeat after a finished session",
			branch: "feat/x",
			rows: []string{
				status(1785951000000, "c1", "worker:feat/x#s1-1", "done"),
				status(1785951100000, "c1", "worker:feat/x", "working"),
			},
			line: "crew: feat/x has no session id on the bus — a branch-only address can never reach a live worker's inbox; re-dispatch",
		},
		{
			// A watchdog `failed` while the session is still live joins it, and the
			// joined state is what the terminal check reads.
			name:   "session-less failed after a live session",
			branch: "feat/x",
			rows: []string{
				status(1785951000000, "c1", "worker:feat/x#s1-1", "working"),
				status(1785951100000, "c1", "worker:feat/x", "failed"),
			},
			line: "crew: newest session on feat/x is failed — a stopped session never reads its inbox; re-dispatch with the context baked in",
		},
		{
			// `.state` is read straight off the bus, and `jq -r` prints a non-string
			// value rather than bare: a hand-written row carrying an array prints
			// the array, in jq's pretty form, not a misleading `null`.
			name:   "state is not a string",
			branch: "feat/x",
			rows: []string{
				`{"ts":1785951000000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":["done"]}}`,
			},
			line: "crew: newest session on feat/x is [\n  \"done\"\n] — a stopped session never reads its inbox; re-dispatch with the context baked in",
		},
		{
			name:   "no log at all",
			branch: "feat/x",
			rows:   nil,
			line:   "crew: no session on feat/x — dispatch a worker before replying to one",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setup(t)
			fx.write(t, c.rows...)
			errs, code := fx.run(t, Options{}, "worker:"+c.branch, "go")
			if code != 1 {
				t.Fatalf("code %d, want 1 (%s)", code, errs)
			}
			if errs != c.line+"\n" {
				t.Fatalf("stderr\n got %q\nwant %q", errs, c.line+"\n")
			}
			if c.rows != nil {
				if got := len(fx.rows(t)); got != len(c.rows) {
					t.Fatalf("%d rows, want the bus untouched at %d", got, len(c.rows))
				}
			}
		})
	}
}

// A branch whose only row is a dispatch has a session id but no state: the arm
// resolves it (state null, terminal false) and addresses it.
func TestDispatchOnlySessionResolves(t *testing.T) {
	tr := setup(t)
	tr.write(t, dispatch(1785951000000, "c1", "feat/x", "s4-4"))
	if _, code := tr.run(t, Options{}, "worker:feat/x", "go"); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	tr.wantRow(t, row("c1", "worker:feat/x#s4-4", "go"))
}

func TestCorruptLogIsJqFailure(t *testing.T) {
	tr := setup(t)
	tr.write(t, `{"ts":1785951000000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}`)
	errs, code := tr.run(t, Options{}, "worker:feat/x", "go")
	if code != 5 {
		t.Fatalf("code %d, want 5 (%s)", code, errs)
	}
	if !strings.HasPrefix(errs, "crew: reply: "+tr.paths.Log+": ") || strings.Count(errs, "\n") != 1 {
		t.Fatalf("stderr %q, want one crew: reply: <log>: line", errs)
	}
}

// A log that exists but will not open is jq's exit 2, not a missing one's.
func TestUnreadableLogIsExitTwo(t *testing.T) {
	tr := setup(t)
	tr.write(t, status(1785951000000, "c1", "worker:feat/x#s1-1", "working"))
	if err := os.Chmod(tr.paths.Log, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tr.paths.Log, 0o644) })
	errs, code := tr.run(t, Options{}, "worker:feat/x", "go")
	if code != 2 {
		t.Fatalf("code %d, want 2 (%s)", code, errs)
	}
	if !strings.HasPrefix(errs, "crew: reply: "+tr.paths.Log+": ") {
		t.Fatalf("stderr %q, want one crew: reply: <log>: line", errs)
	}
}

func TestCrossCrewLookup(t *testing.T) {
	// A registered pid that is dead is a crashed crew; no pid file at all is
	// unknown, not dead, so it never narrows the lookup (#327).
	cases := []struct {
		name   string
		rows   []string
		crews  map[string]string
		live   map[int]bool
		want   string
		refuse string
	}{
		{
			name:  "single live crew resolves",
			rows:  []string{status(1785951000000, "c-a", "worker:feat/x#s1-1", "working")},
			crews: map[string]string{"c-a": "4321"},
			live:  map[int]bool{4321: true},
			want:  row("c-a", "worker:feat/x#s1-1", "go"),
		},
		{
			name:  "no pid file is unknown, not dead",
			rows:  []string{status(1785951000000, "c-none", "worker:feat/x#s1-1", "working")},
			crews: map[string]string{"c-none": ""},
			want:  row("c-none", "worker:feat/x#s1-1", "go"),
		},
		{
			name: "dead crew dropped beside a live one",
			rows: []string{
				status(1785951000000, "c-a", "worker:feat/x#s1-1", "working"),
				status(1785951100000, "c-b", "worker:feat/x#s2-2", "working"),
			},
			crews: map[string]string{"c-a": "4321", "c-b": "4322"},
			live:  map[int]bool{4322: true},
			want:  row("c-b", "worker:feat/x#s2-2", "go"),
		},
		{
			// One candidate is never dropped for being dead: with nothing to
			// prefer it still delivers, exactly as before #327.
			name:  "the only crew, dead, still delivers",
			rows:  []string{status(1785951000000, "c-a", "worker:feat/x#s1-1", "working")},
			crews: map[string]string{"c-a": "0"},
			want:  row("c-a", "worker:feat/x#s1-1", "go"),
		},
		{
			name: "every crew dead refuses naming both",
			rows: []string{
				status(1785951000000, "c-a", "worker:feat/x#s1-1", "working"),
				status(1785951100000, "c-b", "worker:feat/x#s2-2", "working"),
			},
			crews:  map[string]string{"c-a": "4321", "c-b": "4322"},
			refuse: "has live sessions in crews: c-a, c-b",
		},
		{
			name:   "no crew has a live session",
			rows:   []string{status(1785951000000, "c-a", "worker:feat/x#s1-1", "failed")},
			crews:  map[string]string{"c-a": "4321"},
			live:   map[int]bool{4321: true},
			refuse: "no live session on feat/x in any crew",
		},
		{
			name:   "no crew has a session at all",
			rows:   []string{status(1785951000000, "c-a", "worker:feat/y#s1-1", "working")},
			crews:  map[string]string{"c-a": "4321"},
			live:   map[int]bool{4321: true},
			refuse: "no live session on feat/x in any crew",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setup(t)
			fx.write(t, c.rows...)
			for id, pid := range c.crews {
				fx.crewDir(t, id, pid)
			}
			errs, code := fx.run(t, Options{CrewID: func() string { return "" }, Probes: liveProbes(c.live)}, "worker:feat/x", "go")
			if c.refuse != "" {
				if code != 1 || !strings.Contains(errs, c.refuse) {
					t.Fatalf("code %d stderr %q, want 1 naming %q", code, errs, c.refuse)
				}
				if got := len(fx.rows(t)); got != len(c.rows) {
					t.Fatalf("%d rows, want the bus untouched at %d", got, len(c.rows))
				}
				return
			}
			if code != 0 {
				t.Fatalf("code %d, want 0 (%s)", code, errs)
			}
			fx.wantRow(t, c.want)
		})
	}
}

func TestTwoLiveCrewsRefuseNamingBoth(t *testing.T) {
	tr := setup(t)
	tr.crewDir(t, "c-a", "4321")
	tr.crewDir(t, "c-b", "4322")
	tr.write(t,
		status(1785951000000, "c-a", "worker:feat/x#s1-1", "working"),
		status(1785951100000, "c-b", "worker:feat/x#s2-2", "blocked"),
	)
	errs, code := tr.run(t, Options{CrewID: func() string { return "" }, Probes: liveProbes(map[int]bool{4321: true, 4322: true})}, "worker:feat/x", "go")
	if code != 1 {
		t.Fatalf("code %d, want 1 (%s)", code, errs)
	}
	if want := "crew: CREW_ID not set and feat/x has live sessions in crews: c-a, c-b — pass --crew <id>\n"; errs != want {
		t.Fatalf("stderr\n got %q\nwant %q", errs, want)
	}
	if got := len(tr.rows(t)); got != 2 {
		t.Fatalf("%d rows, want the bus untouched at 2", got)
	}
}

func TestNoLiveSessionInAnyCrew(t *testing.T) {
	tr := setup(t)
	tr.crewDir(t, "c-a", "4321")
	tr.write(t, status(1785951000000, "c-a", "worker:feat/x#s1-1", "failed"))
	errs, code := tr.run(t, Options{CrewID: func() string { return "" }, Probes: liveProbes(map[int]bool{4321: true})}, "worker:feat/x", "go")
	if code != 1 {
		t.Fatalf("code %d, want 1 (%s)", code, errs)
	}
	want := "crew: CREW_ID not set and no live session on feat/x in any crew — pass --crew <id> (or dispatch a worker before replying to one)\n"
	if errs != want {
		t.Fatalf("stderr\n got %q\nwant %q", errs, want)
	}
}

// With no crew anywhere and a target that needs no resolution, the append still
// needs a crew to post as — and refuses without one.
func TestNoCrewAtAll(t *testing.T) {
	tr := setup(t)
	errs, code := tr.run(t, Options{CrewID: func() string { return "" }}, "role:feat/x:reviewer", "go")
	if code != 1 {
		t.Fatalf("code %d, want 1 (%s)", code, errs)
	}
	if want := "crew: CREW_ID not set and no WORKER_TASK.md crew_id — pass --crew <id>\n"; errs != want {
		t.Fatalf("stderr\n got %q\nwant %q", errs, want)
	}
}

func TestFlagsAndPassthroughTargets(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		crewID string
		want   string
		err    string
	}{
		{name: "--crew needs a value", args: []string{"role:a", "go", "--crew"}, err: "crew: --crew needs a value"},
		{name: "--crew refuses an empty value", args: []string{"role:a", "go", "--crew", ""}, err: "crew: --crew needs a value"},
		{name: "--crew after the positionals", args: []string{"role:a", "go", "--crew", "c2"}, want: row("c2", "role:a", "go")},
		{name: "the last --crew wins", args: []string{"--crew", "c2", "--crew", "c3", "role:a", "go"}, want: row("c3", "role:a", "go")},
		{name: "a non-worker target is untouched", args: []string{"metrics:c1", "{}"}, want: row("c1", "metrics:c1", "{}")},
		{name: "a branch with a # is branch-only", args: []string{"worker:feat/12-a#b", "go"}, want: row("c1", "worker:feat/12-a#b#s9-9", "go")},
		{name: "a missing body is the empty string", args: []string{"role:a"}, want: row("c1", "role:a", "")},
		{name: "no arguments at all", args: nil, want: row("c1", "", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := setup(t)
			fx.write(t,
				status(1785951000000, "c1", "worker:feat/12-a#b#s9-9", "working"),
				status(1785951100000, "c1", "worker:feat/x#s1-1", "working"),
			)
			before := len(fx.rows(t))
			errs, code := fx.run(t, Options{}, c.args...)
			if c.err != "" {
				if code != 1 || errs != c.err+"\n" {
					t.Fatalf("code %d stderr %q, want 1 %q", code, errs, c.err)
				}
				if len(fx.rows(t)) != before {
					t.Fatal("the refusal wrote a row")
				}
				return
			}
			if code != 0 {
				t.Fatalf("code %d, want 0 (%s)", code, errs)
			}
			fx.wantRow(t, c.want)
		})
	}
}

// A body past the cap is elided the way _fit_line does: the encoded line fits
// LineMax and the body ends with the marker.
func TestOversizedBodyIsElided(t *testing.T) {
	tr := setup(t)
	body := strings.Repeat("x", 6000)
	if _, code := tr.run(t, Options{}, "role:a", body); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(tr.rawLog(t), "\n"), "\n")
	line := lines[len(lines)-1]
	if len(line) > bus.LineMax {
		t.Fatalf("line is %d bytes, over _LINE_MAX", len(line))
	}
	if !strings.HasSuffix(line, elidedMarker+`"}`) {
		t.Fatalf("line %q does not end with the elided marker", line[len(line)-40:])
	}
}

// The append is the point of the command, so a bus dir that does not exist yet
// is created rather than failing the write.
func TestCreatesBusDir(t *testing.T) {
	tr := setup(t)
	if _, code := tr.run(t, Options{}, "role:a", "go"); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	if _, err := os.Stat(tr.paths.Log); err != nil {
		t.Fatalf("no row written: %v", err)
	}
}
