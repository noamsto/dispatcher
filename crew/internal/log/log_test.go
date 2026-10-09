package log

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// fixture is a bus without git: Run reads bus.Paths, so a temp dir stands in
// for the common dir.
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

func run(t *testing.T, p bus.Paths, crew string, o Options) (string, string, int) {
	t.Helper()
	var out, errB bytes.Buffer
	code := Run(crew, p, &out, &errB, o)
	return out.String(), errB.String(), code
}

// c1Row and friends are the arm's own event shapes; the expected outputs below
// are `jq -c 'select(.crew_id==$crew)'` on the same input, byte for byte.
const (
	c1Row = `{"ts":1785951264000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`
	c2Row = `{"ts":1785951265000,"crew_id":"c2","kind":"msg","from":"dispatcher:c2","to":"worker:feat/y#s2-1","body":{"text":"héllo 中文 🚀"}}`
	c1Msg = `{"crew_id":"c9","kind":"msg","crew_id":"c1","n":1.50,"e":1e3,"big":18446744073709551617}`
	// c1MsgOut is jq -c on c1Msg: the duplicate key collapses to its last
	// value in place, and the number literals keep jq 1.8's canonical text.
	c1MsgOut = `{"crew_id":"c1","kind":"msg","n":1.50,"e":1E+3,"big":18446744073709551617}`
	// c1UniOut is jq -c on c1UniRow: raw UTF-8, jq's \u007f and \u00XX escapes,
	// -0.0 kept, and an out-of-range literal in jq's canonical form.
	c1UniRow = `{"crew_id":"c1","u":"héllo 中文 🚀 tab\there <b> & \"q\" \u007f","z":-0.0,"inf":1e1000}`
	c1UniOut = `{"crew_id":"c1","u":"héllo 中文 🚀 tab\there <b> & \"q\" \u007f","z":-0.0,"inf":1E+1000}`
)

func TestFiltersOneCrewByteForByte(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, c1Row+"\n"+c2Row+"\n"+c1Msg+"\n"+c1UniRow+"\n")

	stdout, stderr, code := run(t, p, "c1", Options{})
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	want := c1Row + "\n" + c1MsgOut + "\n" + c1UniOut + "\n"
	if stdout != want {
		t.Errorf("stdout\n got %q\nwant %q", stdout, want)
	}
}

func TestNoMatchAndEmptyLog(t *testing.T) {
	for _, tc := range []struct{ name, body, crew string }{
		{"other crews only", c2Row + "\n", "c1"},
		{"empty log", "", "c1"},
		{"blank lines between rows", "\n" + c2Row + "\n\n", "c1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.body)
			stdout, stderr, code := run(t, p, tc.crew, Options{})
			if stdout != "" || stderr != "" || code != 0 {
				t.Fatalf("stdout %q stderr %q code %d", stdout, stderr, code)
			}
		})
	}
}

func TestAbsentLogIsSilent(t *testing.T) {
	t.Run("no log at all", func(t *testing.T) {
		p := fixture(t)
		stdout, stderr, code := run(t, p, "c1", Options{JQColorsInvalid: true})
		if stdout != "" || stderr != "" || code != 0 {
			t.Fatalf("stdout %q stderr %q code %d", stdout, stderr, code)
		}
	})

	t.Run("log is a directory", func(t *testing.T) {
		p := fixture(t)
		if err := os.MkdirAll(p.Log, 0o755); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, code := run(t, p, "c1", Options{})
		if stdout != "" || stderr != "" || code != 0 {
			t.Fatalf("stdout %q stderr %q code %d", stdout, stderr, code)
		}
	})
}

func TestTornTailKeepsThePrefixAndExitsFive(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, c1Row+"\n"+c2Row+"\n"+`{"crew_id":"c1","ts":3`+"\n")

	stdout, stderr, code := run(t, p, "c1", Options{})
	if code != 5 {
		t.Fatalf("code %d, want 5 (jq's status for a parse error)", code)
	}
	if want := c1Row + "\n"; stdout != want {
		t.Errorf("stdout got %q, want %q", stdout, want)
	}
	if !strings.HasPrefix(stderr, "crew: log: "+p.Log+": ") || strings.Count(stderr, "\n") != 1 {
		t.Errorf("stderr %q, want one crew: log: line", stderr)
	}
}

func TestNonObjectRowsFollowJqLastInputRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantCode   int
		wantStdout string
		wantErrs   []string
	}{
		{
			name:     "error last",
			body:     c1Row + "\n5\n",
			wantCode: 5,
			// jq prints the match before the error, then exits 5.
			wantStdout: c1Row + "\n",
			wantErrs:   []string{`cannot index number with "crew_id"`},
		},
		{
			name:     "error then object is exit 0",
			body:     "5\n" + c1Row + "\n",
			wantCode: 0,
			// jq's status is the last input's outcome, not a sticky flag.
			wantStdout: c1Row + "\n",
			wantErrs:   []string{`cannot index number with "crew_id"`},
		},
		{
			name:       "array string and boolean rows",
			body:       "[]\n\"s\"\ntrue\n" + c1Row + "\n",
			wantCode:   0,
			wantStdout: c1Row + "\n",
			wantErrs: []string{
				`cannot index array with "crew_id"`,
				`cannot index string with "crew_id"`,
				`cannot index boolean with "crew_id"`,
			},
		},
		{
			name:     "null row is not an error",
			body:     "null\n" + c2Row + "\n",
			wantCode: 0,
			wantErrs: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, tc.body)

			stdout, stderr, code := run(t, p, "c1", Options{})
			if code != tc.wantCode {
				t.Fatalf("code %d, want %d (stderr %q)", code, tc.wantCode, stderr)
			}
			if stdout != tc.wantStdout {
				t.Errorf("stdout got %q, want %q", stdout, tc.wantStdout)
			}
			lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
			if stderr == "" {
				lines = nil
			}
			if len(lines) != len(tc.wantErrs) {
				t.Fatalf("stderr %q, want %d lines", stderr, len(tc.wantErrs))
			}
			for i, want := range tc.wantErrs {
				if lines[i] != "crew: log: "+p.Log+": "+want {
					t.Errorf("stderr line %d = %q, want %q", i, lines[i], want)
				}
			}
		})
	}
}

func TestNonStringCrewIDNeverMatches(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, `{"crew_id":1}`+"\n"+`{"crew_id":{"a":1}}`+"\n")

	stdout, stderr, code := run(t, p, "1", Options{})
	if stdout != "" || stderr != "" || code != 0 {
		t.Fatalf("stdout %q stderr %q code %d", stdout, stderr, code)
	}
}

func TestUnreadableLogIsJqsExitTwo(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, c1Row+"\n")
	if err := os.Chmod(p.Log, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p.Log, 0o600) })

	stdout, stderr, code := run(t, p, "c1", Options{})
	if os.Geteuid() == 0 {
		t.Skip("root reads the log anyway")
	}
	if code != 2 || stdout != "" {
		t.Fatalf("code %d stdout %q, want 2 and nothing", code, stdout)
	}
	if !strings.Contains(stderr, "permission denied") {
		t.Errorf("stderr %q, want the open failure", stderr)
	}
}

func TestJQColorsWarnsOnceWhenTheLogExists(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, c1Row+"\n")

	stdout, stderr, code := run(t, p, "c1", Options{JQColorsInvalid: true})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if want := c1Row + "\n"; stdout != want {
		t.Errorf("stdout got %q, want %q", stdout, want)
	}
	if stderr != "Failed to set $JQ_COLORS\n" {
		t.Errorf("stderr %q, want one warning", stderr)
	}
}

// The arm's jq colours its output when stdout is a terminal; main.go decides
// that and hands the palette down.
func TestColourComesFromThePalette(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, c1Row+"\n")

	palette := jsonv.DefaultPalette()
	stdout, _, code := run(t, p, "c1", Options{Colors: &palette})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.HasPrefix(stdout, palette[jsonv.KindObject]+"{") {
		t.Errorf("stdout %q, want it opened with the object colour", stdout)
	}
	if !strings.Contains(stdout, palette[7]+`"crew_id"`+"\x1b[0m") {
		t.Errorf("stdout %q, want a coloured key", stdout)
	}
}
