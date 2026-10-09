package report

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

const wantHeader = "engine\tmodel\ttier\tshape\toutcome\tduration_s\n"

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

var fixedNow = func() time.Time { return time.Unix(1785951264, 0) }

func run(t *testing.T, p bus.Paths, crew string, o Options) (string, string, int) {
	t.Helper()
	if o.Now == nil {
		o.Now = fixedNow
	}
	var out, errB bytes.Buffer
	code := Run(crew, p, &out, &errB, o)
	return out.String(), errB.String(), code
}

// The expected tables below are `jq -s -r --arg crew c1 '<the arm program>'` on
// the same fixture, byte for byte (docs/crew-go-port.md's value-identity rule);
// the em dash is the fold's own "no value" column.
const dash = "—"

func TestRowJoinsDispatchToItsSessionStatuses(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1","engine":"claude","model":"sonnet","tier":"standard","shape":"ui"}
{"ts":2000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}
{"ts":65000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"done"}}
`)

	stdout, stderr, code := run(t, p, "c1", Options{})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if want := wantHeader + "claude\tsonnet\tstandard\tui\tdone\t63\n"; stdout != want {
		t.Errorf("stdout got %q, want %q", stdout, want)
	}
}

func TestRowsPerDispatchAndCrew(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1","engine":"claude","model":"sonnet","tier":"standard","shape":"ui"}
{"ts":1500,"crew_id":"c1","kind":"dispatch","branch":"feat/y","session":"s2-1","engine":"pi","model":"lemonade","tier":"deep"}
{"ts":2000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}
{"ts":9000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"pr_open"}}
{"ts":12000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"done"}}
{"ts":3000,"crew_id":"c2","kind":"status","from":"worker:feat/y#s2-1","body":{"state":"working"}}
{"ts":4000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"worker:feat/x#s1-1"}
`)

	stdout, stderr, code := run(t, p, "c1", Options{})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	want := wantHeader +
		"claude\tsonnet\tstandard\tui\tdone\t10\n" +
		"pi\tlemonade\tdeep\t" + dash + "\t" + dash + "\t" + dash + "\n"
	if stdout != want {
		t.Errorf("stdout got %q, want %q", stdout, want)
	}
}

// A dispatch with no branch match — no status rows, or only another crew's —
// still prints its row with the dash columns, and legacy branch-keyed status
// rows (no #session) join the same way.
func TestDashColumnsAndLegacyRows(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "no status rows",
			body: `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"claude","model":"sonnet","tier":"standard"}` + "\n",
			want: "claude\tsonnet\tstandard\t" + dash + "\t" + dash + "\t" + dash + "\n",
		},
		{
			name: "legacy branch-keyed status rows",
			body: `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"e","model":"m","tier":"t"}
{"ts":2000,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}
{"ts":3000,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"failed"}}
`,
			want: "e\tm\tt\t" + dash + "\tfailed\t1\n",
		},
		{
			// The arm joins on branch, not session: two dispatches of one
			// branch read the same status rows.
			name: "two dispatches of one branch",
			body: `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"first","model":"m","tier":"t"}
{"ts":1100,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"second","model":"m","tier":"t"}
{"ts":2000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}
{"ts":3000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s2-1","body":{"state":"done"}}
`,
			want: "first\tm\tt\t" + dash + "\tdone\t1\nsecond\tm\tt\t" + dash + "\tdone\t1\n",
		},
		{
			// No ts anywhere: $start stays null, so the duration is a dash
			// rather than a subtraction of nulls.
			name: "rows with no ts",
			body: `{"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"e","model":"m","tier":"t"}
{"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}
{"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"blocked"}}
`,
			want: "e\tm\tt\t" + dash + "\tblocked\t" + dash + "\n",
		},
		{
			// @tsv renders every column: nulls empty, other types as text,
			// number literals verbatim.
			name: "non-string columns",
			body: `{"ts":1.5,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"e","model":"1.50","tier":2,"shape":true}
{"ts":10,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}
{"ts":20,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"done"}}
`,
			want: "e\t1.50\t2\ttrue\tdone\t0\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.body)

			stdout, stderr, code := run(t, p, "c1", Options{})
			if code != 0 || stderr != "" {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if want := wantHeader + tc.want; stdout != want {
				t.Errorf("stdout got %q, want %q", stdout, want)
			}
		})
	}
}

func TestNonASCIIColumns(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"cláude 模型","model":"🚀","tier":"standard","shape":"—"}
{"ts":1785951264000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s9-9","body":{"state":"working"}}
{"ts":1785951269000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s9-9","body":{"state":"exited"}}
`)

	stdout, stderr, code := run(t, p, "c1", Options{})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	want := wantHeader + "cláude 模型\t🚀\tstandard\t" + dash + "\texited\t5\n"
	if stdout != want {
		t.Errorf("stdout got %q, want %q", stdout, want)
	}
}

func TestHeaderOnlyCases(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty log", ""},
		{"no dispatch rows", `{"ts":1,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"done"}}` + "\n"},
		{"another crew", `{"ts":1,"crew_id":"c2","kind":"dispatch","branch":"feat/x","engine":"e","model":"m","tier":"t"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.body)

			stdout, stderr, code := run(t, p, "c1", Options{})
			if code != 0 || stderr != "" {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if stdout != wantHeader {
				t.Errorf("stdout got %q, want just the header", stdout)
			}
		})
	}
}

func TestAbsentLogPrintsNothing(t *testing.T) {
	p := fixture(t)
	stdout, stderr, code := run(t, p, "c1", Options{JQColorsInvalid: true})
	if stdout != "" || stderr != "" || code != 0 {
		t.Fatalf("stdout %q stderr %q code %d", stdout, stderr, code)
	}
}

// `jq -s` has no torn-tail tolerance: the header survives, every row is lost.
func TestTornTailLosesEveryRow(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"e","model":"m","tier":"t"}`+
		"\n"+`{"ts":2000,"crew_id":"c1","kind":"dispatch","branch":"feat/y","engine":"e","model":"m","tier":`+"\n")

	stdout, stderr, code := run(t, p, "c1", Options{})
	if code != 5 {
		t.Fatalf("code %d, want 5", code)
	}
	if stdout != wantHeader {
		t.Errorf("stdout got %q, want just the header", stdout)
	}
	if !strings.HasPrefix(stderr, "crew: report: "+p.Log+": ") {
		t.Errorf("stderr %q, want the crew: report: line", stderr)
	}
}

// A row the fold cannot read — one it cannot index at all, or a status row whose
// body is not an object — fails the whole program, as jq's does.
func TestFoldTypeErrors(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{"non-object row", "5"},
		{"status body is a number", `{"ts":2,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"e","model":"m","tier":"t"}`+"\n"+tc.row+"\n")

			stdout, stderr, code := run(t, p, "c1", Options{})
			if code != 5 {
				t.Fatalf("code %d, want 5 (stderr %q)", code, stderr)
			}
			if stdout != wantHeader {
				t.Errorf("stdout got %q, want just the header", stdout)
			}
			if stderr == "" || !strings.HasPrefix(stderr, "crew: report: "+p.Log+": ") {
				t.Errorf("stderr %q, want the crew: report: line", stderr)
			}
		})
	}
}

func TestUnreadableLogKeepsItsHeader(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1,"crew_id":"c1","kind":"dispatch"}`+"\n")
	if err := os.Chmod(p.Log, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p.Log, 0o600) })

	stdout, stderr, code := run(t, p, "c1", Options{})
	if os.Geteuid() == 0 {
		t.Skip("root reads the log anyway")
	}
	if code != 2 {
		t.Fatalf("code %d, want 2", code)
	}
	if stdout != wantHeader {
		t.Errorf("stdout got %q, want just the header", stdout)
	}
	if !strings.Contains(stderr, "permission denied") {
		t.Errorf("stderr %q, want the open failure", stderr)
	}
}

// The arm prints its header, then jq warns about $JQ_COLORS.
func TestJQColorsWarnsAfterTheHeader(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"e","model":"m","tier":"t"}`+"\n")

	stdout, stderr, code := run(t, p, "c1", Options{JQColorsInvalid: true})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if want := wantHeader + "e\tm\tt\t" + dash + "\t" + dash + "\t" + dash + "\n"; stdout != want {
		t.Errorf("stdout got %q, want %q", stdout, want)
	}
	if stderr != "Failed to set $JQ_COLORS\n" {
		t.Errorf("stderr %q, want one warning", stderr)
	}
}

// A fold that fails on a later dispatch row costs every row: the arm's `jq -r`
// had already streamed the rows before the failing one, so Go printing the
// header only is the documented divergence (docs/crew-go-port.md), with the
// same exit 5 and stderr line.
func TestLaterFoldFailurePrintsNoEarlierRow(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"first","model":"m","tier":"t"}
{"ts":1100,"crew_id":"c1","kind":"dispatch","branch":"feat/b","engine":"second","model":"m","tier":"t"}
{"ts":2000,"crew_id":"c1","kind":"status","from":"worker:feat/b","body":5}
`)

	stdout, stderr, code := run(t, p, "c1", Options{})
	if code != 5 {
		t.Fatalf("code %d, want 5 (stderr %q)", code, stderr)
	}
	if stdout != wantHeader {
		t.Errorf("stdout got %q, want just the header", stdout)
	}
}
