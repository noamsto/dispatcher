package await

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
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

func run(t *testing.T, p bus.Paths, req Request) (string, string, int) {
	t.Helper()
	return runOpts(t, p, Options{}, req)
}

func runOpts(t *testing.T, p bus.Paths, o Options, req Request) (string, string, int) {
	t.Helper()
	if o.CrewID == nil {
		o.CrewID = func() string { return "c1" }
	}
	// A test that wants the real clock sets Now and leaves CrewClock empty.
	if o.Clock.Now == nil {
		o.Clock = clockIn(t, 1800000000)
	}
	var out, errB bytes.Buffer
	code := Run(req, p, &out, &errB, o)
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
	dec := json.NewDecoder(strings.NewReader(row))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if !marks.Record(p.Dir, "c1", who, []any{v}) {
		t.Fatal("marks.Record failed")
	}
}

func TestDeliversDueReply(t *testing.T) {
	p := fixture(t)
	row := msg(1785951264000, disp, me, "answer")
	writeLog(t, p, row)

	stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "0"})
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

	stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "0"})
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

	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, a1, a2, a3) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, a1, a2, a3))
	}
	// The other sender's rows are neither printed nor marked: a later await still
	// owes them.
	if got := readMarks(t, p, me); got != `{"`+disp+`":500}` {
		t.Errorf("marks = %s", got)
	}

	stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "300", Interval: "1"})
	if code != 0 || stdout != want(t, b1, b2) {
		t.Fatalf("second await: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if got := readMarks(t, p, me); got != `{"`+disp+`":500,"`+role+`":400}` {
		t.Errorf("marks after the second await = %s", got)
	}
}

func TestFromRestrictsTheSender(t *testing.T) {
	p := fixture(t)
	a1 := msg(100, disp, me, "a1")
	b1 := msg(200, role, me, "b1")
	b2 := msg(300, role, me, "b2")
	writeLog(t, p, a1, b1, b2)

	// The awaited sender's whole due backlog comes as one batch, oldest first
	// (#466).
	stdout, _, code := run(t, p, Request{Me: me, From: role, Timeout: "0"})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, b1, b2) {
		t.Errorf("stdout = %q", stdout)
	}
	if got := readMarks(t, p, me); got != `{"`+role+`":300}` {
		t.Errorf("marks = %s", got)
	}

	// A second await for the same sender has nothing left to hand out.
	stdout, stderr, code := run(t, p, Request{Me: me, From: role, Timeout: "0"})
	if code != 0 || stdout != "" {
		t.Fatalf("second await: code %d stdout %q", code, stdout)
	}
	if !strings.HasSuffix(stderr, "no reply to "+me+" from "+role+" yet\n") {
		t.Errorf("stderr = %q", stderr)
	}

	// The sender this await never waited for is neither returned nor consumed
	// (#300).
	stdout, stderr, code = run(t, p, Request{Me: me, Timeout: "300", Interval: "1"})
	if code != 0 || stdout != want(t, a1) {
		t.Fatalf("plain await: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// A --from that never matches is a timeout that names the sender and still exits
// 0 with empty stdout.
func TestFromMismatchTimesOut(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msg(100, disp, me, "a1"))

	stdout, stderr, code := run(t, p, Request{Me: me, From: "dispatcher:c9", Timeout: "0"})
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

	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
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
	body := first + "\n" + `{"ts":200,"crew_id":"c-to` + "\n" + `{"a":1} {"b":2}` + "\n\n" + "null\n" + last + "\n" +
		`{"ts":400,"crew_id":"c-to`
	if err := os.WriteFile(p.Log, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
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

	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != want(t, first, second) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, first, second))
	}
	if got := readMarks(t, p, me); got != `{"`+disp+`":100}` {
		t.Errorf("marks = %s", got)
	}

	// The batch left nothing behind: a same-ts sibling is never re-handed.
	stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "0"})
	if code != 0 || stdout != "" {
		t.Fatalf("second await: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// A row the fold cannot index is a jq type error, and the arm threw that away:
// nothing is due, nothing is marked, the poll runs to the deadline. (`null` is
// not one of these: jq indexes it to null, so it is simply not a candidate.)
func TestUnindexableRowStopsTheFold(t *testing.T) {
	for _, bad := range []string{"5", `"s"`, `[]`, `true`} {
		p := fixture(t)
		writeLog(t, p, msg(100, disp, me, "answer"), bad)

		stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "0"})
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

		stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
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

	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
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

	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
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

	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
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

	if _, _, code := run(t, p, Request{Me: me, Timeout: "0"}); code != 0 {
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
	stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"})
	if stdout != "" || code != 0 {
		t.Errorf("stdout %q code %d", stdout, code)
	}

	p2 := fixture(t)
	if _, _, code := run(t, p2, Request{Me: me, Timeout: "0"}); code != 0 {
		t.Errorf("missing log: exit %d", code)
	}
}

// The timeout note reports the wait the polls consumed — under the virtual clock
// that is the interval times the polls, exactly as the bats row reads it.
func TestTimeoutNoteReportsTheElapsedWait(t *testing.T) {
	p := fixture(t)
	stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "7", Interval: "3"})
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

	stdout, stderr, code := runOpts(t, p, Options{
		Clock: clock.Clock{Now: func() time.Time { return now }, CrewClock: path},
	}, Request{Me: me, Timeout: "30", Interval: "1"})
	<-posted
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	// Delivered, not merely posted: a timeout exits 0 too.
	if stdout != want(t, row) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, row))
	}
	if note := "no reply to " + me + " yet"; strings.Contains(stderr, note) {
		t.Errorf("stderr = %q, want no timeout note", stderr)
	}
}

func TestClampNotice(t *testing.T) {
	p := fixture(t)
	_, stderr, code := run(t, p, Request{Me: me, Timeout: "5000", Interval: "60"})
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
	_, stderr, code := run(t, p, Request{Me: me, Timeout: "010", Interval: "4"})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "ended after 12s") {
		t.Errorf("stderr = %q", stderr)
	}
}

// The arm's value checks in its order: usage, the session suffix, the timeout.
// The argv grammar's refusals are main_test.go's.
func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"no agent", Request{}, usageMsg},
		{"empty agent with a timeout", Request{Timeout: "0"}, usageMsg},
		{"branch-only worker id", Request{Me: "worker:feat/x", Timeout: "1"},
			"crew: await: 'worker:feat/x' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message"},
		{"suffix before a bad timeout", Request{Me: "worker:feat/x", Timeout: "abc"},
			"crew: await: 'worker:feat/x' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message"},
		{"timeout not digits", Request{Me: me, Timeout: "abc"}, timeoutUsage},
		{"timeout negative", Request{Me: me, Timeout: "-1"}, timeoutUsage},
		{"timeout ten digits", Request{Me: me, Timeout: "1234567890"}, timeoutUsage},
		{"timeout float", Request{Me: me, Timeout: "1.5"}, timeoutUsage},
	}
	for _, tc := range cases {
		p := fixture(t)
		_, stderr, code := run(t, p, tc.req)
		if code != 1 || stderr != tc.want+"\n" {
			t.Errorf("%s: code %d stderr %q, want %q", tc.name, code, stderr, tc.want)
		}
	}
}

// The crew check comes first, as it does in the arm.
func TestNoCrewID(t *testing.T) {
	p := fixture(t)
	_, stderr, code := runOpts(t, p, Options{CrewID: func() string { return "" }}, Request{Me: me, Timeout: "0"})
	if code != 1 || stderr != noCrewMsg+"\n" {
		t.Errorf("code %d stderr %q", code, stderr)
	}
}

// The arm raised the marks for any `<agent>`: a role waits on them too.
func TestMarksRaisedForAnyAgent(t *testing.T) {
	p := fixture(t)
	row := msg(100, disp, role, "verdict")
	writeLog(t, p, row)

	stdout, _, code := run(t, p, Request{Me: role, Timeout: "0"})
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

	_, _, code := runOpts(t, p, Options{Flush: func() error { return errors.New("no space") }}, Request{Me: me, Timeout: "0"})
	if code != 1 {
		t.Errorf("code %d", code)
	}
	if readMarks(t, p, me) != "" {
		t.Error("marks raised for rows that never reached the caller")
	}
}

// The worker's own outbound rows are never candidates, so none of them is a
// watermark: the reply can cross the question in flight (#385), a later outbound
// to a third party cannot hide an earlier reply, and --from names the awaited
// sender without turning the question to it into a cursor.
func TestOutboundRowsAreNotCandidates(t *testing.T) {
	const third = "worker:feat/y#s2-2"
	const plan = "role:feat/x:plan-critic"
	for _, tc := range []struct {
		name  string
		from  string
		due   string
		rows  []string
		marks string
	}{{
		// The per-sender mark, not the newest outbound row, is the anchor.
		name:  "a question that crossed the reply",
		due:   msg(100, disp, me, "answer"),
		rows:  []string{msg(100, disp, me, "answer"), msg(200, me, disp, "why?")},
		marks: `{"` + disp + `":100}`,
	}, {
		// An outbound to someone else is no watermark either.
		name:  "a later outbound to a third party",
		due:   msg(200, disp, me, "answer"),
		rows:  []string{msg(100, me, disp, "why?"), msg(200, disp, me, "answer"), msg(300, me, third, "unrelated")},
		marks: `{"` + disp + `":200}`,
	}, {
		// #385 under --from: the restriction names the sender, not the cursor.
		name:  "the question to the awaited sender",
		from:  plan,
		due:   msg(100, plan, me, "verdict"),
		rows:  []string{msg(100, plan, me, "verdict"), msg(200, me, plan, "review diff")},
		marks: `{"` + plan + `":100}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.rows...)
			stdout, stderr, code := run(t, p, Request{Me: me, From: tc.from, Timeout: "0"})
			if code != 0 {
				t.Fatalf("exit %d stderr %q", code, stderr)
			}
			if stdout != want(t, tc.due) {
				t.Errorf("stdout\n got %q\nwant %q", stdout, want(t, tc.due))
			}
			// Only the delivering sender is marked: an outbound target gets none.
			if got := readMarks(t, p, me); got != tc.marks {
				t.Errorf("marks = %s, want %s", got, tc.marks)
			}
		})
	}
}

// #760: a second blocked episode. The first answer is delivered and marked; the
// reply to the second question is already on the bus when the next await starts,
// so it comes out at once instead of waiting for a third.
func TestSecondEpisodeDeliversTheNewerReply(t *testing.T) {
	p := fixture(t)
	q1, a1 := msg(100, me, disp, "Q1"), msg(200, disp, me, "A1")
	writeLog(t, p, q1, a1)
	if stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"}); code != 0 || stdout != want(t, a1) {
		t.Fatalf("first episode: code %d stdout %q", code, stdout)
	}

	// A second question, and its answer, land before the next await starts.
	q2, a2 := msg(300, me, disp, "Q2"), msg(400, disp, me, "A2")
	writeLog(t, p, q1, a1, q2, a2)
	stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "300", Interval: "1"})
	if code != 0 || stdout != want(t, a2) {
		t.Fatalf("second episode: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if got := readMarks(t, p, me); got != `{"`+disp+`":400}` {
		t.Errorf("marks after the second episode = %s", got)
	}
}

// A delivered reply stays delivered across the traffic that follows it: a
// question to the next critic is outbound, so it neither delivers nor re-opens
// the mark, while that critic's own answer is due at once.
func TestCriticTrafficDoesNotReopenADeliveredReply(t *testing.T) {
	const spec = "role:feat/x:spec-critic"
	const plan = "role:feat/x:plan-critic"
	for _, tc := range []struct{ name, first, body, marks string }{
		{name: "a critic verdict", first: spec, body: "spec verdict",
			marks: `{"` + plan + `":400,"` + spec + `":200}`},
		{name: "a dispatcher answer", first: disp, body: "answer",
			marks: `{"` + disp + `":200,"` + plan + `":400}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			ask, reply := msg(100, me, tc.first, "review"), msg(200, tc.first, me, tc.body)
			writeLog(t, p, ask, reply)
			if stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"}); code != 0 || stdout != want(t, reply) {
				t.Fatalf("first await: code %d stdout %q", code, stdout)
			}

			askPlan := msg(300, me, plan, "review plan")
			writeLog(t, p, ask, reply, askPlan)
			stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "0"})
			if code != 0 || stdout != "" {
				t.Fatalf("await after the question to the next critic: code %d stdout %q", code, stdout)
			}
			if !strings.HasSuffix(stderr, "no reply to "+me+" yet\n") {
				t.Errorf("stderr = %q", stderr)
			}

			// The next critic's answer is a new sender's row: due at once, and the
			// first sender's mark rides through the merge.
			planReply := msg(400, plan, me, "plan verdict")
			writeLog(t, p, ask, reply, askPlan, planReply)
			stdout, stderr, code = run(t, p, Request{Me: me, Timeout: "300", Interval: "1"})
			if code != 0 || stdout != want(t, planReply) {
				t.Fatalf("next critic: code %d stdout %q stderr %q", code, stdout, stderr)
			}
			if got := readMarks(t, p, me); got != tc.marks {
				t.Errorf("marks = %s, want %s", got, tc.marks)
			}
		})
	}
}

// A status row is not a msg: a watchdog-sourced blocked row is neither a reply
// nor a watermark, so the reply that follows it still comes out, once. The
// second row is addressed to this session, so the kind test is what excludes it.
func TestStatusRowsAreNotReplies(t *testing.T) {
	p := fixture(t)
	blocked := `{"ts":100,"crew_id":"c1","kind":"status","from":"` + me + `","to":"dispatcher:c1",` +
		`"body":{"state":"blocked","detail":"prompt: interactive prompt in pane %9","source":"watchdog"}}`
	statusToMe := `{"ts":150,"crew_id":"c1","kind":"status","from":"` + disp + `","to":"` + me + `","body":{"state":"working"}}`
	writeLog(t, p, blocked, statusToMe)
	stdout, stderr, code := run(t, p, Request{Me: me, Timeout: "0"})
	if code != 0 || stdout != "" {
		t.Fatalf("code %d stdout %q", code, stdout)
	}
	if !strings.HasSuffix(stderr, "no reply to "+me+" yet\n") {
		t.Errorf("stderr = %q", stderr)
	}
	if got := readMarks(t, p, me); got != "" {
		t.Errorf("marks after a status-only bus = %s", got)
	}

	answer := msg(200, disp, me, "answer")
	writeLog(t, p, blocked, statusToMe, answer)
	stdout, stderr, code = run(t, p, Request{Me: me, Timeout: "300", Interval: "1"})
	if code != 0 || stdout != want(t, answer) {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if got := readMarks(t, p, me); got != `{"`+disp+`":200}` {
		t.Errorf("marks = %s", got)
	}
}

// Delivered state is keyed per session, so a resumed session (new id) starts
// clean: another session's mark at the same ts never hides this one's reply.
func TestMarksArePerSession(t *testing.T) {
	const other = "worker:feat/x#s2-2"
	p := fixture(t)
	first, second := msg(100, disp, me, "answer"), msg(100, disp, other, "answer2")
	writeLog(t, p, first, second)

	if stdout, _, code := run(t, p, Request{Me: me, Timeout: "0"}); code != 0 || stdout != want(t, first) {
		t.Fatalf("first session: code %d stdout %q", code, stdout)
	}
	stdout, stderr, code := run(t, p, Request{Me: other, Timeout: "300", Interval: "1"})
	if code != 0 || stdout != want(t, second) {
		t.Fatalf("resumed session: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if got := readMarks(t, p, other); got != `{"`+disp+`":100}` {
		t.Errorf("the resumed session's marks = %s", got)
	}
	if got := readMarks(t, p, me); got != `{"`+disp+`":100}` {
		t.Errorf("the first session's marks = %s", got)
	}
}

// With no virtual clock a bad interval is `sleep`'s own failure, and the arm's
// `set -e` turned that into exit 1.
func TestBadIntervalExits(t *testing.T) {
	p := fixture(t)
	_, _, code := runOpts(t, p, Options{Clock: clock.Clock{Now: func() time.Time { return now }}},
		Request{Me: me, Timeout: "300", Interval: "not-an-interval"})
	if code != 1 {
		t.Errorf("code %d", code)
	}
}
