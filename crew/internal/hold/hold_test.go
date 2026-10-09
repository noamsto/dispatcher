package hold

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// now is the fixed wall clock every case runs on: 2023-11-14T22:13:20.5Z, so
// `ts`/`id` and the "must be in the future" line are deterministic.
var now = time.Unix(1700000000, 500_000_000).UTC()

const future = "1800000000"

// elidedMarker is bus's `_ELIDED`, spelled out so the test fails if the marker
// the port copies ever changes.
const elidedMarker = " …[elided]"

type tree struct {
	paths bus.Paths
	dir   string
}

func setup(t *testing.T) *tree {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
	return &tree{paths: p, dir: dir}
}

func (tr *tree) write(t *testing.T, rows ...string) {
	t.Helper()
	if err := os.MkdirAll(tr.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr.paths.Log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (tr *tree) rows(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(tr.paths.Log)
	if err != nil {
		t.Fatalf("no log written: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// run is one arm invocation with the crew the fixture resolves to.
func (tr *tree) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return tr.runOpts(t, Options{}, args...)
}

func (tr *tree) runOpts(t *testing.T, o Options, args ...string) (string, string, int) {
	t.Helper()
	if o.CrewID == nil {
		o.CrewID = func() string { return "c1" }
	}
	if o.Now == nil {
		o.Now = func() time.Time { return now }
	}
	var out, err bytes.Buffer
	code := Run(args, tr.paths, &out, &err, o)
	return out.String(), err.String(), code
}

func (tr *tree) clock(t *testing.T, seconds string) Options {
	t.Helper()
	path := filepath.Join(tr.dir, "clock")
	if err := os.WriteFile(path, []byte(seconds+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{CrewClock: path}
}

func TestUnknownAction(t *testing.T) {
	usage := "crew: hold add|list|due|park|release\n"
	cases := []struct {
		args []string
		want string
	}{
		{nil, usage},
		{[]string{"hold"}, usage},
		{[]string{"bogus"}, usage},
		{[]string{"add", "--crew"}, "crew: --crew needs a value\n"},
		{[]string{"list", "--json", "--crew"}, "crew: --crew needs a value\n"},
		{[]string{"release"}, "crew: hold release <id> [--crew ID]\n"},
	}
	for _, tc := range cases {
		tr := setup(t)
		_, err, code := tr.run(t, tc.args...)
		if code != 1 || err != tc.want {
			t.Errorf("%v: code %d, stderr %q, want %q", tc.args, code, err, tc.want)
		}
	}
}

// The row is what the bash builder wrote, byte for byte: the body is a STRING,
// so its key order is part of the value and `jq -S` cannot normalise it.
func TestAddRowMatchesTheBuilder(t *testing.T) {
	tr := setup(t)
	out, err, code := tr.run(t, "add",
		"--engine", "claude", "--window", "5h", "--resets-at", future,
		"--agent", "codex", "--ref", "r1", "--branch", "b1", "--tier", "standard",
		"--model", "sonnet", "--effort", "medium", "--plan", "p.md", "--mcp", "m.json",
		"--draft", "--shape", "round-trip", "--crew", "c9", "a title")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, err)
	}
	rows := tr.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows: %q", rows)
	}
	id := strings.TrimSpace(out)
	if !strings.HasPrefix(id, "1700000000500-") {
		t.Errorf("id %q is not the fixed clock's milliseconds", id)
	}
	var row struct {
		TS      int64  `json:"ts"`
		CrewID  string `json:"crew_id"`
		From    string `json:"from"`
		To      string `json:"to"`
		Kind    string `json:"kind"`
		BodyStr string `json:"body"`
	}
	if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
		t.Fatal(err)
	}
	if row.TS != 1700000000500 || row.CrewID != "c9" || row.From != "dispatcher:c9" ||
		row.To != "hold:c9" || row.Kind != "msg" {
		t.Errorf("row envelope: %+v", row)
	}
	want := fmt.Sprintf(`{"id":"%s","wait":{"engine":"claude","window":"5h","resets_at":1800000000},`+
		`"task":{"ref":"r1","branch":"b1","tier":"standard","engine":"codex","model":"sonnet",`+
		`"effort":"medium","plan":"p.md","mcp":"m.json","draft":true,"shape":"round-trip",`+
		`"title":"a title","spec":null}}`, id)
	if row.BodyStr != want {
		t.Errorf("body string\n got %s\nwant %s", row.BodyStr, want)
	}
}

// `--argjson resets_at <digits>` is jq's literal: leading zeros are a number,
// every other digit survives exactly.
func TestAddResetsAtLiteral(t *testing.T) {
	cases := map[string]string{
		future:                 `"resets_at":1800000000`,
		"00001800000000":       `"resets_at":1800000000`,
		"99999999999999999999": `"resets_at":99999999999999999999`,
	}
	for resets, wantLeaf := range cases {
		t.Run(resets, func(t *testing.T) {
			tr := setup(t)
			_, err, code := tr.run(t, "add",
				"--engine", "claude", "--window", "5h", "--resets-at", resets,
				"--agent", "codex", "--ref", "r1", "--branch", "b1", "--tier", "standard",
				"--model", "sonnet", "--effort", "medium", "t")
			// A 20-digit literal is out of int64: bash's `test` fails, so the
			// arm says "future"; Go's parse overflows the same way.
			if resets == "99999999999999999999" {
				if code != 1 || err != "crew: hold add: --resets-at must be in the future\n" {
					t.Fatalf("code %d, stderr %q", code, err)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d: %s", code, err)
			}
			if !strings.Contains(tr.rows(t)[0], strings.ReplaceAll(wantLeaf, `"`, `\"`)) {
				t.Errorf("row %s has no %s", tr.rows(t)[0], wantLeaf)
			}
		})
	}
}

// The arm's rejection order is observable: the first missing required flag
// wins, and the clock test comes after the integer test.
func TestAddValidationOrder(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"missing engine": {
			[]string{"add", "--window", "5h", "--resets-at", future, "--agent", "a",
				"--ref", "r", "--branch", "b", "--tier", "t", "--model", "m", "--effort", "f", "x"},
			"crew: hold add: --engine is required\n",
		},
		"missing title": {
			[]string{"add", "--engine", "e", "--window", "5h", "--resets-at", future, "--agent", "a",
				"--ref", "r", "--branch", "b", "--tier", "t", "--model", "m", "--effort", "f"},
			"crew: hold add: a title is required\n",
		},
		"non-integer resets": {
			[]string{"add", "--engine", "e", "--window", "5h", "--resets-at", "abc", "--agent", "a",
				"--ref", "r", "--branch", "b", "--tier", "t", "--model", "m", "--effort", "f", "x"},
			"crew: hold add: --resets-at must be an integer epoch-seconds timestamp\n",
		},
		"flag without a value": {
			[]string{"add", "--engine"},
			"crew: --engine needs a value\n",
		},
		"unknown flag": {
			[]string{"add", "--engine", "e", "--bogus", "x"},
			"crew: hold add: unknown arg '--bogus'\n",
		},
		"invalid crew": {
			[]string{"add", "--engine", "e", "--window", "5h", "--resets-at", future, "--agent", "a",
				"--ref", "r", "--branch", "b", "--tier", "t", "--model", "m", "--effort", "f",
				"--crew", "a/b", "x"},
			"crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			_, err, code := tr.run(t, tc.args...)
			if code != 1 || err != tc.want {
				t.Errorf("code %d, stderr %q, want %q", code, err, tc.want)
			}
		})
	}
}

func TestAddNoCrewID(t *testing.T) {
	tr := setup(t)
	// The arm's `_hold_crew ""` dies in `_crew_id`, before any validation.
	_, err, code := tr.runOpts(t, Options{CrewID: func() string { return "" }}, "add",
		"--engine", "e", "--window", "5h", "--resets-at", future,
		"--agent", "a", "--ref", "r", "--branch", "b", "--tier", "t", "--model", "m",
		"--effort", "f", "x")
	if code != 1 || err != "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n" {
		t.Errorf("code %d, stderr %q", code, err)
	}
}

// `--resets-at` is compared against `_clock_now`, not the real clock: with the
// virtual clock set, a past-in-real-time but future-in-clock value is accepted
// and the reverse is refused.
func TestAddFutureFollowsTheVirtualClock(t *testing.T) {
	tr := setup(t)
	add := func(resets string) (string, int) {
		_, err, code := tr.runOpts(t, tr.clock(t, "1800000000"), "add",
			"--engine", "e", "--window", "5h", "--resets-at", resets, "--agent", "a",
			"--ref", "r", "--branch", "b", "--tier", "t", "--model", "m", "--effort", "f", "x")
		return err, code
	}
	if err, code := add("1800000001"); code != 0 {
		t.Errorf("future against the clock: %d %s", code, err)
	}
	if err, code := add("1800000000"); code != 1 ||
		err != "crew: hold add: --resets-at must be in the future\n" {
		t.Errorf("equal to the clock: %d %q", code, err)
	}
}

// `_clock_now` seeds an unset clock file from the real time, and the file it
// seeds is the one later reads use.
func TestClockSeedsMissingFile(t *testing.T) {
	tr := setup(t)
	path := filepath.Join(tr.dir, "clock")
	if got := clockText(Options{CrewClock: path, Now: func() time.Time { return now }}); got != "1700000000" {
		t.Errorf("clockText = %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "1700000000\n" {
		t.Errorf("seeded %q (%v)", data, err)
	}
	if got := clockText(Options{CrewClock: path, Now: func() time.Time { return now }}); got != "1700000000" {
		t.Errorf("re-read = %q", got)
	}
}

func TestAddSpec(t *testing.T) {
	tr := setup(t)
	spec := filepath.Join(tr.dir, "spec.md")
	if err := os.WriteFile(spec, []byte("the spec body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err, code := tr.run(t, "add", "--engine", "e", "--window", "5h", "--resets-at", future,
		"--agent", "a", "--ref", "r", "--branch", "b", "--tier", "t", "--model", "m",
		"--effort", "f", "--spec", spec, "with spec")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, err)
	}
	id := strings.TrimSpace(out)
	copied := filepath.Join(tr.paths.Dir, "holds", id+".md")
	data, rerr := os.ReadFile(copied)
	if rerr != nil || string(data) != "the spec body\n" {
		t.Fatalf("copied spec %q (%v)", data, err)
	}
	if !strings.Contains(tr.rows(t)[0], strings.ReplaceAll(`"spec":"`+copied+`"`, `"`, `\"`)) {
		t.Errorf("task.spec missing from %s", tr.rows(t)[0])
	}
}

func TestAddSpecErrors(t *testing.T) {
	tr := setup(t)
	dir := filepath.Join(tr.dir, "adir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ spec, want string }{
		"missing": {filepath.Join(tr.dir, "nope.md"), "is not readable"},
		"unreadable": {func() string {
			p := filepath.Join(tr.dir, "secret.md")
			if err := os.WriteFile(p, []byte("x"), 0o000); err != nil {
				t.Fatal(err)
			}
			return p
		}(), "is not readable"},
		// `[ -r dir ]` is true, so the arm reaches cp and cp refuses it; Go
		// reaches the copy and fails the same way (wording aside).
		"directory": {dir, "cannot copy --spec"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root reads a 0o000 file")
			}
			_, err, code := tr.run(t, "add", "--engine", "e", "--window", "5h", "--resets-at", future,
				"--agent", "a", "--ref", "r", "--branch", "b", "--tier", "t", "--model", "m",
				"--effort", "f", "--spec", tc.spec, "x")
			if code != 1 || !strings.Contains(err, tc.want) {
				t.Errorf("code %d, stderr %q", code, err)
			}
			if _, statErr := os.Stat(tr.paths.Log); statErr == nil {
				t.Errorf("a rejected --spec still wrote a log")
			}
		})
	}
}

// A title long enough to shrink must still leave `id` and `task.branch` whole:
// `_fit_line` shortens the title leaf, never the row around it.
func TestAddLongTitleFitsAndKeepsItsShape(t *testing.T) {
	for name, title := range map[string]string{
		"ascii": strings.Repeat("x", 20000),
		"cjk":   strings.Repeat("漢", 2000),
		"json":  `{"a":"` + strings.Repeat("y", 9000) + `","released":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			out, err, code := tr.run(t, "add", "--engine", "e", "--window", "5h",
				"--resets-at", future, "--agent", "a", "--ref", "r", "--branch", "keep-me",
				"--tier", "t", "--model", "m", "--effort", "f", title)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, err)
			}
			row := tr.rows(t)[0]
			if len(row) > bus.LineMax {
				t.Errorf("%d bytes, over the cap", len(row))
			}
			id := strings.TrimSpace(out)
			if !strings.Contains(row, `\"id\":\"`+id+`\"`) ||
				!strings.Contains(row, `\"branch\":\"keep-me\"`) {
				t.Errorf("id or branch did not survive: %s", row)
			}
			var body struct {
				Task struct {
					Title string `json:"title"`
				} `json:"task"`
			}
			if err := json.Unmarshal([]byte(mustBody(t, row)), &body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body.Task.Title, elidedMarker) {
				t.Errorf("title kept no elided marker")
			}
		})
	}
}

func mustBody(t *testing.T, row string) string {
	t.Helper()
	var v struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(row), &v); err != nil {
		t.Fatal(err)
	}
	return v.Body
}

func TestReleaseAppendsAndIsIdempotent(t *testing.T) {
	tr := setup(t)
	tr.write(t, holdRow(t, "c1", "h1", "9999999999"))
	for i := 0; i < 2; i++ {
		out, err, code := tr.run(t, "release", "h1")
		if code != 0 || out != "" {
			t.Fatalf("release %d: code %d, stdout %q, stderr %q", i, code, out, err)
		}
	}
	rows := tr.rows(t)
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	var row struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(rows[1]), &row); err != nil {
		t.Fatal(err)
	}
	if row.Body != `{"id":"h1","released":true}` {
		t.Errorf("release body %q", row.Body)
	}
	// Two releases of one id still fold to nothing outstanding.
	out, _, code := tr.run(t, "list", "--json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("list after release: %q (%d)", out, code)
	}
}

// A release id long enough to shrink goes through _shrink's JSON branch, so the
// id is cut and `released` survives — the record keeps its shape.
func TestReleaseLongIDShrinksTheLeaf(t *testing.T) {
	tr := setup(t)
	hid := strings.Repeat("z", 20000)
	_, err, code := tr.run(t, "release", hid)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, err)
	}
	row := tr.rows(t)[0]
	if len(row) > bus.LineMax {
		t.Errorf("%d bytes, over the cap", len(row))
	}
	var outer struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(row), &outer); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(outer.Body), &body); err != nil {
		t.Fatalf("the release body no longer parses: %v", err)
	}
	if body["released"] != true {
		t.Errorf("released lost: %v", body)
	}
	if !strings.Contains(body["id"].(string), elidedMarker) {
		t.Errorf("id was not shrunk: %v", body["id"])
	}
}

func TestListAndDueReadTheBus(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		holdRow(t, "c1", "h1", "9999999999"),
		holdRow(t, "c2", "other", "9999999999"),
		`{"ts":9,"crew_id":"c1","from":"d","to":"hold:c1","kind":"msg","body":"not json"}`,
	)
	out, err, code := tr.run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, err)
	}
	if got := jsonLen(t, out); got != 1 {
		t.Errorf("list --json length %d: %s", got, out)
	}
	out, _, _ = tr.run(t, "list")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 || lines[0] != renderHeader ||
		!strings.HasSuffix(lines[1], "\tt-h1") {
		t.Errorf("tsv:\n%s", out)
	}
	// A foreign crew's hold is invisible, and nothing is matured.
	if _, _, code = tr.run(t, "due", "--json"); code != 1 {
		t.Errorf("due with nothing matured: code %d", code)
	}
}

func TestDueMaturityBoundary(t *testing.T) {
	cases := map[string]struct {
		resetsAt string
		want     int
	}{
		"exactly now matures": {"1800000000", 1},
		"one second later does not": {
			"1800000001", 0,
		},
		"already past": {"1500000000", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			tr.write(t, holdRow(t, "c1", "h1", tc.resetsAt))
			out, _, code := tr.runOpts(t, tr.clock(t, "1800000000"), "due", "--json")
			if got := jsonLen(t, out); got != tc.want {
				t.Errorf("matured %d, want %d (%s)", got, tc.want, out)
			}
			if code != 0 && tc.want != 0 {
				t.Errorf("matured holds must exit 0, got %d", code)
			}
			if code != 1 && tc.want == 0 {
				t.Errorf("an empty due must exit 1, got %d", code)
			}
		})
	}
}

// A missing log is the arm's `[ -f "$log" ] || printf '[]'`: an empty array,
// exit 0, and no jq — so no $JQ_COLORS warning either.
func TestAbsentLogReadsAsEmpty(t *testing.T) {
	tr := setup(t)
	// `due` exits 1 on an empty set, `list` 0; the header and the `[]` are the
	// arm's, and the warning appears wherever the arm started a jq.
	cases := []struct {
		args   []string
		stdout string
		code   int
		warns  int
	}{
		{[]string{"list"}, renderHeader + "\n", 0, 1},
		{[]string{"list", "--json"}, "[]\n", 0, 0},
		{[]string{"due"}, renderHeader + "\n", 1, 1},
		{[]string{"due", "--json"}, "[]\n", 1, 1},
	}
	for _, tc := range cases {
		out, err, code := tr.runOpts(t, Options{JQColorsInvalid: true}, tc.args...)
		if out != tc.stdout {
			t.Errorf("%v printed %q, want %q", tc.args, out, tc.stdout)
		}
		if code != tc.code {
			t.Errorf("%v exited %d, want %d", tc.args, code, tc.code)
		}
		if got := strings.Count(err, "Failed to set $JQ_COLORS"); got != tc.warns {
			t.Errorf("%v warned %d times, want %d (%q)", tc.args, got, tc.warns, err)
		}
	}
}

// A bus jq cannot use costs every row: one stderr line, exit 5, nothing on
// stdout (the arm's `holds=$(…)` assignment under `set -e`).
func TestCorruptBusExitsFive(t *testing.T) {
	cases := map[string]struct {
		rows []string
		want string
	}{
		"a bare number row": {
			[]string{"5"},
			"expected an object",
		},
		"a torn tail": {
			[]string{`{"crew_id":"c1"`},
			"events.jsonl",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			tr.write(t, tc.rows...)
			for _, args := range [][]string{{"list", "--json"}, {"due", "--json"}, {"park", "60"}} {
				out, err, code := tr.run(t, args...)
				if code != exitType || out != "" || !strings.Contains(err, tc.want) {
					t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, out, err)
				}
				if strings.Count(err, "\n") != 1 {
					t.Errorf("%v printed %q", args, err)
				}
			}
		})
	}
}

// A body that parses but is not an object is a type error in the arm's fold, and
// a body whose field is an array is a `@tsv` error in its render.
func TestHostileBodiesExitFive(t *testing.T) {
	tr := setup(t)
	tr.write(t, `{"ts":1,"crew_id":"c1","from":"d","to":"hold:c1","kind":"msg","body":"5"}`)
	if _, err, code := tr.run(t, "list", "--json"); code != exitType ||
		!strings.Contains(err, "expected an object") {
		t.Errorf("list --json: code %d, stderr %q", code, err)
	}
	tr2 := setup(t)
	tr2.write(t, `{"ts":1,"crew_id":"c1","from":"d","to":"hold:c1","kind":"msg",`+
		`"body":"{\"id\":[\"arr\"],\"wait\":{\"resets_at\":1},\"task\":{}}"}`)
	if _, err, code := tr2.run(t, "list"); code != exitType ||
		!strings.Contains(err, "@tsv") {
		t.Errorf("list: code %d, stderr %q", code, err)
	}
}

func TestParkBranches(t *testing.T) {
	cases := map[string]struct {
		resetsAt string
		def      string
		want     string
	}{
		"nothing outstanding": {"", "3600", "3600\n"},
		"earliest already matured": {
			"1500000000", "3600", "3600\n",
		},
		// min(default, earliest - now): the remaining second wins over 60.
		"earliest one second out": {
			"1800000001", "60", "1\n",
		},
		"earliest sooner than the default": {
			"1800000030", "3600", "30\n",
		},
		"default under the remaining time": {
			"1800000030", "5", "5\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			var opts Options
			if tc.resetsAt != "" {
				tr.write(t, holdRow(t, "c1", "h1", tc.resetsAt))
			}
			opts = tr.clock(t, "1800000000")
			out, err, code := tr.runOpts(t, opts, "park", tc.def)
			if code != 0 || out != tc.want {
				t.Errorf("park %s: code %d, stdout %q (%s), want %q", tc.def, code, out, err, tc.want)
			}
		})
	}
}

// `park` never returns 0: `crew watch --timeout 0` is refused, and a 0 would
// fail the cursor re-arm.
func TestParkNeverReturnsZero(t *testing.T) {
	tr := setup(t)
	tr.write(t, holdRow(t, "c1", "h1", "1800000000"))
	out, _, code := tr.runOpts(t, tr.clock(t, "1800000000"), "park", "1")
	if code != 0 || out != "1\n" {
		t.Errorf("park 1: %q (%d)", out, code)
	}
}

func TestParkRejectsABadDefault(t *testing.T) {
	tr := setup(t)
	want := "crew: hold park <default> must be a positive integer number of seconds\n"
	for _, def := range []string{"", "0", "-5", "abc", "1.5", "99999999999999999999"} {
		_, err, code := tr.run(t, "park", def)
		if code != 1 || err != want {
			t.Errorf("park %q: code %d, stderr %q", def, code, err)
		}
	}
	_, err, code := tr.run(t, "park", "60", "--bogus")
	if code != 1 || err != "crew: hold park <default> [--crew ID]\n" {
		t.Errorf("stray flag: code %d, stderr %q", code, err)
	}
}

// The arm forked one jq per step and jq warns once per process; Go runs each
// fold once, so the line appears once per call — and not at all where the arm
// started no jq either.
func TestJQColorsWarningOnce(t *testing.T) {
	cases := map[string]struct {
		rows []string
		args []string
		want int
	}{
		"list with a log":      {[]string{holdRowString()}, []string{"list"}, 1},
		"list without a log":   {nil, []string{"list"}, 1},
		"list --json with log": {[]string{holdRowString()}, []string{"list", "--json"}, 1},
		"list --json no log":   {nil, []string{"list", "--json"}, 0},
		"due":                  {nil, []string{"due", "--json"}, 1},
		"park":                 {nil, []string{"park", "60"}, 1},
		"add":                  {nil, []string{"add"}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			if tc.rows != nil {
				tr.write(t, tc.rows...)
			}
			args := tc.args
			if args[0] == "add" {
				// A rejected `add` starts no jq in the arm either.
				args = append(args, "--engine", "e")
			}
			_, err, _ := tr.runOpts(t, Options{JQColorsInvalid: true}, args...)
			if got := strings.Count(err, "Failed to set $JQ_COLORS"); got != tc.want {
				t.Errorf("%s warned %d times, want %d (%q)", name, got, tc.want, err)
			}
		})
	}
}

func TestDigitsLiteral(t *testing.T) {
	cases := map[string]string{"7": "7", "007": "7", "0": "0", "0000": "0", "1800000000": "1800000000"}
	for in, want := range cases {
		if got := digitsLiteral(in); got != want {
			t.Errorf("digitsLiteral(%q) = %q", in, got)
		}
	}
	if isDigits("") || isDigits("1a") || isDigits("-1") || !isDigits("0") {
		t.Error("isDigits disagrees with the arm's case pattern")
	}
}

func TestOrNulAndElems(t *testing.T) {
	if orNull("").Kind() != jsonv.KindNull || orNull("x").Kind() != jsonv.KindString {
		t.Error(`orNull: "" must be null and "x" a string`)
	}
	if elems(jsonv.Str("x")) != nil {
		t.Error("elems of a non-array is no input at all")
	}
	if got := elems(jsonv.Array()); len(got) != 0 {
		t.Error("elems of an empty array must be an empty slice, so `.` is []")
	}
}

// holdRow is a seed shaped like `_build_hold`'s output, written straight to the
// bus so a test can plant a matured hold (`add` refuses a past --resets-at).
func holdRow(t *testing.T, crew, id, resetsAt string) string {
	t.Helper()
	return holdRowBody(t, crew, id, mustJSON(t, map[string]any{
		"id": id,
		"wait": map[string]any{
			"engine": "claude", "window": "5h", "resets_at": json.RawMessage(resetsAt),
		},
		"task": map[string]any{
			"ref": "r1", "branch": "b1", "tier": "standard", "engine": "codex",
			"model": "sonnet", "effort": "medium", "plan": nil, "mcp": nil,
			"draft": false, "shape": nil, "title": "t-" + id, "spec": nil,
		},
	}))
}

func holdRowBody(t *testing.T, crew, id, body string) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{
		"ts": 1, "crew_id": crew, "from": "dispatcher:" + crew,
		"to": "hold:" + crew, "kind": "msg", "body": json.RawMessage(encoded),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(outer)
}

func holdRowString() string {
	body, _ := json.Marshal(map[string]any{
		"id": "h1", "wait": map[string]any{"engine": "e", "window": "w", "resets_at": 9999999999},
		"task": map[string]any{"ref": "r1", "branch": "b1", "title": "t-h1"},
	})
	row, _ := json.Marshal(map[string]any{
		"ts": 1, "crew_id": "c1", "from": "dispatcher:c1", "to": "hold:c1",
		"kind": "msg", "body": string(body),
	})
	return string(row)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	// Object keys are sorted here; the fold under test reads values, not order.
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func jsonLen(t *testing.T, s string) int {
	t.Helper()
	var v []jsonv.Value
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not a JSON array (%v): %q", err, s)
	}
	return len(v)
}
