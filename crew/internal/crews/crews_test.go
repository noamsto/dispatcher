package crews

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

// fixture is a bus without git: crews reads bus.Paths, so a temp dir stands
// in for the common dir.
func fixture(t *testing.T) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	return bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
}

func crewDir(t *testing.T, p bus.Paths, id, pidFile string) {
	t.Helper()
	dir := p.Dir + "/crews/" + id
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if pidFile != "" {
		if err := os.WriteFile(dir+"/pid", []byte(pidFile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeLog(t *testing.T, p bus.Paths, body string) {
	t.Helper()
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Log, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeNow makes the arm's `now*1000` 1785951264000.
var fakeNow = func() time.Time { return time.Unix(1785951264, 0) }

func status(ts int64, crew, from string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"%s","kind":"status","from":"%s","body":{"state":"working"}}`, ts, crew, from)
}

func run(t *testing.T, p bus.Paths, o Options) (string, string, int) {
	t.Helper()
	return runWith(t, false, p, o)
}

func runMine(t *testing.T, p bus.Paths, o Options) (string, string, int) {
	t.Helper()
	return runWith(t, true, p, o)
}

func runWith(t *testing.T, mine bool, p bus.Paths, o Options) (string, string, int) {
	t.Helper()
	var out, errB bytes.Buffer
	code := Run(t.Context(), mine, false, p, &out, &errB, o)
	return out.String(), errB.String(), code
}

func opts(live map[int]bool, parents map[int]int) Options {
	return Options{Proc: proctest.New(live, parents), Clock: clock.Clock{Now: fakeNow}}
}

func TestTableUnionSortAndAliveMatrix(t *testing.T) {
	p := fixture(t)
	crewDir(t, p, "c-b", "4321\n")
	crewDir(t, p, "c-a", "")
	crewDir(t, p, "c-zero", "0\n")
	crewDir(t, p, "c-junk", "not-a-pid\n")
	crewDir(t, p, "c-dead", "8888\n")
	writeLog(t, p, strings.Join([]string{
		status(1785951000000, "c-b", "worker:feat/x#s1"),
		status(1785950000000, "c-b", "worker:feat/y#s2"),
		`{"ts":1785950900000,"crew_id":"c-b","kind":"msg","from":"worker:feat/x#s1","body":{}}`,
		status(1785950500000, "c-log", "dispatcher:1"),
		status(1785950600000, "c-log", "worker:feat/z#s9"),
		`{"ts":1785950700000,"kind":"reap"}`,
	}, "\n")+"\n")

	out, errB, code := run(t, p, opts(map[int]bool{4321: true}, nil))
	if code != 0 || errB != "" {
		t.Fatalf("code %d, stderr %q", code, errB)
	}
	want := strings.Join([]string{
		"crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive",
		"c-b\t264\t1264\t2\t4321\tyes",
		"c-log\t664\t764\t1\t—\t—",
		"c-a\t—\t—\t0\t—\t—",
		"c-dead\t—\t—\t0\t8888\tno",
		"c-junk\t—\t—\t0\tnot-a-pid\tno",
		"c-zero\t—\t—\t0\t0\tno",
	}, "\n") + "\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestTableTornTailKeepsPrefixIDsWithoutStats(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951000000, "c-intact", "worker:feat/x#s1")+"\n"+`{"ts":1785951264000,"crew_id":"c-to`)

	out, _, code := run(t, p, opts(nil, nil))
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	// The torn id never lands, the intact one keeps its row, and the stats
	// column is gone (bash: jq -s failed whole, `|| echo '{}'`).
	want := "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n" +
		"c-intact\t—\t—\t0\t—\t—\n"
	if out != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestTableCorruptLogContributesNoIDs(t *testing.T) {
	p := fixture(t)
	crewDir(t, p, "c-dir", "")
	writeLog(t, p, "not json at all\n")
	out, _, code := run(t, p, opts(nil, nil))
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); len(lines) != 2 {
		t.Errorf("want header + c-dir only, got %q", out)
	}
	if !strings.HasSuffix(out, "c-dir\t—\t—\t0\t—\t—\n") {
		t.Errorf("c-dir row wrong: %q", out)
	}
}

func TestIDsAreDirsAndLogIDsSortedAndDeduped(t *testing.T) {
	p := fixture(t)
	crewDir(t, p, "c-dir", "")
	crewDir(t, p, "c-both", "")
	writeLog(t, p, status(1785951000000, "c-log", "worker:feat/x#s1")+"\n"+
		status(1785951001000, "c-both", "worker:feat/y#s1")+"\n"+
		status(1785951002000, "c-log", "worker:feat/x#s1")+"\n"+
		`{"ts":1785951003000,"kind":"status"}`+"\n")

	want := []string{"c-both", "c-dir", "c-log"}
	if got := IDs(p); !slices.Equal(got, want) {
		t.Errorf("IDs = %q, want %q", got, want)
	}
}

func TestIDsKeepTheWellFormedPrefixOfACorruptLog(t *testing.T) {
	p := fixture(t)
	crewDir(t, p, "c-dir", "")
	writeLog(t, p, status(1785951000000, "c-intact", "worker:feat/x#s1")+"\n"+`{"ts":1785951264000,"crew_id":"c-to`)

	want := []string{"c-dir", "c-intact"}
	if got := IDs(p); !slices.Equal(got, want) {
		t.Errorf("IDs = %q, want %q", got, want)
	}
}

func TestIDsOfAMissingLogAreTheDirsOnly(t *testing.T) {
	p := fixture(t)
	crewDir(t, p, "c-b", "")
	crewDir(t, p, "c-a", "")

	want := []string{"c-a", "c-b"}
	if got := IDs(p); !slices.Equal(got, want) {
		t.Errorf("IDs = %q, want %q", got, want)
	}
}

func TestTableEmptyBusAndNoCrewsDir(t *testing.T) {
	p := fixture(t)
	out, errB, code := run(t, p, opts(nil, nil))
	if code != 0 || errB != "" {
		t.Fatalf("code %d stderr %q", code, errB)
	}
	if out != "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n" {
		t.Errorf("got %q", out)
	}
}

func TestTableRecycledPidIsNotAlive(t *testing.T) {
	p := fixture(t)
	crewDir(t, p, "c-rec", "7777\n")
	pidfile := p.Dir + "/crews/c-rec/pid"
	old := time.Unix(1785951264-100, 0) // the file is 100s old
	if err := os.Chtimes(pidfile, old, old); err != nil {
		t.Fatal(err)
	}
	// The process started 10s ago, after the file was written.
	o := Options{Clock: clock.Clock{Now: fakeNow}, Proc: &proctest.Table{Procs: map[int]proctest.Proc{7777: {Elapsed: 10}}}}
	out, _, code := run(t, p, o)
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out, "c-rec\t—\t—\t0\t7777\tno\n") {
		t.Errorf("recycled pid must read no: %q", out)
	}
}

func TestMineListsOnlyLiveAncestors(t *testing.T) {
	p := fixture(t)
	const ancestor, stranger = 4242, 9999
	crewDir(t, p, "c-anc", fmt.Sprintf("%d\n", ancestor))
	crewDir(t, p, "c-str", fmt.Sprintf("%d\n", stranger))
	crewDir(t, p, "c-dead", "8888\n")
	crewDir(t, p, "c-nopid", "")
	writeLog(t, p, status(1785951000000, "c-log", "worker:feat/x#s1"))

	live := map[int]bool{ancestor: true, stranger: true}
	// The walk: os.Getpid() → 300 → ancestor. stranger is live but never on
	// the chain, 8888 is dead, c-nopid has no pid file, c-log only log traffic.
	parents := map[int]int{os.Getpid(): 300, 300: ancestor}
	out, _, code := runMine(t, p, opts(live, parents))
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if out != "c-anc\n" {
		t.Errorf("got %q want c-anc only", out)
	}
}

func TestMineNoCrewsDir(t *testing.T) {
	p := fixture(t)
	out, errB, code := runMine(t, p, opts(nil, nil))
	if code != 0 || out != "" || errB != "" {
		t.Fatalf("code %d out %q stderr %q", code, out, errB)
	}
}

func TestValidPidMatrix(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"123", 123, true},
		{"00", 0, true}, // kill -0 00 signals our own group in bash too
		{"0", 0, false},
		{"", 0, false},
		{"12 34", 0, false},
		{"12\n34", 0, false},
		{"-1", 0, false},
		{"4194304", 4194304, true},
		{"4194305", 0, false},
		{"4294967295", 0, false}, // kill(2) truncates to pid_t -1
		{"9999999999", 0, false},
		{"99999999999999999999999", 0, false}, // bash: kill and ps both fail
	} {
		got, ok := validPid(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("validPid(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// A stats row whose only ts is missing makes the final pass subtract null:
// both jq engines reject that, but the arm's jq -r had streamed the earlier
// rows while gojq yields nothing until its single value completes. Go's
// outcome — header only, one stderr line, exit 5 — is the documented
// divergence, pinned here.
func TestTableFinalPassFailureIsExit5WithNoRows(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951200000, "a", "worker:feat/x#s1")+"\n"+
		`{"crew_id":"b","kind":"status","from":"worker:feat/x#s1"}`+"\n")
	out, errB, code := run(t, p, opts(nil, nil))
	if code != 5 {
		t.Fatalf("code %d, want 5", code)
	}
	if out != "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n" {
		t.Errorf("stdout %q, want header only", out)
	}
	if !strings.HasPrefix(errB, "crew: crews: ") {
		t.Errorf("stderr %q", errB)
	}
}

// A non-string crew_id contributes no id: the arm's jq -r printed numbers
// and JSON fragments as id lines; Go drops them (documented divergence).
func TestTableNonStringCrewIDsDropped(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1785951200000,"crew_id":5,"kind":"status"}`+"\n"+
		`{"ts":1785951200000,"crew_id":{"o":1},"kind":"status"}`+"\n")
	out, _, code := run(t, p, opts(nil, nil))
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if out != "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n" {
		t.Errorf("stdout %q, want header only", out)
	}
}

func TestPidAliveRejectsOverRangePid(t *testing.T) {
	for _, in := range []string{"4294967295", "9999999999"} {
		got := PidAlive(&proctest.Table{}, time.Now(), "", in)
		if got == nil || *got {
			t.Errorf("PidAlive(%q) = %v; want false", in, got)
		}
	}
}

func TestFirstRunes(t *testing.T) {
	if got := firstRunes("héllo wörld", 5); got != "héllo" {
		t.Errorf("firstRunes = %q", got)
	}
	if got := firstRunes("héllo", 10); got != "héllo" {
		t.Errorf("firstRunes = %q", got)
	}
}

func gitRepo(t *testing.T) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	p, err := bus.Locate(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func homeOnly(t *testing.T, p bus.Paths, tbl proc.Table) []string {
	t.Helper()
	var out, errB bytes.Buffer
	if code := Run(t.Context(), false, true, p, &out, &errB, Options{Proc: tbl, Clock: clock.Clock{Now: fakeNow}}); code != 0 {
		t.Fatalf("code %d: %s", code, errB.String())
	}
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
		ids = append(ids, strings.SplitN(l, "\t", 2)[0])
	}
	return ids
}

func writeHome(t *testing.T, p bus.Paths, id, common string) {
	t.Helper()
	if err := os.MkdirAll(p.Dir+"/homes", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HomePath(p, id), []byte(common+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHomeFilter(t *testing.T) {
	here, other := gitRepo(t), gitRepo(t)
	pidLog := func(t *testing.T, action, outcome, id string) {
		t.Helper()
		PidfileLog(here, Caller{Now: fakeNow(), PID: 1, PPID: 1, Pwd: "/x", By: "b"}, action, outcome, id, "", "9")
	}
	tbl := func(pid int, cwd string) *proctestTable {
		return newProcs(map[int]string{pid: cwd})
	}
	cases := []struct {
		name  string
		setup func(t *testing.T)
		procs *proctestTable
		want  []string
	}{
		{"matching home kept after deregister", func(t *testing.T) { writeHome(t, here, "c-1", here.Common) }, nil, []string{"c-1"}},
		{"foreign home with no legacy evidence dropped", func(t *testing.T) { writeHome(t, here, "c-1", other.Common) }, nil, nil},
		{"mismatching home record falls through to a register ok log line", func(t *testing.T) {
			writeHome(t, here, "c-1", other.Common)
			pidLog(t, "register", "ok", "c-1")
		}, nil, []string{"c-1"}},
		{"foreign live pid file with no record dropped", func(t *testing.T) { crewDir(t, here, "c-1", "700\n") },
			tbl(700, filepath.Dir(other.Common)), nil},
		{"legacy register ok log line kept", func(t *testing.T) { pidLog(t, "register", "ok", "c-1") }, nil, []string{"c-1"}},
		{"legacy adopt ok log line kept", func(t *testing.T) { pidLog(t, "adopt", "ok", "c-1") }, nil, []string{"c-1"}},
		{"legacy adopt forced log line kept", func(t *testing.T) { pidLog(t, "adopt", "forced", "c-1") }, nil, []string{"c-1"}},
		{"legacy reattach log line is no evidence", func(t *testing.T) {
			crewDir(t, here, "c-1", "")
			pidLog(t, "reattach", "ok", "c-1")
		}, nil, nil},
		{"legacy live pid in this repo kept", func(t *testing.T) { crewDir(t, here, "c-1", "700\n") },
			tbl(700, filepath.Dir(here.Common)), []string{"c-1"}},
		{"legacy dead pid with no log dropped", func(t *testing.T) { crewDir(t, here, "c-1", "700\n") }, nil, nil},
		{"legacy no evidence dropped", func(t *testing.T) { crewDir(t, here, "c-1", "") }, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.RemoveAll(here.Dir); err != nil {
				t.Fatal(err)
			}
			writeLog(t, here, status(1785951000, "c-1", "worker:feat/x#s1")+"\n")
			tc.setup(t)
			var tb proc.Table = newProcs(nil)
			if tc.procs != nil {
				tb = tc.procs
			}
			if got := homeOnly(t, here, tb); !slices.Equal(got, tc.want) {
				t.Errorf("home crews = %v, want %v", got, tc.want)
			}
		})
	}
}

type proctestTable = proctest.Table

func newProcs(cwds map[int]string) *proctest.Table {
	t := proctest.New(nil, nil)
	for pid, dir := range cwds {
		t.Procs[pid] = proctest.Proc{Cwd: dir}
	}
	return t
}
