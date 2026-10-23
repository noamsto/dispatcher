package watch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

// base is the wall clock every fixture starts at. watch never reads
// $CREW_CLOCK, so the fake below is the real pair — the same Now/Sleep the
// arm had — with the clock moved by the interval instead of waited out.
var base = time.Unix(1800000000, 0)

// park is a run's clock: Now reads it, Sleep advances it by the interval it was
// handed, so a test that polls three times spends three seconds and no real
// ones. The interval string is kept so a test can pin what `sleep` would get.
type park struct {
	now       time.Time
	intervals []string
}

func (p *park) Now() time.Time { return p.now }

func (p *park) Sleep(interval string, _ io.Writer) error {
	d, err := time.ParseDuration(interval + "s")
	if err != nil {
		// What coreutils' `sleep` does with it; the caller exits 1 either way.
		return err
	}
	p.intervals = append(p.intervals, interval)
	p.now = p.now.Add(d)
	return nil
}

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

func status(ts int64, from, state, detail, source string) string {
	return statusIn("c1", ts, from, state, detail, source)
}

// statusIn is seed_raw's row for a crew other than the fixture's c1: detail and
// source are dropped when empty, as the helper's two `if $x!=""` branches do.
func statusIn(crew string, ts int64, from, state, detail, source string) string {
	body := `{"state":"` + state + `"}`
	if detail != "" {
		body = strings.TrimSuffix(body, "}") + `,"detail":` + quote(detail) + "}"
	}
	if source != "" {
		body = strings.TrimSuffix(body, "}") + `,"source":"` + source + `"}`
	}
	return `{"ts":` + itoa(ts) + `,"crew_id":"` + crew + `","from":"` + from +
		`","to":"dispatcher:` + crew + `","kind":"status","body":` + body + "}"
}

func msg(ts int64, from, to string) string {
	return `{"ts":` + itoa(ts) + `,"crew_id":"c1","from":"` + from + `","to":"` + to +
		`","kind":"msg","body":"{\"q\":1}"}`
}

func quote(s string) string {
	return strconv.Quote(s)
}

func since(s string) *string { return &s }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func run(t *testing.T, p bus.Paths, req Request) (string, string, int, *park) {
	t.Helper()
	return runOpts(t, p, Options{}, req)
}

func runOpts(t *testing.T, p bus.Paths, o Options, req Request) (string, string, int, *park) {
	t.Helper()
	pk := &park{now: base}
	if o.CrewID == nil {
		o.CrewID = func() string { return "c1" }
	}
	if o.Clock.Now == nil {
		o.Clock.Now = pk.Now
	}
	if o.Sleep == nil {
		o.Sleep = pk.Sleep
	}
	var out, errB bytes.Buffer
	code := Run(req, p, &out, &errB, o)
	return out.String(), errB.String(), code, pk
}

// want is the arm's `jq -c` line for these rows: value-identical, with keys
// sorted the way gojq hands objects back (the sanctioned key-order difference).
func want(t *testing.T, cursor int64, rows ...string) string {
	t.Helper()
	events := "[" + strings.Join(rows, ",") + "]"
	return testjson.Compact(testjson.MustParse(t, `{"cursor":`+itoa(cursor)+`,"events":`+events+`}`)) + "\n"
}

func cursorOf(t *testing.T, p bus.Paths, crew string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.CrewDir(crew), "cursor"))
	if err != nil {
		return "<missing>"
	}
	return strings.TrimRight(string(data), "\n")
}

func TestQualifyingStatusPrintsBatchAndMovesCursor(t *testing.T) {
	p := fixture(t)
	row := status(1785951264000, "worker:feat/x#s1-1", "done", "", "")
	writeLog(t, p, row)

	stdout, stderr, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if stdout != want(t, 1785951264000, row) {
		t.Errorf("stdout = %s, want %s", stdout, want(t, 1785951264000, row))
	}
	if got := cursorOf(t, p, "c1"); got != "1785951264000" {
		t.Errorf("cursor file = %q", got)
	}
	// The lock is the park's, so a finished park leaves none behind.
	if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "watch.lock.d")); !os.IsNotExist(err) {
		t.Error("the park left watch.lock.d behind")
	}
}

// A stream-spawned inner watch whose named owner is not its parent is an orphan:
// it must exit without folding or advancing the shared cursor, so a restarted
// lane still delivers the events (#1139). A directly armed `crew watch` carries
// no CREW_WATCH_PARENT and stays unchecked (the rest of this file).
func TestOrphanExitsWithoutAdvancingCursor(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951264000, "worker:feat/x#s1-1", "done", "", ""))
	// A process is never its own parent, so this owner pid can never match
	// os.Getppid() and the watch is orphaned by construction.
	t.Setenv("CREW_WATCH_PARENT", itoa(int64(os.Getpid())))

	stdout, _, code, _ := run(t, p, Request{Crew: "c1"})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("orphan printed a batch: %q", stdout)
	}
	if got := cursorOf(t, p, "c1"); got != "<missing>" {
		t.Errorf("cursor = %q, want untouched", got)
	}
}

func TestMsgToDispatcherAndStarWake(t *testing.T) {
	for _, to := range []string{"dispatcher:c1", "*"} {
		t.Run("to "+to, func(t *testing.T) {
			p := fixture(t)
			row := msg(1785951264000, "worker:feat/x#s1-1", to)
			writeLog(t, p, row)
			stdout, stderr, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
			if code != 0 || stderr != "" {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if stdout != want(t, 1785951264000, row) {
				t.Errorf("stdout = %s, want %s", stdout, want(t, 1785951264000, row))
			}
		})
	}
}

func TestStatesNarrowTheWakeSet(t *testing.T) {
	row := status(1785951264000, "worker:feat/x#s1-1", "working", "", "")

	// `working` is not in the default set, so the park expires quietly — with or
	// without `--crew`, which picks the bus and not the wake set.
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"default states", Request{Since: since("0"), Timeout: "1", Interval: "1"}},
		{"--crew, default states", Request{Crew: "c1", Since: since("0"), Timeout: "1", Interval: "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, row)
			stdout, stderr, code, _ := run(t, p, tc.req)
			if code != 0 || stdout != "" ||
				stderr != "crew: watch park ended after 1s — no new events (cursor 0)\n" {
				t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
			}
		})
	}

	p2 := fixture(t)
	writeLog(t, p2, row)
	stdout, stderr, code, _ := run(t, p2, Request{States: "blocked,working", Since: since("0"), Timeout: "1"})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if stdout != want(t, 1785951264000, row) {
		t.Errorf("stdout = %s, want %s", stdout, want(t, 1785951264000, row))
	}
}

// `reaped` is the reap verb's own terminal row: outside the default wake set,
// while the same setup with `done` wakes.
func TestReapedStatusDoesNotWake(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951264000, "worker:feat/x#s1-1", "reaped", "PR #8 MERGED", "reap"))
	stdout, stderr, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 || stdout != "" || stderr != "crew: watch park ended after 1s — no new events (cursor 0)\n" {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}

	p2 := fixture(t)
	row := status(1785951264000, "worker:feat/x#s1-1", "done", "PR #8 MERGED", "reap")
	writeLog(t, p2, row)
	stdout, stderr, code, _ = run(t, p2, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 || stderr != "" || stdout != want(t, 1785951264000, row) {
		t.Fatalf("control: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// The `exited` gate (#396): the SessionEnd backstop wakes the park only as the
// session's first terminal state.
func TestExitedWakesOnlyAsFirstTerminalState(t *testing.T) {
	for _, tc := range []struct {
		prev  string
		wakes bool
	}{
		{"working", true},
		{"blocked", true},
		{"done", false},
		{"failed", false},
	} {
		t.Run(tc.prev+" then exited", func(t *testing.T) {
			p := fixture(t)
			from := "worker:feat/x#s1-1"
			exited := status(2000, from, "exited", "", "")
			writeLog(t, p, status(1000, from, tc.prev, "", ""), exited)
			stdout, _, code, _ := run(t, p, Request{Since: since("1000"), Timeout: "1", Interval: "1"})
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			// Exactly one event, the `exited`: the row at the cursor stays behind it.
			quiet := ""
			if tc.wakes {
				quiet = want(t, 2000, exited)
			}
			if stdout != quiet {
				t.Errorf("stdout = %q, want %q", stdout, quiet)
			}
		})
	}
}

func TestBlockedRestampSuppression(t *testing.T) {
	const first = "need a waiver — awaited 300s, no reply (cycle 7 of 24)"
	for _, tc := range []struct {
		name   string
		detail string
		wakes  bool
	}{
		{"the cycle number alone normalises away", "need a waiver — awaited 300s, no reply (cycle 8 of 24)", false},
		{"cycle and awaited noise normalise away", "need a waiver — awaited 302s, no reply (cycle 8 of 24)", false},
		{"a changed question wakes", "different question (cycle 2 of 24)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			from := "worker:feat/x#s1-1"
			second := status(2000, from, "blocked", tc.detail, "")
			writeLog(t, p, status(1000, from, "blocked", first, ""), second)
			stdout, _, code, _ := run(t, p, Request{Since: since("1000"), Timeout: "1", Interval: "1"})
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			quiet := ""
			if tc.wakes {
				quiet = want(t, 2000, second)
			}
			if stdout != quiet {
				t.Errorf("stdout = %q, want %q", stdout, quiet)
			}
		})
	}
}

// The watchdog's `… cleared` suppression, including the patched anchor: jq's
// `$` also matched before one trailing newline, which Go's does not, so the
// embedded copy spells the position out.
func TestWatchdogClearedSuppressed(t *testing.T) {
	for _, tc := range []struct{ name, detail string }{
		{"load cleared", "load: cleared"},
		{"cleared with a trailing newline", "load: cleared\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			from := "worker:feat/x#s1-1"
			writeLog(t, p, status(1000, from, "working", tc.detail, "watchdog"))
			stdout, _, code, _ := run(t, p, Request{States: "blocked,working", Since: since("0"), Timeout: "1", Interval: "1"})
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %s, want the park to stay quiet", stdout)
			}
		})
	}
}

// A blocked after a working wakes even with the same detail: the suppression is
// against the immediately previous status, not against history.
func TestBlockedAfterWorkingWakesWithSameDetail(t *testing.T) {
	p := fixture(t)
	from := "worker:feat/x#s1-1"
	d := "need a waiver (cycle 7 of 24)"
	restamp := status(3000, from, "blocked", d, "")
	writeLog(t, p,
		status(1000, from, "blocked", d, ""),
		status(2000, from, "working", d, ""),
		restamp)
	stdout, _, code, _ := run(t, p, Request{Since: since("2000"), Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if stdout != want(t, 3000, restamp) {
		t.Errorf("stdout = %s, want %s", stdout, want(t, 3000, restamp))
	}
}

// A first `blocked` has no earlier status for suppression to measure against.
func TestFirstBlockedWakes(t *testing.T) {
	p := fixture(t)
	row := status(1000, "worker:feat/x#s1-1", "blocked", "need a waiver — awaited 300s, no reply (cycle 7 of 24)", "")
	writeLog(t, p, row)
	stdout, stderr, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if stdout != want(t, 1000, row) {
		t.Errorf("stdout = %s, want %s", stdout, want(t, 1000, row))
	}
}

// Suppression is keyed on the session, not on the detail: the same question
// under another session id is another worker asking, so it wakes.
func TestBlockedRestampOnAnotherSessionWakes(t *testing.T) {
	const detail = "need a waiver — awaited 300s, no reply (cycle 7 of 24)"
	p := fixture(t)
	second := status(2000, "worker:feat/x#s2-2", "blocked", detail, "")
	writeLog(t, p, status(1000, "worker:feat/x#s1-1", "blocked", detail, ""), second)
	stdout, _, code, _ := run(t, p, Request{Since: since("1000"), Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if stdout != want(t, 2000, second) {
		t.Errorf("stdout = %s, want %s", stdout, want(t, 2000, second))
	}
}

// An object `detail` is not text, so `detail_text` reads it as "": the empty
// re-stamp after it normalizes equal and stays quiet. The jq arm aborted in
// gsub on this shape and dropped the whole batch, later `done` included.
func TestObjectBlockedDetailDoesNotDropALaterDone(t *testing.T) {
	p := fixture(t)
	object := rawRow(1000, "c1", "worker:feat/x#s1-1", "dispatcher:c1", "status",
		`{"state":"blocked","detail":{"k":true}}`)
	done := status(3000, "worker:feat/x#s2-2", "done", "", "")
	writeLog(t, p, object, status(2000, "worker:feat/x#s1-1", "blocked", "", ""), done)
	stdout, _, code, _ := run(t, p, Request{Since: since("1000"), Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if stdout != want(t, 3000, done) {
		t.Errorf("stdout = %s, want %s", stdout, want(t, 3000, done))
	}
}

// Suppression is a read-side rule: the quiet re-stamp stays on the log for every
// other reader, and a park that suppresses it rewrites nothing.
func TestSuppressedRestampStaysOnTheLog(t *testing.T) {
	p := fixture(t)
	writeLog(t, p,
		status(1000, "worker:feat/x#s1-1", "blocked", "need a waiver — awaited 300s, no reply (cycle 7 of 24)", ""),
		status(2000, "worker:feat/x#s1-1", "blocked", "need a waiver — awaited 300s, no reply (cycle 8 of 24)", ""))
	stdout, _, code, _ := run(t, p, Request{Since: since("1000"), Timeout: "1", Interval: "1"})
	if code != 0 || stdout != "" {
		t.Fatalf("code %d stdout %q", code, stdout)
	}
	data, err := os.ReadFile(p.Log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"state":"blocked"`); n != 2 {
		t.Errorf("the log holds %d blocked rows, want both:\n%s", n, data)
	}
}

// The cursor self-seeds from the crew's file: a valid file is the cursor, and a
// missing, empty or non-numeric one reads as 0.
func TestCursorSelfSeed(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		want string
	}{
		{"valid file seeds it", "1500", "3000"},
		{"empty file seeds 0", "", "3000"},
		{"non-numeric file seeds 0", "not-a-cursor", "3000"},
		{"trailing newlines are trimmed", "1500\n\n", "3000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p,
				status(1000, "worker:feat/x#s1-1", "done", "", ""),
				status(2000, "worker:feat/x#s2-2", "done", "", ""),
				status(3000, "worker:feat/x#s3-3", "done", "", ""))
			if tc.file != "" {
				if err := os.MkdirAll(p.CrewDir("c1"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p.CrewDir("c1"), "cursor"), []byte(tc.file+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			stdout, _, code, _ := run(t, p, Request{Timeout: "1", Interval: "1"})
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			// Seeded from 1500 the oldest row is already delivered, so two
			// arrive; seeded from 0 all three do.
			seeded := tc.file != "" && strings.HasPrefix(tc.file, "1500")
			wantN := map[bool]int{true: 2, false: 3}[seeded]
			if got := strings.Count(stdout, `"kind":"status"`); got != wantN {
				t.Errorf("events = %d, want %d (%s)", got, wantN, stdout)
			}
			if got := cursorOf(t, p, "c1"); got != tc.want {
				t.Errorf("cursor file = %q, want %q", got, tc.want)
			}
		})
	}
}

// `--since 0` is a caller that means the start of the log, not an omitted flag.
func TestExplicitSinceZeroDoesNotSelfSeed(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1000, "worker:feat/x#s1-1", "done", "", ""))
	if err := os.MkdirAll(p.CrewDir("c1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.CrewDir("c1"), "cursor"), []byte("9999999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(stdout, "done") {
		t.Errorf("stdout = %s, want the row the seeded cursor would have hidden", stdout)
	}
}

// Events that share a ts keep log order: the fold's sort is stable, as jq's is.
func TestEqualTimestampsKeepLogOrder(t *testing.T) {
	p := fixture(t)
	writeLog(t, p,
		status(1000, "worker:feat/a#s1-1", "done", "", ""),
		status(1000, "worker:feat/b#s1-1", "done", "", ""))
	stdout, _, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if strings.Index(stdout, "feat/a") > strings.Index(stdout, "feat/b") {
		t.Errorf("events reordered:\n%s", stdout)
	}
}

func TestParkExpiry(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1000, "worker:feat/x#s1-1", "working", "", ""))

	stdout, stderr, code, pk := run(t, p, Request{Since: since("0"), Timeout: "3", Interval: "1"})
	if code != 0 {
		t.Fatalf("an expired park is exit 0, got %d", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want the empty marker", stdout)
	}
	if stderr != "crew: watch park ended after 3s — no new events (cursor 0)\n" {
		t.Errorf("stderr = %q", stderr)
	}
	// The park polled on the interval it was given, and no more.
	if len(pk.intervals) != 3 {
		t.Errorf("polled %d times, want 3", len(pk.intervals))
	}
	if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "cursor")); !os.IsNotExist(err) {
		t.Error("an expired park advanced the cursor")
	}
}

// The expiry line prints the caller's own bytes, as the arm's `${timeout}s` and
// `$since` did.
func TestExpiryLineKeepsCallerText(t *testing.T) {
	p := fixture(t)
	_, stderr, code, _ := run(t, p, Request{Since: since("07"), Timeout: "0300", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if stderr != "crew: watch park ended after 0300s — no new events (cursor 07)\n" {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRefusals(t *testing.T) {
	const (
		sinceMsg  = "crew: --since must be an integer ms timestamp"
		digitsMsg = "crew: --timeout must be a positive integer number of seconds"
		positive  = "crew: --timeout must be > 0 (indefinite watch unsupported: a reaped watch would be undetectable)"
	)
	// The argv grammar's refusals are main_test.go's; these are the value checks.
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		// A following flag is a value like any other word: the check on that
		// value is what speaks next.
		{"a flag swallowed as the since", Request{Since: since("--timeout")}, sinceMsg},
		{"non-integer --since", Request{Since: since("1x")}, sinceMsg},
		{"negative --since", Request{Since: since("-1")}, sinceMsg},
		{"non-integer --timeout", Request{Timeout: "1x"}, digitsMsg},
		{"negative --timeout", Request{Timeout: "-1"}, digitsMsg},
		{"zero --timeout", Request{Timeout: "0"}, positive},
		// bash's `[ "$timeout" -gt 0 ]` errors on a value past its signed long,
		// and `set -e` made that this same refusal: an unbounded park is what
		// the flag rules out, so it never becomes one.
		{"past-int64 --timeout", Request{Timeout: "9223372036854775808"}, positive},
		{"20-digit --timeout", Request{Timeout: "99999999999999999999"}, positive},
		{"empty --states set", Request{States: ","}, "crew: --states must be non-empty"},
		{"all-empty --states set", Request{States: ",,"}, "crew: --states must be non-empty"},
		{"traversal --crew", Request{Crew: "../escaped"}, badCrewMsg},
		{"dot-dot --crew", Request{Crew: ".."}, badCrewMsg},
		// The value checks run in the arm's order: since, timeout, states.
		{"since before timeout", Request{Since: since("x"), Timeout: "0"}, sinceMsg},
		{"timeout before states", Request{States: ",", Timeout: "0"}, positive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			_, stderr, code, _ := run(t, p, tc.req)
			if code != 1 {
				t.Fatalf("code %d, want 1", code)
			}
			if stderr != tc.want+"\n" {
				t.Errorf("stderr = %q, want %q", stderr, tc.want)
			}
			// A refusal never takes the lock.
			if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "watch.lock.d")); !os.IsNotExist(err) {
				t.Error("a refused call took the watch lock")
			}
		})
	}
}

// The traversal id is refused before it becomes a path: `TestRefusals` pins the
// line it prints, and this is the write half — nothing lands above the bus dir
// (`../../escaped`) nor beside it (`../escaped`).
func TestTraversalCrewWritesNothingOutsideTheBusDir(t *testing.T) {
	p := fixture(t)
	for _, crew := range []string{"../escaped", "../../escaped"} {
		_, _, code, _ := run(t, p, Request{Crew: crew, Timeout: "1", Interval: "1"})
		if code != 1 {
			t.Fatalf("--crew %s: code %d, want 1", crew, code)
		}
		for _, dir := range []string{p.Common, p.Dir} {
			if _, err := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
				t.Errorf("%s/escaped exists after --crew %s (stat err %v)", dir, crew, err)
			}
		}
	}
}

// With no --crew and no `_crew_id`, the arm's refusal is the unset one.
func TestNoCrewResolvesToRefusal(t *testing.T) {
	p := fixture(t)
	_, stderr, code, _ := runOpts(t, p, Options{CrewID: func() string { return "" }}, Request{Timeout: "1"})
	if code != 1 || stderr != "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}

// --crew names a crew whose cursor and lock are its own, not the resolved one's.
func TestCrewFlagOwnsCursorAndLock(t *testing.T) {
	p := fixture(t)
	row9 := statusIn("c9", 2000, "worker:feat/y#s1-1", "done", "", "")
	writeLog(t, p, status(1000, "worker:feat/x#s1-1", "done", "", ""), row9)
	stdout, _, code, _ := run(t, p, Request{Crew: "c9", Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := cursorOf(t, p, "c9"); got != "2000" {
		t.Errorf("c9 cursor = %q", got)
	}
	if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "cursor")); !os.IsNotExist(err) {
		t.Error("the resolved crew's cursor moved instead of --crew's")
	}
	// The cursor the batch reports is the file's.
	if stdout != want(t, 2000, row9) {
		t.Errorf("stdout = %s, want %s", stdout, want(t, 2000, row9))
	}
	for _, crew := range []string{"c1", "c9"} {
		if _, err := os.Stat(filepath.Join(p.CrewDir(crew), "watch.lock.d")); !os.IsNotExist(err) {
			t.Errorf("%s kept the watch lock", crew)
		}
	}

	// The lock it takes is the named crew's: a live holder under c9 refuses.
	lockd := filepath.Join(p.CrewDir("c9"), "watch.lock.d")
	if err := os.MkdirAll(lockd, 0o755); err != nil {
		t.Fatal(err)
	}
	held := strconv.Itoa(os.Getppid()) + "\n"
	if err := os.WriteFile(filepath.Join(lockd, "pid"), []byte(held), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code, _ := run(t, p, Request{Crew: "c9", Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 1 || stderr != "crew: another watch is already running for this crew (c9)\n" {
		t.Errorf("code %d stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "watch.lock.d")); !os.IsNotExist(err) {
		t.Error("--crew took the resolved crew's lock")
	}
}

// A live holder refuses with the arm's line; a dead one is reclaimed through
// the same mkdir gate `crew stream` takes.
func TestLockHeldAndReclaimed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		held    string // "self" is this pid; "" leaves no pid file at all
		refuses bool
	}{
		{"live holder", "parent", true},
		{"dead holder", "99998", false},
		{"empty pid file", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			lockd := filepath.Join(p.CrewDir("c1"), "watch.lock.d")
			if err := os.MkdirAll(lockd, 0o755); err != nil {
				t.Fatal(err)
			}
			held := tc.held
			if held == "parent" {
				// This process cannot stand in for the holder: the protocol is
				// idempotent for the owner's own pid, and that is what the watch
				// writes. The test runner's parent is live and is not it.
				held = strconv.Itoa(os.Getppid())
			}
			if held != "" {
				if err := os.WriteFile(filepath.Join(lockd, "pid"), []byte(held+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			old := time.Now().Add(-10 * time.Second)
			if err := os.Chtimes(lockd, old, old); err != nil {
				t.Fatal(err)
			}
			writeLog(t, p, status(1000, "worker:feat/x#s1-1", "done", "", ""))
			_, stderr, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
			if tc.refuses {
				if code != 1 ||
					stderr != "crew: another watch is already running for this crew (c1)\n" {
					t.Fatalf("code %d stderr %q", code, stderr)
				}
				return
			}
			if code != 0 {
				t.Fatalf("code %d stderr %q: a stale lock must be reclaimed", code, stderr)
			}
		})
	}
}

// The interval reaches `sleep` verbatim, decimals included: the arm never
// validated it, so coreutils owns the grammar.
func TestIntervalReachesSleepVerbatim(t *testing.T) {
	p := fixture(t)
	_, _, _, pk := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "0.1"})
	if len(pk.intervals) == 0 || pk.intervals[0] != "0.1" {
		t.Errorf("intervals = %v, want the caller's string", pk.intervals)
	}
}

// An interval coreutils rejects costs its own status, which is what killed the
// arm under `set -e`: this one runs the real `sleep`.
func TestRejectedIntervalExitsFailure(t *testing.T) {
	p := fixture(t)
	var slept []string
	_, _, code, _ := runOpts(t, p, Options{Sleep: func(interval string, stderr io.Writer) error {
		slept = append(slept, interval)
		err := (&park{}).Sleep(interval, stderr)
		return err
	}}, Request{Since: since("0"), Timeout: "1", Interval: "not-an-interval"})
	if code != 1 {
		t.Fatalf("code %d, want 1 (slept %v)", code, slept)
	}
}

// A past-int64 --since is the one value the arm widened rather than refused:
// jq read it as a double, so nothing is newer and the park expires quietly with
// the caller's own digits in the cursor position.
func TestSincePastInt64ParksQuietly(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1000, "worker:feat/x#s1-1", "done", "", ""))
	const huge = "99999999999999999999"
	stdout, stderr, code, _ := run(t, p, Request{Since: since(huge), Timeout: "1", Interval: "1"})
	if code != 0 || stdout != "" {
		t.Fatalf("code %d stdout %q", code, stdout)
	}
	if stderr != "crew: watch park ended after 1s — no new events (cursor "+huge+")\n" {
		t.Errorf("stderr = %q", stderr)
	}
}

// A timeout whose ms product overflows is bash's `$((start + timeout*1000))`
// wrap: the deadline lands in the past and the park expires with the caller's
// digits in the line.
func TestOverflowingTimeoutExpires(t *testing.T) {
	for _, timeout := range []string{"9223372036854775807", "4611686018427387904"} {
		t.Run(timeout, func(t *testing.T) {
			p := fixture(t)
			stdout, stderr, code, _ := run(t, p, Request{Since: since("0"), Timeout: timeout, Interval: "1"})
			if code != 0 || stdout != "" {
				t.Fatalf("code %d stdout %q", code, stdout)
			}
			if stderr != "crew: watch park ended after "+timeout+"s — no new events (cursor 0)\n" {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

// The status `stream`'s `wait` sees: the conventional 128+n, and 1 for the one
// signal that has no number to report.
func TestSignalStatus(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		want int
	}{
		{syscall.SIGTERM, 143},
		{syscall.SIGINT, 130},
		{syscall.SIGHUP, 129},
	} {
		if got := signalStatus(tc.sig); got != tc.want {
			t.Errorf("signalStatus(%v) = %d, want %d", tc.sig, got, tc.want)
		}
	}
	if got := signalStatus(osSignalStub{}); got != 1 {
		t.Errorf("signalStatus(unnamed) = %d, want 1", got)
	}
}

// osSignalStub is a signal with no syscall number to report.
type osSignalStub struct{}

func (osSignalStub) String() string { return "stub" }
func (osSignalStub) Signal()        {}

// `set -e` ended the arm with whatever `sleep` returned: the child's status for
// an interval it rejects, and 128+n when a signal killed it mid-poll.
func TestExitStatus(t *testing.T) {
	if _, err := exec.Command("sh", "-c", "exit 3").Output(); err != nil {
		if got := childStatus(err); got != 3 {
			t.Errorf("childStatus(exit 3) = %d, want 3", got)
		}
	} else {
		t.Fatal("`exit 3` succeeded")
	}
	if _, err := exec.Command("sh", "-c", "kill -TERM $$").Output(); err != nil {
		if got := childStatus(err); got != 143 {
			t.Errorf("childStatus(SIGTERM) = %d, want 143", got)
		}
	} else {
		t.Skip("`kill -TERM $$` did not fail the child")
	}
	if got := childStatus(errors.New("exec never started")); got != 1 {
		t.Errorf("childStatus(non-ExitError) = %d, want 1", got)
	}
}

// The bus dir is the arm's first statement, so even a refusal creates it.
func TestRunCreatesBusDir(t *testing.T) {
	p := fixture(t)
	if _, _, code, _ := run(t, p, Request{Timeout: "0"}); code != 1 {
		t.Fatalf("code %d", code)
	}
	if st, err := os.Stat(p.Dir); err != nil || !st.IsDir() {
		t.Errorf("bus dir %s: %v", p.Dir, err)
	}
}

// #910: `crew stall-watch` re-posts its `unread:` blocked while a msg stays
// unread, and only the age changes. norm strips the `undelivered for <N>s`
// age the way it strips the cycle and awaited noise, so the re-post is the
// suppressed repeated-blocked case; a changed reason is a different detail
// and wakes.
func TestUnreadRestampSuppressed(t *testing.T) {
	const (
		directive = "unread: dispatcher directive undelivered for %ds — lead is working but has not reached a peek seam (long stage or idle on a background task)"
		verdict   = "unread: role verdict undelivered for %ds — lead is working but has not read it; nudge it to run `crew await`"
		nudge     = "unread: dispatcher directive undelivered for %ds — auto-nudge typed but not accepted (nudge held: %%9 worker:feat/x#s1-1); verify the pane with crew where"
	)
	for _, tc := range []struct {
		name          string
		first, second string
		wakes         bool
	}{
		{"directive re-post differing only in age", fmt.Sprintf(directive, 644), fmt.Sprintf(directive, 1850), false},
		{"verdict re-post differing only in age", fmt.Sprintf(verdict, 2212), fmt.Sprintf(verdict, 2393), false},
		{"directive to verdict is a changed reason", fmt.Sprintf(directive, 644), fmt.Sprintf(verdict, 1850), true},
		{"directive to auto-nudge is a changed reason", fmt.Sprintf(directive, 644), fmt.Sprintf(nudge, 1850), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			from := "worker:feat/x#s1-1"
			second := status(2000, from, "blocked", tc.second, "")
			writeLog(t, p, status(1000, from, "blocked", tc.first, ""), second)
			stdout, _, code, _ := run(t, p, Request{Since: since("1000"), Timeout: "1", Interval: "1"})
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			quiet := ""
			if tc.wakes {
				quiet = want(t, 2000, second)
			}
			if stdout != quiet {
				t.Errorf("stdout = %q, want %q", stdout, quiet)
			}
		})
	}
}

// A first `unread:` wakes and the restamped age does not: with since 0 the
// dispatcher gets exactly one event, the first post.
func TestFirstUnreadWakesRestampQuiet(t *testing.T) {
	p := fixture(t)
	from := "worker:feat/x#s1-1"
	writeLog(t, p,
		status(1000, from, "blocked", fmt.Sprintf("unread: dispatcher directive undelivered for %ds — lead is working but has not reached a peek seam (long stage or idle on a background task)", 644), ""),
		status(2000, from, "blocked", fmt.Sprintf("unread: dispatcher directive undelivered for %ds — lead is working but has not reached a peek seam (long stage or idle on a background task)", 1850), ""))
	stdout, _, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if n := strings.Count(stdout, `"state":"blocked"`); n != 1 {
		t.Errorf("blocked events = %d, want 1 (%s)", n, stdout)
	}
	if !strings.Contains(stdout, `"ts":1000`) {
		t.Errorf("the first unread must be the wake (%s)", stdout)
	}
}

// genBus is the equivalence fixture: a deterministic mixed-session bus that
// exercises every suppression case — first-terminal exited, repeated
// blocked with cycle and awaited noise, blocked after working with the
// same detail, watchdog `… cleared`, msgs to me/*/other, sessions that tie
// on a millisecond, sessions that share a name across crews, rows of other
// kinds, and a clock that steps backwards (the legacy program rescanned the
// whole log and never trusted order, so the single pass may not either).
// It never emits the one row whose behavior #910 changed: an `unread:`
// blocked re-posted with a different age.
func genBus(seed int64, n int) []string {
	rng := rand.New(rand.NewSource(seed))
	sessions := map[string][]string{
		"c1": {"worker:feat/a#s1-1", "worker:feat/a#s2-2", "worker:feat/b#s1-1", "dispatcher:c1", "role:feat/a:reviewer", "worker:feat/c#s3-3"},
		"c2": {"worker:feat/a#s1-1", "worker:feat/d#s1-1", "dispatcher:c2"},
		"c3": {"worker:feat/e#s1-1", "dispatcher:c3"},
	}
	crews := []string{"c1", "c2", "c3"}
	plain := []string{"need a waiver: fast gate red", "plan-shaped gate rework: flaky bats", ""}
	other := []string{"done", "failed", "pr_open", "pending"}
	rows := make([]string, 0, n)
	ts := int64(1700000000000)
	for i := 0; i < n; i++ {
		crew := crews[rng.Intn(len(crews))]
		pool := sessions[crew]
		from := pool[rng.Intn(len(pool))]
		switch d := rng.Intn(100); {
		case d < 15:
			rows = append(rows, statusRow(crew, from, "blocked", plain[rng.Intn(len(plain))], "", ts))
		case d < 29:
			rows = append(rows, statusRow(crew, from, "blocked",
				fmt.Sprintf("need a waiver — awaited %ds, no reply (cycle %d of 24)", 298+rng.Intn(6), 1+rng.Intn(24)), "", ts))
		case d < 38:
			rows = append(rows, statusRow(crew, from, "blocked",
				fmt.Sprintf("question %d (cycle %d of 24)", rng.Intn(50), 1+rng.Intn(24)), "", ts))
		case d < 42:
			rows = append(rows, statusRow(crew, from, "blocked", "", "", ts))
		case d < 45:
			rows = append(rows, rawRow(ts, crew, from, "dispatcher:"+crew, "status", `{"state":"blocked","detail":{"k":true}}`))
		case d < 59:
			rows = append(rows, statusRow(crew, from, "working", "", "", ts))
		case d < 63:
			rows = append(rows, statusRow(crew, from, "working", "load: cleared", "watchdog", ts))
		case d < 65:
			rows = append(rows, statusRow(crew, from, "working", "turn-stall: cleared\n", "watchdog", ts))
		case d < 67:
			rows = append(rows, statusRow(crew, from, "blocked", "prompt: option-select frame", "watchdog", ts))
		case d < 73:
			rows = append(rows, statusRow(crew, from, "exited", "", "", ts))
		case d < 80:
			rows = append(rows, statusRow(crew, from, other[rng.Intn(len(other))], "", "", ts))
		case d < 92:
			rows = append(rows, msgRow(ts, crew, from, "dispatcher:"+crew))
		case d < 96:
			rows = append(rows, msgRow(ts, crew, from, "*"))
		case d < 99:
			rows = append(rows, msgRow(ts, crew, from, "worker:feat/a#s1-1"))
		default:
			rows = append(rows, rawRow(ts, crew, from, "dispatcher:"+crew, "start", `{"session":"s1","kind":"dispatch"}`))
		}
		if rng.Intn(100) < 12 {
			// ts tie: the same millisecond as the row before it.
		} else {
			ts += int64(rng.Intn(300))
		}
		if i%400 == 399 {
			ts -= 250 // a clock that steps back; the fold may not trust order
		}
	}
	return rows
}

func rawRow(ts int64, crew, from, to, kind, body string) string {
	return `{"ts":` + itoa(ts) + `,"crew_id":` + strconv.Quote(crew) + `,"from":` + strconv.Quote(from) +
		`,"to":` + strconv.Quote(to) + `,"kind":` + strconv.Quote(kind) + `,"body":` + body + "}"
}

func statusRow(crew, from, state, detail, source string, ts int64) string {
	body := `{"state":` + strconv.Quote(state) + "}"
	if detail != "" {
		body = strings.TrimSuffix(body, "}") + `,"detail":` + strconv.Quote(detail) + "}"
	}
	if source != "" {
		body = strings.TrimSuffix(body, "}") + `,"source":` + strconv.Quote(source) + "}"
	}
	return rawRow(ts, crew, from, "dispatcher:"+crew, "status", body)
}

func msgRow(ts int64, crew, from, to string) string {
	return rawRow(ts, crew, from, to, "msg", `"{\"verdict\":\"accept\"}"`)
}

// legacyProgram reads the frozen pre-#910 fold, the equivalence oracle.
func legacyProgram(t testing.TB) string {
	t.Helper()
	b, err := os.ReadFile("testdata/watch-legacy.jq")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// foldWith runs one program the way fold does — same bindings, and the same
// "no value means poll again" outcome — and returns the batch line.
func foldWith(prog string, rows []any, since int64, states []string) (string, bool) {
	sv := make([]any, len(states))
	for i, s := range states {
		sv[i] = s
	}
	res, err := jqrun.RunAny(prog, rows, 0, map[string]any{
		"crew":   "c1",
		"me":     "dispatcher:c1",
		"since":  json.Number(strconv.FormatInt(since, 10)),
		"states": sv,
	})
	if err != nil {
		return "", false
	}
	out, ok := res.(map[string]any)
	if !ok {
		return "", false
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "encode error: " + err.Error(), true
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}

func parseRows(t testing.TB, rows []string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(strings.Join(rows, "\n")))
	dec.UseNumber()
	var vs []any
	for {
		var v any
		err := dec.Decode(&v)
		if err == io.EOF {
			return vs
		}
		if err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
}

type genRow struct {
	Ts   int64           `json:"ts"`
	Crew string          `json:"crew_id"`
	From string          `json:"from"`
	To   string          `json:"to"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// assertSuppressionCases fails the equivalence run when the generated bus
// never reached one of the selection rules it is meant to cover — the fold
// agreeing on cases it never saw would prove nothing.
func assertSuppressionCases(t *testing.T, rows []string) {
	t.Helper()
	hit := map[string]bool{}
	type hist struct {
		states  [2]string // [previous, last]
		details [2]string
	}
	sess := map[string]*hist{}
	var lastTS int64
	for _, r := range rows {
		var g genRow
		if err := json.Unmarshal([]byte(r), &g); err != nil {
			t.Fatal(err)
		}
		var body struct {
			State  string          `json:"state"`
			Detail json.RawMessage `json:"detail"`
			Source string          `json:"source"`
		}
		if g.Kind == "status" {
			if err := json.Unmarshal(g.Body, &body); err != nil {
				t.Fatal(err)
			}
		}
		detail := string(body.Detail)
		state := body.State
		if g.Kind == "status" && state == "blocked" && body.Detail == nil {
			// A blocked with no detail field is its own coverage case.
			hit["blocked with no detail"] = true
		}
		switch {
		case g.Kind == "msg" && g.To == "dispatcher:c1":
			hit["msg to dispatcher"] = true
		case g.Kind == "msg" && g.To == "*":
			hit["msg to *"] = true
		case g.Kind == "start":
			hit["row of another kind"] = true
		case g.Kind == "status" && state == "blocked" && len(body.Detail) > 0 && body.Detail[0] == '{':
			hit["object blocked detail"] = true
		case g.Kind == "status" && state == "blocked" && body.Source == "watchdog":
			hit["watchdog blocked"] = true
		case g.Kind == "status" && state == "working" && body.Source == "watchdog" &&
			(strings.HasSuffix(detail, `cleared"`) || strings.HasSuffix(detail, `cleared\n"`)):
			hit["watchdog cleared working"] = true
		}
		if g.Ts == lastTS {
			hit["millisecond ts tie"] = true
		}
		if g.Ts < lastTS {
			hit["backwards clock step"] = true
		}
		lastTS = g.Ts
		if g.Kind != "status" {
			continue
		}
		key := g.Crew + "|" + g.From
		h := sess[key]
		if h == nil {
			h = &hist{}
			sess[key] = h
		}
		switch {
		case h.states[1] == "blocked" && state == "blocked" && h.details[1] == detail && detail != "":
			hit["repeated blocked, same detail"] = true
		case h.states[1] == "working" && h.states[0] == "blocked" && h.details[0] == detail:
			hit["blocked after working, same detail"] = true
		case h.states[1] == "working" && state == "exited":
			hit["exited after working"] = true
		case (h.states[1] == "done" || h.states[1] == "failed") && state == "exited":
			hit["exited after terminal"] = true
		}
		h.states[0], h.states[1] = h.states[1], state
		h.details[0], h.details[1] = h.details[1], detail
	}
	for _, w := range []string{
		"msg to dispatcher", "msg to *", "row of another kind",
		"object blocked detail", "watchdog blocked", "watchdog cleared working",
		"repeated blocked, same detail", "blocked after working, same detail",
		"exited after working", "exited after terminal", "blocked with no detail",
		"millisecond ts tie", "backwards clock step",
	} {
		if !hit[w] {
			t.Errorf("generated bus never produced: %s", w)
		}
	}
}

func TestFoldEquivalentToLegacyProgram(t *testing.T) {
	rows := genBus(910, 6000)
	if len(rows) < 5000 {
		t.Fatalf("generator produced %d rows", len(rows))
	}
	for _, r := range rows {
		if strings.Contains(r, "undelivered for") {
			t.Fatal("the equivalence bus may not contain the row whose behavior changed")
		}
	}
	vs := parseRows(t, rows)
	assertSuppressionCases(t, rows)
	legacy := legacyProgram(t)
	def := []string{"blocked", "pr_open", "done", "failed", "exited"}
	for _, bind := range []struct {
		since  int64
		states []string
	}{
		{0, def},
		{1700000450000, def},
		{0, []string{"blocked", "working", "exited", "done", "failed", "pr_open", "pending"}},
		{1700000600000, []string{"exited", "done"}},
		{1800000000000, []string{"blocked"}},
	} {
		wantLine, wantOK := foldWith(legacy, vs, bind.since, bind.states)
		gotLine, gotOK := foldWith(program, vs, bind.since, bind.states)
		name := fmt.Sprintf("since %d states %v", bind.since, bind.states)
		if wantOK != gotOK {
			t.Errorf("%s: legacy ok=%v, new ok=%v", name, wantOK, gotOK)
			continue
		}
		if wantOK && wantLine != gotLine {
			t.Errorf("%s: batches differ\n legacy: %.400s\n    new: %.400s", name, wantLine, gotLine)
		}
		if wantOK && wantLine != "" {
			t.Logf("%s: %d bytes identical", name, len(wantLine))
		}
	}
}

// The #910 poll cost, measured at the size the issue cites: 20k rows, one
// fold each. `go test -bench 'Fold' -benchtime 3x`.
func benchFoldPrograms(b *testing.B, prog string, rows []any) {
	states := []string{"blocked", "pr_open", "done", "failed", "exited"}
	sv := make([]any, len(states))
	for i, s := range states {
		sv[i] = s
	}
	vars := map[string]any{
		"crew":   "c1",
		"me":     "dispatcher:c1",
		"since":  json.Number("0"),
		"states": sv,
	}
	for b.Loop() {
		if _, err := jqrun.RunAny(prog, rows, 0, vars); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFoldLegacy20k(b *testing.B) {
	benchFoldPrograms(b, legacyProgram(b), parseRows(b, genBus(911, 20000)))
}

func BenchmarkFoldSinglePass20k(b *testing.B) {
	benchFoldPrograms(b, program, parseRows(b, genBus(911, 20000)))
}

// A malformed session key on the shared bus must not throw the fold down:
// jqrun reads a throw as "poll again", so one such row would blind every
// watch of every crew. The fold skips non-string crew_id/from when building
// its maps and answers null for them; the legacy program tolerated them
// through plain equality. (Other malformed shapes — a bare scalar row, an
// own-crew non-object body — still throw here, exactly as in the arm.)
func TestMalformedSessionKeysDoNotThrowTheFold(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []string
	}{
		{"foreign numeric crew_id", []string{
			`{"ts":1,"crew_id":7,"from":"x","kind":"status","body":{"state":"working"},"to":"dispatcher:c1"}`,
			status(2000, "worker:feat/x#s1-1", "done", "", ""),
		}},
		{"null crew_id", []string{
			`{"ts":1,"crew_id":null,"from":"x","kind":"status","body":{"state":"blocked"},"to":"dispatcher:c1"}`,
			status(2000, "worker:feat/x#s1-1", "done", "", ""),
		}},
		{"numeric from on this crew", []string{
			`{"ts":1,"crew_id":"c1","from":7,"kind":"status","body":{"state":"blocked"},"to":"dispatcher:c1"}`,
			`{"ts":2000,"crew_id":"c1","from":7,"kind":"status","body":{"state":"blocked"},"to":"dispatcher:c1"}`,
		}},
		{"null from on this crew", []string{
			`{"ts":1,"crew_id":"c1","kind":"status","body":{"state":"blocked"},"to":"dispatcher:c1"}`,
			status(2000, "worker:feat/x#s1-1", "done", "", ""),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.rows...)
			stdout, _, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			if !strings.Contains(stdout, `"events"`) {
				t.Errorf("the fold must answer, not poll again (%s)", stdout)
			}
		})
	}
}
