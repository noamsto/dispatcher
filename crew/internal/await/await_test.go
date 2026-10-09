package await

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

// now is 1800000000, the second every fixture's virtual clock starts at, so a
// timeout's "ended after Ns" is the polls it ran and nothing else.
var now = time.Unix(1800000000, 0)

const (
	me   = "worker:feat/x#s1-1"
	disp = "dispatcher:c1"
	role = "role:feat/x:reviewer"
)

// fixture is a bus without git: Run reads bus.Paths, so a temp dir stands in for
// the common dir and the marks file lands under it as $dir/await does.
func fixture(t *testing.T) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	return bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
}

func writeLog(t *testing.T, p bus.Paths, rows ...string) {
	t.Helper()
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func msg(ts int64, from, to, body string) string {
	return `{"ts":` + strconv.FormatInt(ts, 10) + `,"crew_id":"c1","kind":"msg","from":"` + from +
		`","to":"` + to + `","body":"` + body + `"}`
}

// clockIn is the virtual clock, so no test sleeps and each poll advances the
// deadline math by exactly the interval it passed.
func clockIn(t *testing.T, seconds int64) clock.Clock {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clock")
	if err := os.WriteFile(path, []byte(strconv.FormatInt(seconds, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return clock.Clock{Now: func() time.Time { return now }, CrewClock: path}
}

func run(t *testing.T, p bus.Paths, args ...string) (string, string, int) {
	t.Helper()
	return runOpts(t, p, Options{}, args...)
}

func runOpts(t *testing.T, p bus.Paths, o Options, args ...string) (string, string, int) {
	t.Helper()
	if o.CrewID == nil {
		o.CrewID = func() string { return "c1" }
	}
	// A test that wants the real clock sets Now and leaves CrewClock empty.
	if o.Clock.Now == nil {
		o.Clock = clockIn(t, 1800000000)
	}
	var out, errB bytes.Buffer
	code := Run(args, p, &out, &errB, o)
	return out.String(), errB.String(), code
}

// want is the arm's `jq -c` output for these rows: value-identical, with keys
// sorted the way gojq hands objects back (the sanctioned key-order difference).
func want(t *testing.T, rows ...string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(testjson.Compact(testjson.MustParse(t, r)))
		b.WriteString("\n")
	}
	return b.String()
}

func readMarks(t *testing.T, p bus.Paths, who string) string {
	t.Helper()
	b, err := os.ReadFile(marks.Path(p.Dir, "c1", who))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func mark(t *testing.T, p bus.Paths, who, row string) {
	t.Helper()
	if !marks.Record(p.Dir, "c1", who, []jsonv.Value{testjson.MustParse(t, row)}) {
		t.Fatal("marks.Record failed")
	}
}

func TestDeliversDueReply(t *testing.T) {
	p := fixture(t)
	row := msg(1785951264000, disp, me, "answer")
	writeLog(t, p, row)

	stdout, stderr, code := run(t, p, me, "--timeout", "0")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if stdout != want(t, row) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, row))
	}
	if got := readMarks(t, p, me); got != `{"`+disp+`":1785951264000}` {
		t.Errorf("marks = %s", got)
	}
}

// The delivered mark is a ceiling: at it, not past it.
func TestAlreadyHandedReplyTimesOut(t *testing.T) {
	p := fixture(t)
	row := msg(1785951264000, disp, me, "answer")
	writeLog(t, p, row)
	mark(t, p, me, row)

	stdout, stderr, code := run(t, p, me, "--timeout", "0")
	if stdout != "" || code != 0 {
		t.Errorf("stdout %q code %d", stdout, code)
	}
	if want := "crew: await ended after 0s — no reply to " + me + " yet\n"; stderr != want {
		t.Errorf("stderr = %q", stderr)
	}
}

// The last due row in log order picks the sender, and that sender's whole due
// backlog prints oldest first — the mark raised for the batch cannot skip a
// sibling (#466).
func TestLastDueSenderWinsOldestFirst(t *testing.T) {
	p := fixture(t)
	a1 := msg(100, disp, me, "a1")
	b1 := msg(200, role, me, "b1")
	a2 := msg(300, disp, me, "a2")
	b2 := msg(400, role, me, "b2")
	a3 := msg(500, disp, me, "a3")
	writeLog(t, p, a1, b1, a2, b2, a3)

	stdout, _, code := run(t, p, me, "--timeout", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, a1, a2, a3) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, a1, a2, a3))
	}
	// The other sender's rows are neither printed nor marked delivered.
	if got := readMarks(t, p, me); got != `{"`+disp+`":500}` {
		t.Errorf("marks = %s", got)
	}
}

func TestFromRestrictsTheSender(t *testing.T) {
	p := fixture(t)
	a1 := msg(100, disp, me, "a1")
	b1 := msg(200, role, me, "b1")
	writeLog(t, p, a1, b1)

	stdout, _, code := run(t, p, me, "--from", role, "--timeout", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, b1) {
		t.Errorf("stdout = %q", stdout)
	}
	if got := readMarks(t, p, me); got != `{"`+role+`":200}` {
		t.Errorf("marks = %s", got)
	}
}

// A --from that never matches is a timeout that names the sender and still exits
// 0 with empty stdout.
func TestFromMismatchTimesOut(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msg(100, disp, me, "a1"))

	stdout, stderr, code := run(t, p, me, "--from", "dispatcher:c9", "--timeout", "0")
	if stdout != "" || code != 0 {
		t.Errorf("stdout %q code %d", stdout, code)
	}
	if !strings.HasSuffix(stderr, "no reply to "+me+" from dispatcher:c9 yet\n") {
		t.Errorf("stderr = %q", stderr)
	}
}

// Unlike `inbox`, `await` is addressed: a `"*"` broadcast is not its reply.
func TestBroadcastIsNotDelivered(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msg(100, role, "*", "all"))

	stdout, _, code := run(t, p, me, "--timeout", "0")
	if stdout != "" || code != 0 {
		t.Errorf("stdout %q code %d", stdout, code)
	}
	if readMarks(t, p, me) != "" {
		t.Error("marks raised for a row that was not delivered")
	}
}

// `-R` + `fromjson?` skips every unparseable line, not just a torn tail, so the
// rows around it still deliver. A `null` row is parseable and indexes to null,
// so it is simply not a candidate.
func TestUnparseableLinesAreSkipped(t *testing.T) {
	p := fixture(t)
	first := msg(100, disp, me, "first")
	last := msg(300, disp, me, "last")
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := first + "\n" + `{"ts":200,"crew_id":"c-to` + "\n" + `{"a":1} {"b":2}` + "\n\n" + "null\n" + last + "\n"
	if err := os.WriteFile(p.Log, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := run(t, p, me, "--timeout", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, first, last) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, first, last))
	}
}

// `sort_by(.ts)` is stable, so same-ms siblings keep their log order.
func TestEqualTsKeepsLogOrder(t *testing.T) {
	p := fixture(t)
	first := msg(100, disp, me, "first")
	second := msg(100, disp, me, "second")
	writeLog(t, p, first, second)

	stdout, _, code := run(t, p, me, "--timeout", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, first, second) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, first, second))
	}
}

// A row the fold cannot index is a jq type error, and the arm threw that away:
// nothing is due, nothing is marked, the poll runs to the deadline. (`null` is
// not one of these: jq indexes it to null, so it is simply not a candidate.)
func TestUnindexableRowStopsTheFold(t *testing.T) {
	for _, bad := range []string{"5", `"s"`, `[]`, `true`} {
		p := fixture(t)
		writeLog(t, p, msg(100, disp, me, "answer"), bad)

		stdout, stderr, code := run(t, p, me, "--timeout", "0")
		if stdout != "" || code != 0 {
			t.Errorf("row %s: stdout %q code %d", bad, stdout, code)
		}
		if stderr != "crew: await ended after 0s — no reply to "+me+" yet\n" {
			t.Errorf("row %s: stderr %q", bad, stderr)
		}
		if readMarks(t, p, me) != "" {
			t.Errorf("row %s: marks raised", bad)
		}
	}
}

// A candidate whose `from` is not a string fails `$got[.from]` the same way.
func TestNonStringFromStopsTheFold(t *testing.T) {
	for _, row := range []string{
		`{"ts":100,"crew_id":"c1","kind":"msg","from":5,"to":"` + me + `","body":"x"}`,
		`{"ts":100,"crew_id":"c1","kind":"msg","to":"` + me + `","body":"x"}`,
	} {
		p := fixture(t)
		writeLog(t, p, row)

		stdout, _, code := run(t, p, me, "--timeout", "0")
		if stdout != "" || code != 0 {
			t.Errorf("%s: stdout %q code %d", row, stdout, code)
		}
	}
}

// A `body` that is not a string is a value like any other: delivered whole.
func TestNonObjectBody(t *testing.T) {
	p := fixture(t)
	row := `{"ts":100,"crew_id":"c1","kind":"msg","from":"` + disp + `","to":"` + me + `","body":{"verdict":"accept"}}`
	writeLog(t, p, row)

	stdout, _, code := run(t, p, me, "--timeout", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, row) {
		t.Errorf("stdout = %q", stdout)
	}
}

// A mark the file left as a string outranks any ts (jq's total order, `//`
// treating only null and false as absent), so the reply is not due.
func TestStringMarkOutranksTs(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msg(100, disp, me, "answer"))
	if err := os.MkdirAll(p.Dir+"/await", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marks.Path(p.Dir, "c1", me), []byte(`{"`+disp+`":"abc"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := run(t, p, me, "--timeout", "0")
	if stdout != "" || code != 0 {
		t.Errorf("stdout %q code %d", stdout, code)
	}
}

// A marks file no reader can parse is no marks at all: redeliver rather than go
// blind.
func TestGarbageMarksFileStillDelivers(t *testing.T) {
	p := fixture(t)
	row := msg(100, disp, me, "answer")
	writeLog(t, p, row)
	if err := os.MkdirAll(p.Dir+"/await", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marks.Path(p.Dir, "c1", me), []byte(`{"dispatcher:c1":5}"x":6}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := run(t, p, me, "--timeout", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, row) {
		t.Errorf("stdout = %q", stdout)
	}
}

// A merge, not a replace: a sender this await never saw keeps its mark.
func TestMarksMergeWithTheOnesHeld(t *testing.T) {
	p := fixture(t)
	mark(t, p, me, msg(900, role, me, "older verdict"))
	row := msg(100, disp, me, "answer")
	writeLog(t, p, row)

	if _, _, code := run(t, p, me, "--timeout", "0"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := readMarks(t, p, me); got != `{"`+disp+`":100,"`+role+`":900}` {
		t.Errorf("marks = %s", got)
	}
}

// `[ -f "$log" ]` is the arm's test: missing, unreadable, or a directory, and
// the pass reads nothing.
func TestLogThatIsNotARegularFile(t *testing.T) {
	p := fixture(t)
	if err := os.MkdirAll(p.Log, 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := run(t, p, me, "--timeout", "0")
	if stdout != "" || code != 0 {
		t.Errorf("stdout %q code %d", stdout, code)
	}

	p2 := fixture(t)
	if _, _, code := run(t, p2, me, "--timeout", "0"); code != 0 {
		t.Errorf("missing log: exit %d", code)
	}
}

// The timeout note reports the wait the polls consumed — under the virtual clock
// that is the interval times the polls, exactly as the bats row reads it.
func TestTimeoutNoteReportsTheElapsedWait(t *testing.T) {
	p := fixture(t)
	stdout, stderr, code := run(t, p, me, "--timeout", "7", "--interval", "3")
	if stdout != "" || code != 0 {
		t.Errorf("stdout %q code %d", stdout, code)
	}
	if want := "crew: await ended after 9s — no reply to " + me + " yet\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// A reply that lands while the await polls is the reason it reads the log every
// pass: the first pass finds nothing, a later one delivers. The clock file is
// the await's own advance, so "it has parked" is observable (bats' own harness
// waits the same way, `after_await_parks`).
func TestReplyLandsMidWait(t *testing.T) {
	p := fixture(t)
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Log, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "clock")
	if err := os.WriteFile(path, []byte("1800000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	row := msg(100, disp, me, "late")

	posted := make(chan struct{})
	go func() {
		defer close(posted)
		for range 2000 {
			if b, err := os.ReadFile(path); err == nil && string(b) != "1800000000\n" {
				break
			}
			time.Sleep(time.Millisecond)
		}
		_ = os.WriteFile(p.Log, []byte(row+"\n"), 0o644)
	}()

	_, stderr, code := runOpts(t, p, Options{
		Clock: clock.Clock{Now: func() time.Time { return now }, CrewClock: path},
	}, me, "--timeout", "30", "--interval", "1")
	<-posted
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if got, err := os.ReadFile(p.Log); err != nil || !strings.Contains(string(got), "late") {
		t.Errorf("reply never posted: %q (%v)", got, err)
	}
}

func TestClampNotice(t *testing.T) {
	p := fixture(t)
	_, stderr, code := run(t, p, me, "--timeout", "5000", "--interval", "60")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "crew: await --timeout 5000 clamped to 600 (the 600s tool ceiling)") {
		t.Errorf("stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "ended after 600s") {
		t.Errorf("stderr = %q", stderr)
	}
}

// `010` is ten, not eight: the arm parsed with `$((10#$timeout))`.
func TestTimeoutIsDecimalWithLeadingZeros(t *testing.T) {
	p := fixture(t)
	_, stderr, code := run(t, p, me, "--timeout", "010", "--interval", "4")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "ended after 12s") {
		t.Errorf("stderr = %q", stderr)
	}
}

// The arm's validation order and every one of its strings.
func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no agent", nil, usageMsg},
		{"empty agent", []string{"", "--timeout", "0"}, usageMsg},
		{"branch-only worker id", []string{"worker:feat/x", "--timeout", "1"},
			"crew: await: 'worker:feat/x' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message"},
		{"timeout needs a value", []string{me, "--timeout"}, "crew: --timeout needs a value"},
		{"timeout empty value", []string{me, "--timeout", ""}, "crew: --timeout needs a value"},
		{"interval needs a value", []string{me, "--interval"}, "crew: --interval needs a value"},
		{"from needs a value", []string{me, "--from"}, "crew: --from needs a value"},
		{"unknown arg", []string{me, "--mine"}, "crew: await: unknown arg '--mine'"},
		{"timeout not digits", []string{me, "--timeout", "abc"}, timeoutUsage},
		{"timeout negative", []string{me, "--timeout", "-1"}, timeoutUsage},
		{"timeout ten digits", []string{me, "--timeout", "1234567890"}, timeoutUsage},
		{"timeout float", []string{me, "--timeout", "1.5"}, timeoutUsage},
	}
	for _, tc := range cases {
		p := fixture(t)
		_, stderr, code := run(t, p, tc.args...)
		if code != 1 || stderr != tc.want+"\n" {
			t.Errorf("%s: code %d stderr %q, want %q", tc.name, code, stderr, tc.want)
		}
	}
}

// The crew check comes first, as it does in the arm.
func TestNoCrewID(t *testing.T) {
	p := fixture(t)
	_, stderr, code := runOpts(t, p, Options{CrewID: func() string { return "" }}, me, "--timeout", "0")
	if code != 1 || stderr != noCrewMsg+"\n" {
		t.Errorf("code %d stderr %q", code, stderr)
	}
}

// The arm raised the marks for any `<agent>`: a role waits on them too.
func TestMarksRaisedForAnyAgent(t *testing.T) {
	p := fixture(t)
	row := msg(100, disp, role, "verdict")
	writeLog(t, p, row)

	stdout, _, code := run(t, p, role, "--timeout", "0")
	if code != 0 || stdout != want(t, row) {
		t.Fatalf("stdout %q code %d", stdout, code)
	}
	if got := readMarks(t, p, role); got != `{"`+disp+`":100}` {
		t.Errorf("marks = %s", got)
	}
}

// The rows have to be on the wire before the marks go up: a mark raised for rows
// nobody received makes the next await skip them for good.
func TestFlushFailureLeavesMarksAlone(t *testing.T) {
	p := fixture(t)
	row := msg(100, disp, me, "answer")
	writeLog(t, p, row)

	_, _, code := runOpts(t, p, Options{Flush: func() error { return errors.New("no space") }}, me, "--timeout", "0")
	if code != 1 {
		t.Errorf("code %d", code)
	}
	if readMarks(t, p, me) != "" {
		t.Error("marks raised for rows that never reached the caller")
	}
}

// `_clock_now_ms` was the arm's one un-silenced jq, so a broken $JQ_COLORS
// surfaced there — once for the call, and never under the virtual clock.
func TestJQColorsWarning(t *testing.T) {
	p := fixture(t)
	_, stderr, code := runOpts(t, p, Options{JQColorsInvalid: true, Clock: clock.Clock{Now: func() time.Time { return now }}},
		me, "--timeout", "0")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := strings.Count(stderr, "Failed to set $JQ_COLORS"); got != 1 {
		t.Errorf("warnings = %d, stderr %q", got, stderr)
	}

	p2 := fixture(t)
	_, stderr, code = runOpts(t, p2, Options{JQColorsInvalid: true, Clock: clockIn(t, 1800000000)}, me, "--timeout", "7", "--interval", "3")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(stderr, "JQ_COLORS") {
		t.Errorf("CREW_CLOCK starts no jq: %q", stderr)
	}
}

// With no virtual clock a bad interval is `sleep`'s own failure, and the arm's
// `set -e` turned that into exit 1.
func TestBadIntervalExits(t *testing.T) {
	p := fixture(t)
	_, _, code := runOpts(t, p, Options{Clock: clock.Clock{Now: func() time.Time { return now }}},
		me, "--timeout", "300", "--interval", "not-an-interval")
	if code != 1 {
		t.Errorf("code %d", code)
	}
}
