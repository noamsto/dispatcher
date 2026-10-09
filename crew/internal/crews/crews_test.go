package crews

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
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

func fakeProbes(live map[int]bool, parents map[int]int) Probes {
	return Probes{
		// Elapsed/Mtime unreadable: _pid_recycled fails closed to
		// not-recycled, so liveness is exactly the live set.
		Elapsed: func(int) (int64, bool) { return 0, false },
		Mtime:   func(string) (int64, bool) { return 0, false },
		Alive:   func(pid int) bool { return live[pid] },
		Parent:  func(pid int) (int, bool) { n, ok := parents[pid]; return n, ok },
	}
}

func status(ts int64, crew, from string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"%s","kind":"status","from":"%s","body":{"state":"working"}}`, ts, crew, from)
}

func run(t *testing.T, p bus.Paths, o Options, args ...string) (string, string, int) {
	t.Helper()
	var out, errB bytes.Buffer
	code := Run(args, p, &out, &errB, o)
	return out.String(), errB.String(), code
}

func opts(live map[int]bool, parents map[int]int) Options {
	return Options{Probes: fakeProbes(live, parents), Now: fakeNow}
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
	o := Options{Now: fakeNow, Probes: Probes{
		Alive:   func(int) bool { return true },
		Elapsed: func(int) (int64, bool) { return 10, true },                  // started 10s ago
		Mtime:   func(string) (int64, bool) { return 1785951264 - 100, true }, // file is 100s old
		Parent:  func(int) (int, bool) { return 0, false },
	}}
	out, _, code := run(t, p, o)
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out, "c-rec\t—\t—\t0\t7777\tno\n") {
		t.Errorf("recycled pid must read no: %q", out)
	}
}

func TestTableJQColorsWarnsOnce(t *testing.T) {
	p := fixture(t)
	crewDir(t, p, "c1", "")
	o := opts(nil, nil)
	o.JQColorsInvalid = true
	_, errB, _ := run(t, p, o)
	if errB != "Failed to set $JQ_COLORS\n" {
		t.Errorf("stderr %q", errB)
	}
	// Empty bus: the arm never started a jq, so no warning.
	empty := fixture(t)
	_, errB, _ = run(t, empty, o)
	if errB != "" {
		t.Errorf("empty bus warned: %q", errB)
	}
}

func TestUnknownArg(t *testing.T) {
	p := fixture(t)
	out, errB, code := run(t, p, opts(nil, nil), "--mine=yes")
	if code != 64 {
		t.Fatalf("code %d", code)
	}
	if errB != "crew: crews: unknown arg '--mine=yes'\n" || out != "" {
		t.Errorf("out %q stderr %q", out, errB)
	}
	// The arm's `case "${1:-}"` sends an empty first arg to the table.
	_, _, code = run(t, p, opts(nil, nil), "")
	if code != 0 {
		t.Fatalf("empty arg must take the table branch, code %d", code)
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
	out, _, code := run(t, p, opts(live, parents), "--mine")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if out != "c-anc\n" {
		t.Errorf("got %q want c-anc only", out)
	}
}

func TestMineNoCrewsDir(t *testing.T) {
	p := fixture(t)
	out, errB, code := run(t, p, opts(nil, nil), "--mine")
	if code != 0 || out != "" || errB != "" {
		t.Fatalf("code %d out %q stderr %q", code, out, errB)
	}
}

func TestMineExtraArgsIgnored(t *testing.T) {
	p := fixture(t)
	_, _, code := run(t, p, opts(nil, nil), "--mine", "junk")
	if code != 0 {
		t.Fatalf("code %d", code)
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
