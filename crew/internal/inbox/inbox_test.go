package inbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/marks"
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

// request reads the tests' `agent [crew] [--since TS] [--from S] [--undelivered]`
// shorthand in any order; the argv grammar itself is main's (TestRunInbox).
func request(args ...string) Request {
	var q Request
	var words []string
	for len(args) > 0 {
		switch args[0] {
		case "--since":
			q.Since, args = args[1], args[2:]
		case "--from":
			q.From, args = args[1], args[2:]
		case "--undelivered":
			q.Undelivered, args = true, args[1:]
		default:
			words, args = append(words, args[0]), args[1:]
		}
	}
	if len(words) > 0 {
		q.Me = words[0]
	}
	if len(words) > 1 {
		q.Crew = words[1]
	}
	return q
}

// run is the arm with the crew default stubbed: a test that wants the default
// passes an empty crew in args, one that does not passes it explicitly.
func run(t *testing.T, p bus.Paths, args ...string) (string, string, int) {
	t.Helper()
	var out, errB bytes.Buffer
	code := Run(request(args...), p, &out, &errB, Options{CrewID: func() string { return "c1" }})
	return out.String(), errB.String(), code
}

// canon re-encodes each line with sorted keys: the rows print value-equal to
// the source, not in its key order.
func canon(s string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		if line == "" {
			continue
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return s
		}
		out, _ := json.Marshal(v)
		b.Write(out)
		b.WriteByte('\n')
	}
	return b.String()
}

// compactJSON is canon for one value without the trailing newline.
func compactJSON(s string) string { return strings.TrimSuffix(canon(s+"\n"), "\n") }

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
	// The session's own question: an outbound row is nobody's reply to read.
	asked = `{"ts":1785951269500,"crew_id":"c1","kind":"msg","from":"worker:feat/x#s1-1","to":"dispatcher:c1","body":{"text":"go"}}`
	me    = "worker:feat/x#s1-1"
)

func TestSelectsOnlyThisAgentMsgs(t *testing.T) {
	log := strings.Join([]string{msgRow, broadcast, otherTo, statusRow, otherCrew, metricsRow, asked}, "\n") + "\n"

	t.Run("this agent's msgs only", func(t *testing.T) {
		p := fixture(t)
		writeLog(t, p, log)

		stdout, stderr, code := run(t, p, me, "c1")
		if code != 0 || stderr != "" {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		if want := msgRow + "\n" + broadcast + "\n"; canon(stdout) != canon(want) {
			t.Errorf("stdout\n got %q\nwant %q", stdout, want)
		}
	})

	// The metrics sink is nobody else's inbox: reading as the dispatcher does not
	// take the row addressed to `metrics:c1`.
	t.Run("a metrics-addressed row is not the dispatcher's", func(t *testing.T) {
		p := fixture(t)
		writeLog(t, p, log)

		stdout, stderr, code := run(t, p, "dispatcher:c1", "c1")
		if code != 0 || stderr != "" {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		if want := broadcast + "\n" + asked + "\n"; canon(stdout) != canon(want) {
			t.Errorf("stdout\n got %q\nwant %q", stdout, want)
		}
	})
}

// The printed row is value-equal to what `jq -c 'select(...)'` prints for the
// same row: a duplicate key collapses to its last value, number literals keep
// their digits (1.50, the 20-digit integer, -0.0), and raw UTF-8 and the
// string escapes round-trip. Key order and the text of 1e3 (the bus reads it as 1E+3) are not pinned.
func TestPrintsTheRowUnchanged(t *testing.T) {
	const (
		row  = `{"crew_id":"c1","kind":"msg","to":"me","from":"a","n":1.50,"e":1E+3,"big":18446744073709551617,"crew_id":"c1","u":"héllo 中文 🚀 tab\there <b> & \"q\" \u007f","z":-0.0}`
		want = `{"crew_id":"c1","kind":"msg","to":"me","from":"a","n":1.50,"e":1E+3,"big":18446744073709551617,"u":"héllo 中文 🚀 tab\there <b> & \"q\" \u007f","z":-0.0}`
	)
	p := fixture(t)
	writeLog(t, p, row+"\n")

	stdout, stderr, code := run(t, p, "me", "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	decode := func(s string) any {
		var v any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode %q: %v", s, err)
		}
		return v
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") || !reflect.DeepEqual(decode(stdout), decode(want)) {
		t.Errorf("stdout\n got %q\nwant value-equal to %q", stdout, want)
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

func TestCrewDefaults(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n"+otherCrew+"\n")

	if got, _, _ := run(t, p, me); canon(got) != canon(msgRow+"\n") {
		t.Errorf("defaulted crew = %q", got)
	}
	// An explicit empty crew is still the arm's `${crew:-$(_crew_id)}`.
	if got, _, _ := run(t, p, me, ""); canon(got) != canon(msgRow+"\n") {
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
	empty := Run(request("me"), p, &out, &errB, Options{CrewID: func() string { return "" }})
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
		code := Run(request(me, "c1"), p, &out, &errB, Options{CrewID: func() string { return "c1" }})
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

// A torn trailing line costs the parse but not the msgs above it, or the marks
// they raised. The straggler fold (`--since`) reads the same prefix.
func TestTornTailKeepsThePrefixAndExitsFive(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plain read", nil},
		{"the straggler fold", []string{"--since", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, msgRow+"\n"+`{"ts":1785951265000,"crew_id":"c1","kind":"msg","from":"a","to":"me`+"\n")

			stdout, stderr, code := run(t, p, append([]string{me, "c1"}, tc.args...)...)
			if code != 5 {
				t.Fatalf("code %d, want 5", code)
			}
			if canon(stdout) != canon(msgRow+"\n") {
				t.Errorf("stdout = %q", stdout)
			}
			if !strings.Contains(stderr, "crew: inbox: "+p.Log+":") {
				t.Errorf("stderr = %q", stderr)
			}
			if got := readMarks(t, p, "c1", me); !strings.Contains(got, `"dispatcher:c1":1785951264000`) {
				t.Errorf("marks = %q", got)
			}
		})
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
			if canon(stdout) != canon(msgRow+"\n") {
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
	writeLog(t, p, strings.Join([]string{asked, msgRow, broadcast, otherTo, statusRow}, "\n")+"\n")

	_, stderr, code := run(t, p, me, "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	want := `{"dispatcher:c1":1785951264000,"role:feat/x:reviewer":1785951265000}`
	if got := readMarks(t, p, "c1", me); compactJSON(got) != want {
		t.Errorf("marks = %s, want %s", got, want)
	}
	// One file for the session, at marks.Path's path: `await`, `nudge` and
	// `stall-watch --unread` all glob await/ for it.
	files, err := filepath.Glob(p.Dir + "/await/*")
	if err != nil || len(files) != 1 || files[0] != marks.Path(p.Dir, "c1", me) {
		t.Errorf("await/ holds %v (%v), want only %s", files, err, marks.Path(p.Dir, "c1", me))
	}
	// Only the printed msgs, and only for the session that read them.
	if got := readMarks(t, p, "c1", "worker:feat/x#s2-2"); got != "" {
		t.Errorf("other session's marks = %q", got)
	}
	if got := readMarks(t, p, "c2", me); got != "" {
		t.Errorf("other crew's marks = %q", got)
	}
}

// A mark `await` raised is a ceiling, not a value to replace: the merge leaves
// that sender's mark where await left it while adding the sender it had not
// seen. `--since` ignores marks, so the older msgs print again.
func TestMarksAwaitWroteSurviveTheMerge(t *testing.T) {
	const (
		answer  = `{"ts":1785951264000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"worker:feat/x#s1-1","body":"answer"}`
		verdict = `{"ts":1785951270000,"crew_id":"c1","kind":"msg","from":"role:feat/x:reviewer","to":"worker:feat/x#s1-1","body":"verdict"}`
	)
	p := fixture(t)
	writeLog(t, p, answer+"\n"+verdict+"\n")
	// A ceiling above the row about to print: what `await` left after handing out
	// a newer reply from the same sender. marks.TestRecord pins the rule itself;
	// this is inbox's Go merge over the file await wrote.
	seedMarks(t, p, "c1", me, `{"dispatcher:c1":1785951264001}`)

	stdout, stderr, code := run(t, p, me, "c1", "--since", "0")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	// --since ignores the marks: both rows print again.
	if want := answer + "\n" + verdict + "\n"; canon(stdout) != canon(want) {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want)
	}
	want := `{"dispatcher:c1":1785951264001,"role:feat/x:reviewer":1785951270000}`
	if got := readMarks(t, p, "c1", me); compactJSON(got) != want {
		t.Errorf("marks = %s, want %s", got, want)
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

// The arm printed with `printf` under `set -e`, so a write that fails ends it
// before the marks are raised: msgs the caller never received must stay
// undelivered, or the next await skips them for good.
func TestAFailedPrintRecordsNoMarks(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	var out, errB bytes.Buffer
	code := Run(request(me, "c1"), p, &out, &errB, Options{
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
	code := Run(request(me, "c1"), p, w, &errB, Options{
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
	if canon(out.String()) != canon(msgRow+"\n") {
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
// looking at a number.
func TestGreaterThanNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want bool
	}{
		{"string", "0", true},
		{"array", []any{}, true},
		{"object", map[string]any{}, true},
		{"null", nil, false},
		{"false", false, false},
		{"true", true, false},
	} {
		if got := greaterThanNumber(tc.v, "100"); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A marks file as `_await_record` leaves it, for a test that wants the session
// to start with something already delivered.
func seedMarks(t *testing.T, p bus.Paths, crew, who, body string) {
	t.Helper()
	path := marks.Path(p.Dir, crew, who)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFromKeepsOnlyThatSender(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, strings.Join([]string{msgRow, broadcast, otherTo, otherCrew}, "\n")+"\n")

	stdout, stderr, code := run(t, p, me, "c1", "--from", "dispatcher:c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if canon(stdout) != canon(msgRow+"\n") {
		t.Errorf("stdout = %q, want only %q", stdout, msgRow)
	}
	// Only what was printed is marked delivered, so the reviewer's broadcast is
	// still there for a later await.
	if got, want := readMarks(t, p, "c1", me), `{"dispatcher:c1":1785951264000}`; compactJSON(got) != want {
		t.Errorf("marks = %s, want %s", got, want)
	}
}

func TestFlagOrderIsFree(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, strings.Join([]string{msgRow, broadcast}, "\n")+"\n")

	stdout, stderr, code := run(t, p, me, "--undelivered", "--from", "dispatcher:c1", "--since", "0", "c1")
	if code != 0 || stderr != "" || canon(stdout) != canon(msgRow+"\n") {
		t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
	}
}

func TestUndeliveredPrintsEachMsgOnce(t *testing.T) {
	const newer = `{"ts":1785951270000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"worker:feat/x#s1-1","body":{"text":"stop"}}`
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	args := []string{me, "c1", "--from", "dispatcher:c1", "--undelivered"}
	if stdout, stderr, code := run(t, p, args...); code != 0 || stderr != "" || canon(stdout) != canon(msgRow+"\n") {
		t.Fatalf("first: code %d stderr %q stdout %q", code, stderr, stdout)
	}
	if stdout, stderr, code := run(t, p, args...); code != 0 || stderr != "" || stdout != "" {
		t.Fatalf("second: code %d stderr %q stdout %q", code, stderr, stdout)
	}

	writeLog(t, p, msgRow+"\n"+newer+"\n")
	if stdout, stderr, code := run(t, p, args...); code != 0 || stderr != "" || canon(stdout) != canon(newer+"\n") {
		t.Fatalf("third: code %d stderr %q stdout %q", code, stderr, stdout)
	}
	if got, want := readMarks(t, p, "c1", me), `{"dispatcher:c1":1785951270000}`; compactJSON(got) != want {
		t.Errorf("marks = %s, want %s", got, want)
	}
}

// What a plain inbox, await or nudge already handed this session is not
// undelivered, whichever of them wrote the mark.
func TestUndeliveredHonoursExistingMarks(t *testing.T) {
	for _, tc := range []struct{ name, marks, want string }{
		{"mark at the msg's ts", `{"dispatcher:c1":1785951264000}`, ""},
		{"mark past the msg", `{"dispatcher:c1":1785951264001}`, ""},
		{"mark before the msg", `{"dispatcher:c1":1785951263999}`, msgRow + "\n"},
		{"another sender's mark", `{"role:feat/x:reviewer":1785951999999}`, msgRow + "\n"},
		{"a string mark outranks a number ts", `{"dispatcher:c1":"zz"}`, ""},
		{"no marks file", ``, msgRow + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, msgRow+"\n")
			if tc.marks != "" {
				seedMarks(t, p, "c1", me, tc.marks)
			}

			stdout, stderr, code := run(t, p, me, "c1", "--from", "dispatcher:c1", "--undelivered")
			if code != 0 || stderr != "" || canon(stdout) != canon(tc.want) {
				t.Fatalf("code %d stderr %q stdout %q, want %q", code, stderr, stdout, tc.want)
			}
		})
	}
}

func TestUndeliveredAfterAPlainInbox(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n")

	if _, _, code := run(t, p, me, "c1"); code != 0 {
		t.Fatalf("plain inbox code %d", code)
	}
	if stdout, stderr, code := run(t, p, me, "c1", "--undelivered"); code != 0 || stderr != "" || stdout != "" {
		t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
	}
}

func TestUndeliveredWithoutFromCoversEverySender(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, strings.Join([]string{msgRow, broadcast, otherTo, otherCrew}, "\n")+"\n")
	seedMarks(t, p, "c1", me, `{"dispatcher:c1":1785951264000}`)

	stdout, stderr, code := run(t, p, me, "c1", "--undelivered")
	if code != 0 || stderr != "" || canon(stdout) != canon(broadcast+"\n") {
		t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
	}
	want := `{"dispatcher:c1":1785951264000,"role:feat/x:reviewer":1785951265000}`
	if got := readMarks(t, p, "c1", me); compactJSON(got) != want {
		t.Errorf("marks = %s, want %s", got, want)
	}

	// A fresh session sees every sender's msg.
	stdout, _, code = run(t, fixtureWith(t, msgRow, broadcast), me, "c1", "--undelivered")
	if code != 0 || canon(stdout) != canon(msgRow+"\n"+broadcast+"\n") {
		t.Errorf("fresh session: code %d stdout %q", code, stdout)
	}
}

func fixtureWith(t *testing.T, rows ...string) bus.Paths {
	t.Helper()
	p := fixture(t)
	writeLog(t, p, strings.Join(rows, "\n")+"\n")
	return p
}

// A msg with no sender string has no mark to compare with and would fail the
// whole marks write, so it is skipped and the others still print and record.
func TestUndeliveredSkipsMsgsWithoutAStringSender(t *testing.T) {
	for _, tc := range []struct{ name, from string }{
		{"null", `"from":null,`},
		{"number", `"from":7,`},
		{"absent", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := `{"ts":1785951263000,"crew_id":"c1","kind":"msg",` + tc.from + `"to":"worker:feat/x#s1-1","body":{"text":"anon"}}`
			p := fixtureWith(t, bad, msgRow)

			stdout, stderr, code := run(t, p, me, "c1", "--undelivered")
			if code != 0 || stderr != "" || canon(stdout) != canon(msgRow+"\n") {
				t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
			}
			if got, want := readMarks(t, p, "c1", me), `{"dispatcher:c1":1785951264000}`; compactJSON(got) != want {
				t.Errorf("marks = %s, want %s", got, want)
			}
		})
	}
}

// Sixteen readers of one session against one msg: the marks lock makes the
// read-print-record cycle atomic, so the msg is printed by exactly one of them.
func TestUndeliveredExactlyOnceUnderConcurrency(t *testing.T) {
	p := fixtureWith(t, msgRow)

	const readers = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		lines int
	)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out, errB bytes.Buffer
			code := Run(request(me, "c1", "--from", "dispatcher:c1", "--undelivered"), p, &out, &errB, Options{CrewID: func() string { return "c1" }})
			if code != 0 || errB.Len() != 0 {
				t.Errorf("code %d stderr %q", code, errB.String())
			}
			mu.Lock()
			lines += strings.Count(out.String(), "\n")
			mu.Unlock()
		}()
	}
	wg.Wait()

	if lines != 1 {
		t.Errorf("printed %d lines across %d readers, want 1", lines, readers)
	}
}

// The torn-tail exit is unchanged: the prefix is printed and recorded, then the
// parse failure is reported.
func TestUndeliveredTornTailKeepsThePrefix(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msgRow+"\n"+`{"ts":1785951270000,"crew_id":"c1","kind":"msg","fr`)

	stdout, stderr, code := run(t, p, me, "c1", "--undelivered")
	if code != exitType || canon(stdout) != canon(msgRow+"\n") || !strings.Contains(stderr, "crew: inbox: ") {
		t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
	}
	if got, want := readMarks(t, p, "c1", me), `{"dispatcher:c1":1785951264000}`; compactJSON(got) != want {
		t.Errorf("marks = %s, want %s", got, want)
	}
}

func TestUndeliveredLockFailurePrintsNothing(t *testing.T) {
	p := fixtureWith(t, msgRow)
	// A file where the lock directory belongs makes Lock's MkdirAll fail.
	if err := os.WriteFile(p.Dir+"/await.lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := run(t, p, me, "c1", "--undelivered")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "crew: inbox: cannot lock delivered marks: ") {
		t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
	}
	if got := readMarks(t, p, "c1", me); got != "" {
		t.Errorf("marks = %q", got)
	}
}

// A missing log still exits 0 before anything else, the lock included.
func TestUndeliveredWithoutALogIsSilent(t *testing.T) {
	p := fixture(t)
	stdout, stderr, code := run(t, p, me, "c1", "--undelivered")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("code %d stderr %q stdout %q", code, stderr, stdout)
	}
	if left, _ := filepath.Glob(p.Dir + "/await.lock/*"); len(left) != 0 {
		t.Errorf("lock dir holds %v", left)
	}
}
