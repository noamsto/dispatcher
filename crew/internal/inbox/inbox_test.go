package inbox

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

// fixture is a bus without git: Run reads bus.Paths, so a temp dir stands in
// for the common dir, and marks writes under it as $dir/await does.
func fixture(t *testing.T) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	return bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
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

// run is the arm with the crew default stubbed: a test that wants the default
// passes an empty crew in args, one that does not passes it explicitly.
func run(t *testing.T, p bus.Paths, args ...string) (string, string, int) {
	t.Helper()
	var out, errB bytes.Buffer
	code := Run(args, p, &out, &errB, Options{CrewID: func() string { return "c1" }})
	return out.String(), errB.String(), code
}

// readMarks is what the bash readers see after a run.
func readMarks(t *testing.T, p bus.Paths, crew, me string) string {
	t.Helper()
	b, err := os.ReadFile(marks.Path(p.Dir, crew, me))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

const (
	// The rows below are printed with `jq -c`, so their expected output is the
	// row itself: jq keeps key order and a literal it can hold exactly.
	msgRow     = `{"ts":1785951264000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"worker:feat/x#s1-1","body":{"text":"go"}}`
	broadcast  = `{"ts":1785951265000,"crew_id":"c1","kind":"msg","from":"role:feat/x:reviewer","to":"*","body":{"text":"all"}}`
	otherTo    = `{"ts":1785951266000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"worker:feat/x#s2-2","body":{"text":"not yours"}}`
	statusRow  = `{"ts":1785951267000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`
	otherCrew  = `{"ts":1785951268000,"crew_id":"c2","kind":"msg","from":"dispatcher:c2","to":"worker:feat/x#s1-1","body":{"text":"other crew"}}`
	metricsRow = `{"ts":1785951269000,"crew_id":"c1","kind":"msg","from":"worker:feat/x#s1-1","to":"metrics:c1","body":{"tier":"deep"}}`
	me         = "worker:feat/x#s1-1"
)

func TestSelectsOnlyThisAgentMsgs(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, strings.Join([]string{msgRow, broadcast, otherTo, statusRow, otherCrew, metricsRow}, "\n")+"\n")

	stdout, stderr, code := run(t, p, me, "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if want := msgRow + "\n" + broadcast + "\n"; stdout != want {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want)
	}
}

// The expected lines are `jq -c 'select(...)'` on the same rows: a duplicate
// key collapses to its last value in place, 1e3 becomes 1E+3, and raw UTF-8
// and jq's escapes round-trip.
func TestPrintsTheRowUnchanged(t *testing.T) {
	const (
		row  = `{"crew_id":"c1","kind":"msg","to":"me","from":"a","n":1.50,"e":1e3,"big":18446744073709551617,"crew_id":"c1","u":"héllo 中文 🚀 tab\there <b> & \"q\" \u007f","z":-0.0}`
		want = `{"crew_id":"c1","kind":"msg","to":"me","from":"a","n":1.50,"e":1E+3,"big":18446744073709551617,"u":"héllo 中文 🚀 tab\there <b> & \"q\" \u007f","z":-0.0}`
	)
	p := fixture(t)
	writeLog(t, p, row+"\n")

	stdout, stderr, code := run(t, p, "me", "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if stdout != want+"\n" {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want+"\n")
	}
}

func TestSince(t *testing.T) {
	// Each row is one `ts` shape against `--since 100`; the last column is what
	// `jq -c '... and .ts > $since'` prints for it ("" for nothing).
	for _, tc := range []struct{ name, ts, since, want string }{
		{"above", `101`, "100", msgRow},
		{"boundary is excluded", `100`, "100", ""},
		{"below", `99`, "100", ""},
		{"zero since keeps a real ts", `1785951264000`, "0", msgRow},
		{"leading zeros are a number", `101`, "000100", msgRow},
		{"zero with leading zeros", `0`, "000", ""},
		{"bignum ts beats a small since", `18446744073709551617`, "18446744073709551616", msgRow},
		{"bignum ts loses to itself", `18446744073709551617`, "18446744073709551617", ""},
		{"fraction past a double's digits", `1.0000000000000000000000000000000000001`, "1", msgRow},
		{"huge exponent", `1e999999999`, "5", msgRow},
		{"negative exponent", `1e-3`, "0", msgRow},
		{"negative ts", `-5`, "0", ""},
		{"string ts outranks a number", `"zz"`, "100", msgRow},
		{"array ts outranks a number", `[1]`, "100", msgRow},
		{"object ts outranks a number", `{"a":1}`, "100", msgRow},
		{"null ts never", `null`, "100", ""},
		{"true never", `true`, "100", ""},
		{"false never", `false`, "100", ""},
		{"NaN never", `NaN`, "100", ""},
		{"Infinity always", `Infinity`, "100", msgRow},
		{"since past a double", `1785951264000`, `99999999999999999999999999999999`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := `{"ts":` + tc.ts + `,"crew_id":"c1","kind":"msg","from":"a","to":"me"}`
			p := fixture(t)
			writeLog(t, p, row+"\n")

			stdout, stderr, code := run(t, p, "me", "c1", "--since", tc.since)
			if code != 0 || stderr != "" {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if tc.want == "" {
				if stdout != "" {
					t.Errorf("stdout = %q, want nothing", stdout)
				}
				return
			}
			if !strings.Contains(stdout, `"ts":`) {
				t.Fatalf("stdout = %q", stdout)
			}
		})
	}
}

func TestSinceArgumentOrder(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	for _, args := range [][]string{{me, "c1", "--since", "0"}, {me, "--since", "0", "c1"}, {me, "--since", "0"}} {
		stdout, stderr, code := run(t, p, args...)
		if code != 0 || stderr != "" || stdout != msgRow+"\n" {
			t.Errorf("args %v: code %d stderr %q stdout %q", args, code, stderr, stdout)
		}
	}
}

// The arm's last positional and last --since win.
func TestLastArgumentWins(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n"+otherCrew+"\n")

	stdout, _, code := run(t, p, me, "c9", "c1", "--since", "5", "--since", "0")
	if code != 0 || stdout != msgRow+"\n" {
		t.Fatalf("code %d stdout %q", code, stdout)
	}
}

func TestCrewDefaults(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n"+otherCrew+"\n")

	if got, _, _ := run(t, p, me); got != msgRow+"\n" {
		t.Errorf("defaulted crew = %q", got)
	}
	// An explicit empty crew is still the arm's `${crew:-$(_crew_id)}`.
	if got, _, _ := run(t, p, me, ""); got != msgRow+"\n" {
		t.Errorf("empty crew = %q", got)
	}
}

// A crew with nothing to default to asks for `""`, which only the empty string
// matches: a null, number, bool, array or object crew_id matches no crew
// (#874's HIGH).
func TestEmptyCrewMatchesNoCrewValue(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1,"crew_id":null,"kind":"msg","to":"me","from":"a"}`+"\n"+
		`{"ts":2,"crew_id":"","kind":"msg","to":"me","from":"b"}`+"\n"+
		`{"ts":3,"crew_id":5,"kind":"msg","to":"me","from":"c"}`+"\n")

	var out, errB bytes.Buffer
	empty := Run([]string{"me"}, p, &out, &errB, Options{CrewID: func() string { return "" }})
	if empty != 0 || errB.String() != "" {
		t.Fatalf("code %d stderr %q", empty, errB.String())
	}
	got := out.String()
	if !strings.Contains(got, `"from":"b"`) || strings.Contains(got, `"from":"a"`) || strings.Contains(got, `"from":"c"`) {
		t.Errorf("stdout = %q", got)
	}
}

func TestArgumentErrors(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	for _, tc := range []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{
			name:       "branch-only worker id",
			args:       []string{"worker:feat/x", "c1"},
			wantCode:   1,
			wantStderr: "crew: inbox: 'worker:feat/x' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message\n",
		},
		{
			name:       "worker id whose suffix is not a session",
			args:       []string{"worker:feat/x#s1", "c1"},
			wantCode:   1,
			wantStderr: "crew: inbox: 'worker:feat/x#s1' has no session suffix",
		},
		{
			name:       "--since without a value",
			args:       []string{me, "c1", "--since"},
			wantCode:   1,
			wantStderr: "crew: --since needs a value\n",
		},
		{
			name:       "--since with an empty value",
			args:       []string{me, "c1", "--since", ""},
			wantCode:   1,
			wantStderr: "crew: --since needs a value\n",
		},
		{
			name:       "--since not an integer",
			args:       []string{me, "c1", "--since", "1785951264000x"},
			wantCode:   1,
			wantStderr: "crew: --since must be an integer ms timestamp\n",
		},
		{
			name:       "--since signed",
			args:       []string{me, "c1", "--since", "-1"},
			wantCode:   1,
			wantStderr: "crew: --since must be an integer ms timestamp\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := run(t, p, tc.args...)
			if code != tc.wantCode {
				t.Fatalf("code %d, want %d (stderr %q)", code, tc.wantCode, stderr)
			}
			if !strings.HasPrefix(stderr, tc.wantStderr) {
				t.Errorf("stderr %q, want %q", stderr, tc.wantStderr)
			}
			if stdout != "" {
				t.Errorf("stdout %q, want nothing", stdout)
			}
		})
	}
}

// A session id is the text after the last `#`, so a branch that carries one
// still resolves.
func TestSessionIDSuffix(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	for _, id := range []string{"worker:feat/12-a#b#s1234567890-12345", "worker:x#s0-0", "worker:#s1-1"} {
		if _, stderr, code := run(t, p, id, "c1"); code != 0 || stderr != "" {
			t.Errorf("%s: code %d stderr %q", id, code, stderr)
		}
	}
}

// `crew inbox` with no agent is the arm's `${1:-}`: no msgs, no error, and no
// marks to write.
func TestNoArguments(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	stdout, stderr, code := run(t, p)
	if code != 0 || stderr != "" || stdout != "" {
		t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
	}
	if got := readMarks(t, p, "c1", ""); got != "" {
		t.Errorf("marks = %q", got)
	}
}

func TestAbsentLogIsSilent(t *testing.T) {
	t.Run("no log at all", func(t *testing.T) {
		p := fixture(t)
		var out, errB bytes.Buffer
		code := Run([]string{me, "c1"}, p, &out, &errB, Options{JQColorsInvalid: true, CrewID: func() string { return "c1" }})
		if out.Len() != 0 || errB.String() != "" || code != 0 {
			t.Fatalf("code %d stderr %q", code, errB.String())
		}
	})

	// The arm tests `-f "$log"` before it looks at --since, so a bad TS is not
	// an error on a repo that has never posted.
	t.Run("a bad --since is not an error without a log", func(t *testing.T) {
		p := fixture(t)
		stdout, stderr, code := run(t, p, me, "c1", "--since", "abc")
		if stdout != "" || stderr != "" || code != 0 {
			t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
		}
	})

	// ... but the value check happens while parsing, before the log is opened.
	t.Run("--since without a value is an error without a log", func(t *testing.T) {
		p := fixture(t)
		_, stderr, code := run(t, p, me, "c1", "--since")
		if code != 1 || stderr != "crew: --since needs a value\n" {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
	})

	t.Run("log is a directory", func(t *testing.T) {
		p := fixture(t)
		if err := os.MkdirAll(p.Log, 0o755); err != nil {
			t.Fatal(err)
		}
		if stdout, stderr, code := run(t, p, me, "c1"); stdout != "" || stderr != "" || code != 0 {
			t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
		}
	})
}

// A torn trailing line costs the parse but not the msgs above it, and the marks
// they raised — the arm recorded whatever jq had printed before it failed.
func TestTornTailKeepsThePrefixAndExitsFive(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n"+`{"ts":1785951265000,"crew_id":"c1","kind":"msg","from":"a","to":"me`+"\n")

	stdout, stderr, code := run(t, p, me, "c1")
	if code != 5 {
		t.Fatalf("code %d, want 5", code)
	}
	if stdout != msgRow+"\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "crew: inbox: "+p.Log+":") {
		t.Errorf("stderr = %q", stderr)
	}
	if got := readMarks(t, p, "c1", me); !strings.Contains(got, `"dispatcher:c1":1785951264000`) {
		t.Errorf("marks = %q", got)
	}
}

// jq indexes each row and keeps going after a row it cannot use: one line
// each, and the exit status follows the *last* input, not a sticky flag.
func TestNonObjectRowsFollowTheLastInput(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		wantCode         int
	}{
		{"number row last", msgRow + "\n5\n", "cannot index number", 5},
		{"number row first", "5\n" + msgRow + "\n", "cannot index number", 0},
		{"string row", msgRow + "\n\"s\"\n", "cannot index string", 5},
		{"array row", msgRow + "\n[]\n", "cannot index array", 5},
		{"bool row last", msgRow + "\ntrue\n", "cannot index boolean", 5},
		{"null row never fails", msgRow + "\nnull\n", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.body)
			stdout, stderr, code := run(t, p, me, "c1")
			if code != tc.wantCode {
				t.Fatalf("code %d, want %d (stderr %q)", code, tc.wantCode, stderr)
			}
			if stdout != msgRow+"\n" {
				t.Errorf("stdout = %q", stdout)
			}
			if tc.want == "" {
				if stderr != "" {
					t.Errorf("stderr = %q, want nothing", stderr)
				}
				return
			}
			if strings.Count(stderr, "crew: inbox: "+p.Log+": "+tc.want) != 1 {
				t.Errorf("stderr = %q, want one %q", stderr, tc.want)
			}
		})
	}
}

// The marks are the point of the arm: reading a msg is what makes it delivered.
func TestRecordsMarksForAWorker(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, strings.Join([]string{msgRow, broadcast, otherTo, statusRow}, "\n")+"\n")

	_, stderr, code := run(t, p, me, "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	want := `{"dispatcher:c1":1785951264000,"role:feat/x:reviewer":1785951265000}`
	if got := readMarks(t, p, "c1", me); testjson.Compact(testjson.MustParse(t, got)) != want {
		t.Errorf("marks = %s, want %s", got, want)
	}
	// Only the printed msgs, and only for the session that read them.
	if got := readMarks(t, p, "c1", "worker:feat/x#s2-2"); got != "" {
		t.Errorf("other session's marks = %q", got)
	}
	if got := readMarks(t, p, "c2", me); got != "" {
		t.Errorf("other crew's marks = %q", got)
	}
}

// A non-worker caller (a dispatcher, a role) has no delivered marks to raise,
// and an empty inbox writes nothing at all.
func TestNoMarksForOthersOrForNothing(t *testing.T) {
	for _, tc := range []struct{ name, me, body string }{
		{"dispatcher", "dispatcher:c1", msgRow + "\n"},
		{"role", "role:feat/x:reviewer", msgRow + "\n"},
		{"worker with nothing to read", me, otherCrew + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.body)
			if _, stderr, code := run(t, p, tc.me, "c1"); code != 0 || stderr != "" {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if got := readMarks(t, p, "c1", tc.me); got != "" {
				t.Errorf("marks = %q", got)
			}
			if left, _ := filepath.Glob(p.Dir + "/await/*"); len(left) != 0 {
				t.Errorf("await dir holds %v", left)
			}
		})
	}
}

// A marks file the arm could not merge (a msg with no `from`) leaves whatever
// was there untouched, and the msgs still print.
func TestUnmergeableMsgsStillPrintAndWriteNothing(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1,"crew_id":"c1","kind":"msg","to":"worker:feat/x#s1-1","body":{"text":"no sender"}}`+"\n")

	stdout, stderr, code := run(t, p, me, "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "no sender") {
		t.Errorf("stdout = %q", stdout)
	}
	if got := readMarks(t, p, "c1", me); got != "" {
		t.Errorf("marks = %q", got)
	}
}

// jq started inside `$(...)`, so it warned about a bad $JQ_COLORS but never
// had a terminal to colourise.
func TestJQColorsWarning(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	var out, errB bytes.Buffer
	code := Run([]string{me, "c1"}, p, &out, &errB, Options{JQColorsInvalid: true, CrewID: func() string { return "c1" }})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if errB.String() != "Failed to set $JQ_COLORS\n" {
		t.Errorf("stderr = %q", errB.String())
	}
	if out.String() != msgRow+"\n" {
		t.Errorf("stdout = %q, want the row uncoloured", out.String())
	}

	// The check that exits before jq starts prints no warning.
	var out2, err2 bytes.Buffer
	if code := Run([]string{me, "c1", "--since", "x"}, p, &out2, &err2, Options{JQColorsInvalid: true, CrewID: func() string { return "c1" }}); code != 1 ||
		strings.Contains(err2.String(), "JQ_COLORS") {
		t.Errorf("code %d stderr %q", code, err2.String())
	}
}

// The arm printed with `printf` under `set -e`, so a write that fails ends it
// before the marks are raised: msgs the caller never received must stay
// undelivered, or the next await skips them for good.
func TestAFailedPrintRecordsNoMarks(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	var out, errB bytes.Buffer
	code := Run([]string{me, "c1"}, p, &out, &errB, Options{
		CrewID: func() string { return "c1" },
		Flush:  func() error { return errors.New("no space left on device") },
	})
	if code != 1 {
		t.Fatalf("code %d, want 1", code)
	}
	if got := readMarks(t, p, "c1", me); got != "" {
		t.Errorf("marks = %q, want none", got)
	}
	if left, _ := filepath.Glob(p.Dir + "/await/*"); len(left) != 0 {
		t.Errorf("await dir holds %v", left)
	}
}

// The other half of the ordering: the rows are on the wire before the marks
// exist, so a reader that never got them can still get them from the next read.
func TestPrintComesBeforeTheMarks(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	var out, errB bytes.Buffer
	w := &flushProbe{buf: &out, marksSeen: func() bool { return readMarks(t, p, "c1", me) != "" }}
	code := Run([]string{me, "c1"}, p, w, &errB, Options{
		CrewID: func() string { return "c1" },
		Flush:  w.flush,
	})
	if code != 0 || errB.Len() != 0 {
		t.Fatalf("code %d stderr %q", code, errB.String())
	}
	if !w.flushed {
		t.Fatal("Run never flushed")
	}
	if w.marksAtFlush == nil || *w.marksAtFlush {
		t.Error("marks were raised before the rows were on the wire")
	}
	if got := readMarks(t, p, "c1", me); got == "" {
		t.Error("marks never raised")
	}
	if out.String() != msgRow+"\n" {
		t.Errorf("stdout = %q", out.String())
	}
}

// flushProbe records whether the marks file existed when the flush ran.
type flushProbe struct {
	buf          *bytes.Buffer
	flushed      bool
	marksAtFlush *bool
	marksSeen    func() bool
}

func (f *flushProbe) Write(p []byte) (int, error) { return f.buf.Write(p) }

func (f *flushProbe) flush() error {
	onWire := f.marksSeen()
	f.marksAtFlush = &onWire
	f.flushed = true
	return nil
}

// TestDecimalGreater pins the literal compare against `jq -nc --argjson a <a>
// --argjson b <b> '$a > $b'` answers, the cases a double cannot tell apart.
func TestDecimalGreater(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"5", "5", false},
		{"6", "5", true},
		{"5", "6", false},
		{"0", "0", false},
		{"0", "1", false},
		{"1", "0", true},
		{"0000", "0", false},
		{"0.5", "0", true},
		{"0.5", "1", false},
		{"-0.0", "0", false},
		{"-1", "0", false},
		{"1E+3", "999", true},
		{"1E+3", "1000", false},
		{"1e3", "1001", false},
		{"1.50", "1", true},
		{"1.50", "2", false},
		{"0.0012e3", "1", true},
		{"0.0012e3", "2", false},
		{"18446744073709551617", "18446744073709551616", true},
		{"18446744073709551616", "18446744073709551617", false},
		{"1.0000000000000000000000000000000000001", "1", true},
		{"1.0000000000000000000000000000000000000", "1", false},
		{"1e999999999", "5", true},
		{"1e-999999999", "5", false},
		{"1e999999999999999999999", "5", true},
		{"1e-999999999999999999999", "5", false},
		{"12345678901234567890", "12345678901234567891", false},
		{"9", "89999999999999999999", false},
		{"20", "99999999999999999999", false},
		{"999999999999999999991", "99999999999999999999", true},
	} {
		if got := decimalGreater(tc.a, tc.b); got != tc.want {
			t.Errorf("decimalGreater(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestGreaterThanNumber covers the kinds jq's total order decides without
// looking at a number, and the two values that carry no literal text.
func TestGreaterThanNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    jsonv.Value
		want bool
	}{
		{"string", jsonv.Str("0"), true},
		{"array", jsonv.Array(), true},
		{"object", jsonv.Object(), true},
		{"null", jsonv.Null(), false},
		{"false", jsonv.Bool(false), false},
		{"true", jsonv.Bool(true), false},
		{"NaN", testjson.MustParse(t, "NaN"), false},
		{"Infinity", testjson.MustParse(t, "Infinity"), true},
		{"-Infinity", testjson.MustParse(t, "-Infinity"), false},
	} {
		if got := greaterThanNumber(tc.v, "100"); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
