package hold

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/sender"
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
func (tr *tree) run(t *testing.T, req Request) (string, string, int) {
	t.Helper()
	return tr.runOpts(t, Options{}, req)
}

// fullAdd is an `add` that passes every check; a case edits the field it probes.
func fullAdd(edit func(*AddRequest)) Request {
	a := AddRequest{
		Engine: "e", Window: "5h", ResetsAt: future, Agent: "a", Ref: "r",
		Branch: "b", Tier: "t", Model: "m", Effort: "f", Title: []string{"x"},
	}
	if edit != nil {
		edit(&a)
	}
	return Request{Action: "add", Add: a}
}

func (tr *tree) runOpts(t *testing.T, o Options, req Request) (string, string, int) {
	t.Helper()
	if o.CrewID == nil {
		o.CrewID = func() string { return "c1" }
	}
	if o.Clock.Now == nil {
		o.Clock.Now = func() time.Time { return now }
	}
	var out, err bytes.Buffer
	code := Run(t.Context(), req, tr.paths, &out, &err, o)
	return out.String(), err.String(), code
}

func (tr *tree) clock(t *testing.T, seconds string) Options {
	t.Helper()
	path := filepath.Join(tr.dir, "clock")
	if err := os.WriteFile(path, []byte(seconds+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{Clock: clock.Clock{CrewClock: path}}
}

func TestUnknownAction(t *testing.T) {
	usage := "crew: hold add|list|due|park|release\n"
	for _, action := range []string{"", "hold", "bogus"} {
		tr := setup(t)
		_, err, code := tr.run(t, Request{Action: action})
		if code != 1 || err != usage {
			t.Errorf("%q: code %d, stderr %q, want %q", action, code, err, usage)
		}
	}
	tr := setup(t)
	_, err, code := tr.run(t, Request{Action: "release"})
	if code != 0 || err != "" {
		t.Errorf("release with no id is the parser's refusal, not Run's: %d %q", code, err)
	}
}

// The row is what the bash builder wrote, byte for byte: the body is a STRING,
// so its key order is part of the value and `jq -S` cannot normalise it.
func TestAddRowMatchesTheBuilder(t *testing.T) {
	tr := setup(t)
	out, err, code := tr.run(t, Request{Action: "add", Crew: "c9", Add: AddRequest{
		Engine: "claude", Window: "5h", ResetsAt: future,
		Agent: "codex", Ref: "r1", Branch: "b1", Tier: "standard",
		Model: "sonnet", Effort: "medium", Plan: "p.md", MCP: "m.json",
		Draft: true, Shape: "round-trip", Title: []string{"a", "title"},
	}})
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
			_, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.ResetsAt = resets }))
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

// `bus.FitLine` measures the encoded line against LineMax, and jq wrote `<`, `>`
// and `&` as one byte each, so both encoders (the body's and the row's) must
// leave them raw — an escaped `\u003c` is five bytes longer and moves the shrink
// point. A title reaches the row only through the body string.
func TestAddRowKeepsHTMLRaw(t *testing.T) {
	tr := setup(t)
	_, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Title = []string{`<b> & </b>`} }))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, err)
	}
	if row := tr.rows(t)[0]; !strings.Contains(row, `<b> & </b>`) {
		t.Errorf("the row escaped the title's html: %s", row)
	}
}

// D4: the body string uses Go's encoding, not jq's. Three leaves come out
// differently and every consumer reads the body with `fromjson`, which makes all
// three value-equal.
func TestAddRowEncodesStringsLikeGo(t *testing.T) {
	cases := map[string]struct{ title, want string }{
		"DEL is raw, not \\u007f": {"d\x7fhere", "\"title\":\"d\x7fhere\""},
		"U+2028 is escaped":       {"s\u2028here", "\"title\":\"s\\u2028here\""},
		"a bad byte is one U+FFFD, escaped": {
			"bad \xff byte", "\"title\":\"bad \\ufffd byte\"",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			_, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Title = []string{tc.title} }))
			if code != 0 {
				t.Fatalf("exit %d: %s", code, err)
			}
			if body := mustBody(t, tr.rows(t)[0]); !strings.Contains(body, tc.want) {
				t.Errorf("body %s has no %s", body, tc.want)
			}
		})
	}
}

// The arm's rejection order is observable: the first missing required flag
// wins, and the clock test comes after the integer test.
func TestAddValidationOrder(t *testing.T) {
	cases := map[string]struct {
		req  Request
		want string
	}{
		"missing engine": {
			fullAdd(func(a *AddRequest) { a.Engine = "" }),
			"crew: hold add: --engine is required\n",
		},
		"two missing, the first in the table wins": {
			fullAdd(func(a *AddRequest) { a.Model, a.Window = "", "" }),
			"crew: hold add: --window is required\n",
		},
		"missing title": {
			fullAdd(func(a *AddRequest) { a.Title = nil }),
			"crew: hold add: a title is required\n",
		},
		"non-integer resets": {
			fullAdd(func(a *AddRequest) { a.ResetsAt = "abc" }),
			"crew: hold add: --resets-at must be an integer epoch-seconds timestamp\n",
		},
		"invalid crew": {
			func() Request { r := fullAdd(nil); r.Crew = "a/b"; return r }(),
			"crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			_, err, code := tr.run(t, tc.req)
			if code != 1 || err != tc.want {
				t.Errorf("code %d, stderr %q, want %q", code, err, tc.want)
			}
		})
	}
}

// Each required flag, left out alone, is refused by name.
func TestAddEachRequiredFlagRejected(t *testing.T) {
	omit := map[string]func(*AddRequest){
		"--engine":    func(a *AddRequest) { a.Engine = "" },
		"--window":    func(a *AddRequest) { a.Window = "" },
		"--resets-at": func(a *AddRequest) { a.ResetsAt = "" },
		"--agent":     func(a *AddRequest) { a.Agent = "" },
		"--ref":       func(a *AddRequest) { a.Ref = "" },
		"--branch":    func(a *AddRequest) { a.Branch = "" },
		"--tier":      func(a *AddRequest) { a.Tier = "" },
		"--model":     func(a *AddRequest) { a.Model = "" },
		"--effort":    func(a *AddRequest) { a.Effort = "" },
	}
	for flag, edit := range omit {
		t.Run(flag, func(t *testing.T) {
			tr := setup(t)
			_, err, code := tr.run(t, fullAdd(edit))
			if want := "crew: hold add: " + flag + " is required\n"; code != 1 || err != want {
				t.Errorf("code %d, stderr %q, want %q", code, err, want)
			}
		})
	}
}

// A --resets-at behind the clock is refused with the same line as one equal to it.
func TestAddRejectsAPastResetsAt(t *testing.T) {
	tr := setup(t)
	_, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.ResetsAt = "1600000000" }))
	if want := "crew: hold add: --resets-at must be in the future\n"; code != 1 || err != want {
		t.Errorf("code %d, stderr %q, want %q", code, err, want)
	}
	if _, serr := os.Stat(tr.paths.Log); serr == nil {
		t.Error("a rejected add still wrote a log")
	}
}

// What `add` prints is the minted id and nothing else.
func TestAddPrintsOnlyTheMintedID(t *testing.T) {
	tr := setup(t)
	out, _, code := tr.run(t, fullAdd(nil))
	if code != 0 || !regexp.MustCompile(`^[0-9]+-[0-9]+\n$`).MatchString(out) {
		t.Errorf("code %d, stdout %q", code, out)
	}
}

// `list` folds adds minus releases; an add after a release of another id stays,
// and one body that is not JSON costs its own row and nothing else.
func TestListFoldsAddsMinusReleases(t *testing.T) {
	tr := setup(t)
	tr.write(t,
		seedHold(t, "c1", "h1", future),
		seedHold(t, "c1", "h2", future),
		`{"ts":9,"crew_id":"c1","from":"d","to":"hold:c1","kind":"msg","body":"not json"}`,
	)
	if _, err, code := tr.run(t, Request{Action: "release", ID: "h1"}); code != 0 {
		t.Fatalf("release: %d %s", code, err)
	}
	out, _, code := tr.run(t, Request{Action: "list", JSON: true})
	if ids := strings.Join(idsOf(t, out), ","); code != 0 || ids != "h2" {
		t.Errorf("outstanding %q (code %d), want only h2", ids, code)
	}
}

// Releasing an id nothing ever added is a quiet success that still writes its row.
func TestReleaseUnknownIDExitsZero(t *testing.T) {
	tr := setup(t)
	out, err, code := tr.run(t, Request{Action: "release", ID: "does-not-exist"})
	if code != 0 || out != "" || err != "" {
		t.Errorf("code %d, stdout %q, stderr %q", code, out, err)
	}
	if rows := tr.rows(t); len(rows) != 1 {
		t.Errorf("rows: %d, want the release row", len(rows))
	}
}

// `due --json` reports the matured hold and leaves the unmatured one out.
func TestDueJSONReportsOnlyTheMaturedHold(t *testing.T) {
	tr := setup(t)
	tr.write(t, seedHold(t, "c1", "due1", "1500000000"), seedHold(t, "c1", "future1", future))
	out, _, code := tr.runOpts(t, tr.clock(t, "1700000000"), Request{Action: "due", JSON: true})
	var held []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &held); err != nil || code != 0 {
		t.Fatalf("code %d, %q (%v)", code, out, err)
	}
	if len(held) != 1 || held[0].ID != "due1" {
		t.Errorf("due %v, want only due1", held)
	}
}

// Holds are scoped to their crew: each crew's `list` and `due` read their own
// queue, and one crew's matured hold never matures in another's `due`.
func TestHoldsAreInvisibleToAnotherCrew(t *testing.T) {
	tr := setup(t)
	tr.write(t, seedHold(t, "c1", "h1", "1500000000"), seedHold(t, "c2", "other", "9999999999"))
	for _, tc := range []struct {
		action, crew, want string
		code               int
	}{
		{"list", "c1", "h1", 0},
		{"due", "c1", "h1", 0},
		{"list", "c2", "other", 0},
		// c2's own hold is not matured, and c1's matured one is not c2's.
		{"due", "c2", "", 1},
	} {
		out, err, code := tr.run(t, Request{Action: tc.action, JSON: true, Crew: tc.crew})
		if ids := strings.Join(idsOf(t, out), ","); code != tc.code || ids != tc.want {
			t.Errorf("%s --crew %s: code %d ids %q, want %d %q (%s)", tc.action, tc.crew, code, ids, tc.code, tc.want, err)
		}
	}
}

// The bus writer's torn-tail rule reaches the hold arm: a log whose last byte is
// not a newline gets one, so the fragment stays one record and the hold the next.
func TestAddTerminatesATornTail(t *testing.T) {
	tr := setup(t)
	if err := os.MkdirAll(tr.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	torn := `{"crew_id":"c1","kind":"status","to":"worker:feat/x","body":{"state":"working"}`
	if err := os.WriteFile(tr.paths.Log, []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := tr.run(t, fullAdd(nil))
	if code != 0 {
		t.Fatalf("add: code %d, stderr %q", code, errOut)
	}
	data, err := os.ReadFile(tr.paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("the log holds %d lines, want 2:\n%s", n, data)
	}
	var row struct {
		Kind    string `json:"kind"`
		To      string `json:"to"`
		BodyStr string `json:"body"`
	}
	if err := json.Unmarshal([]byte(tr.rows(t)[1]), &row); err != nil {
		t.Fatal(err)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(row.BodyStr), &body); err != nil {
		t.Fatalf("the hold body no longer parses: %v", err)
	}
	if id := strings.TrimSpace(out); row.Kind != "msg" || row.To != "hold:c1" || body.ID != id {
		t.Errorf("tail row: kind %q to %q id %q, want msg hold:c1 %q", row.Kind, row.To, body.ID, id)
	}
}

// With no CREW_CLOCK the maturity test and `park`'s remaining time read the real clock.
func TestDueAndParkUseRealTimeWithoutAVirtualClock(t *testing.T) {
	real := func() time.Time { return time.Now() }
	tr := setup(t)
	tr.write(t, seedHold(t, "c1", "h1", fmt.Sprint(time.Now().Unix()-10)))
	if _, _, code := tr.runOpts(t, Options{Clock: clock.Clock{Now: real}}, Request{Action: "due"}); code != 0 {
		t.Errorf("a matured hold: due exited %d", code)
	}
	tr2 := setup(t)
	tr2.write(t, seedHold(t, "c1", "h2", fmt.Sprint(time.Now().Unix()+30)))
	out, _, _ := tr2.runOpts(t, Options{Clock: clock.Clock{Now: real}}, Request{Action: "park", Default: "1000"})
	if n, err := strconv.Atoi(strings.TrimSpace(out)); err != nil || n < 1 || n > 30 {
		t.Errorf("park printed %q, want 1..30", out)
	}
}

func TestAddNoCrewID(t *testing.T) {
	tr := setup(t)
	// The arm's `_hold_crew ""` dies in `_crew_id`, before any validation.
	_, err, code := tr.runOpts(t, Options{CrewID: func() string { return "" }}, fullAdd(nil))
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
		_, err, code := tr.runOpts(t, tr.clock(t, "1800000000"), fullAdd(func(a *AddRequest) { a.ResetsAt = resets }))
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

func TestAddSpec(t *testing.T) {
	tr := setup(t)
	spec := filepath.Join(tr.dir, "spec.md")
	if err := os.WriteFile(spec, []byte("the spec body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Spec, a.Title = spec, []string{"with", "spec"} }))
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
	// Each case builds its own fixture: the directory case creates holds/, which
	// would leak into whichever case ran after it.
	cases := map[string]struct {
		spec func(t *testing.T, dir string) string
		want string
	}{
		"missing": {func(_ *testing.T, dir string) string { return filepath.Join(dir, "nope.md") }, "is not readable"},
		"unreadable": {func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "secret.md")
			if err := os.WriteFile(p, []byte("x"), 0o000); err != nil {
				t.Fatal(err)
			}
			return p
		}, "is not readable"},
		// `[ -r dir ]` is true, so the arm reaches cp and cp refuses it; Go
		// reaches the copy and fails the same way (wording aside).
		"directory": {func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "adir")
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		}, "cannot copy --spec"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root reads a 0o000 file")
			}
			tr := setup(t)
			spec := tc.spec(t, tr.dir)
			_, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Spec = spec }))
			if code != 1 || !strings.Contains(err, tc.want) {
				t.Errorf("code %d, stderr %q", code, err)
			}
			if _, statErr := os.Stat(tr.paths.Log); statErr == nil {
				t.Errorf("a rejected --spec still wrote a log")
			}
			// A directory passes the readability test and fails in the copy, after
			// holds/ exists; the readability refusals come before it.
			if _, statErr := os.Stat(filepath.Join(tr.paths.Dir, "holds")); name != "directory" && statErr == nil {
				t.Errorf("a rejected --spec still created holds/")
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
			out, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Branch, a.Title = "keep-me", []string{title} }))
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

// A title with newlines shrinks line by line, the way `cut -c` does: the line
// count survives each cut, and a text whose lines never exceed keep shrinks all
// the way to the elided marker (bash's `cut -c1-0` prints nothing either).
func TestAddMultiLineTitleShrinksEachLine(t *testing.T) {
	cases := map[string]struct {
		title string
		lines int
	}{
		"one char per line, so every cut is a no-op": {strings.Repeat("a\n", 5000), 0},
		"wide lines keep their count": {
			strings.Repeat("x\u00e9y"+strings.Repeat("y", 37)+"\n", 200), 200,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := setup(t)
			_, err, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Title = []string{tc.title} }))
			if code != 0 {
				t.Fatalf("exit %d: %s", code, err)
			}
			row := tr.rows(t)[0]
			if len(row) > bus.LineMax {
				t.Errorf("%d bytes, over the cap", len(row))
			}
			var body struct {
				Task struct {
					Title string `json:"title"`
				} `json:"task"`
			}
			if err := json.Unmarshal([]byte(mustBody(t, row)), &body); err != nil {
				t.Fatal(err)
			}
			title := body.Task.Title
			if !strings.HasSuffix(title, elidedMarker) {
				t.Errorf("no elided marker: %q", title)
				return
			}
			// The title is one of Shrink's own outputs, at the keep its first
			// line was cut to.
			keep := utf8.RuneCountInString(strings.SplitN(title, "\n", 2)[0])
			if title == elidedMarker {
				keep = 0
			}
			if want := bus.Shrink(tc.title, keep); title != want {
				t.Errorf("title is not Shrink(title, %d):\n got %q\nwant %q", keep, title, want)
			}
			if got := strings.Count(title, "\n"); tc.lines > 0 && got != tc.lines-1 {
				t.Errorf("%d newlines, want %d", got, tc.lines-1)
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
	tr.write(t, seedHold(t, "c1", "h1", "9999999999"))
	for i := 0; i < 2; i++ {
		out, err, code := tr.run(t, Request{Action: "release", ID: "h1"})
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
	out, _, code := tr.run(t, Request{Action: "list", JSON: true})
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("list after release: %q (%d)", out, code)
	}
}

// A release id long enough to shrink goes through _shrink's JSON branch, so the
// id is cut and `released` survives — the record keeps its shape.
func TestReleaseLongIDShrinksTheLeaf(t *testing.T) {
	tr := setup(t)
	hid := strings.Repeat("z", 20000)
	_, err, code := tr.run(t, Request{Action: "release", ID: hid})
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
		seedHold(t, "c1", "h1", "9999999999"),
		seedHold(t, "c2", "other", "9999999999"),
		`{"ts":9,"crew_id":"c1","from":"d","to":"hold:c1","kind":"msg","body":"not json"}`,
	)
	out, err, code := tr.run(t, Request{Action: "list", JSON: true})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, err)
	}
	if got := jsonLen(t, out); got != 1 {
		t.Errorf("list --json length %d: %s", got, out)
	}
	out, _, _ = tr.run(t, Request{Action: "list"})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 || lines[0] != renderHeader ||
		!strings.HasSuffix(lines[1], "\tt-h1") {
		t.Errorf("tsv:\n%s", out)
	}
	// A foreign crew's hold is invisible, and nothing is matured.
	if _, _, code = tr.run(t, Request{Action: "due", JSON: true}); code != 1 {
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
			tr.write(t, seedHold(t, "c1", "h1", tc.resetsAt))
			out, _, code := tr.runOpts(t, tr.clock(t, "1800000000"), Request{Action: "due", JSON: true})
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
// exit 0.
func TestAbsentLogReadsAsEmpty(t *testing.T) {
	tr := setup(t)
	// `due` exits 1 on an empty set, `list` 0; the header and the `[]` are the
	// arm's.
	cases := []struct {
		args   Request
		stdout string
		code   int
	}{
		{Request{Action: "list"}, renderHeader + "\n", 0},
		{Request{Action: "list", JSON: true}, "[]\n", 0},
		{Request{Action: "due"}, renderHeader + "\n", 1},
		{Request{Action: "due", JSON: true}, "[]\n", 1},
	}
	for _, tc := range cases {
		out, _, code := tr.run(t, tc.args)
		if out != tc.stdout {
			t.Errorf("%v printed %q, want %q", tc.args, out, tc.stdout)
		}
		if code != tc.code {
			t.Errorf("%v exited %d, want %d", tc.args, code, tc.code)
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
			for _, args := range []Request{{Action: "list", JSON: true}, {Action: "due", JSON: true}, {Action: "park", Default: "60"}} {
				out, err, code := tr.run(t, args)
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
	if _, err, code := tr.run(t, Request{Action: "list", JSON: true}); code != exitType ||
		!strings.Contains(err, "expected an object") {
		t.Errorf("list --json: code %d, stderr %q", code, err)
	}
	tr2 := setup(t)
	tr2.write(t, `{"ts":1,"crew_id":"c1","from":"d","to":"hold:c1","kind":"msg",`+
		`"body":"{\"id\":[\"arr\"],\"wait\":{\"resets_at\":1},\"task\":{}}"}`)
	if _, err, code := tr2.run(t, Request{Action: "list"}); code != exitType ||
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
				tr.write(t, seedHold(t, "c1", "h1", tc.resetsAt))
			}
			opts = tr.clock(t, "1800000000")
			out, err, code := tr.runOpts(t, opts, Request{Action: "park", Default: tc.def})
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
	tr.write(t, seedHold(t, "c1", "h1", "1800000000"))
	out, _, code := tr.runOpts(t, tr.clock(t, "1800000000"), Request{Action: "park", Default: "1"})
	if code != 0 || out != "1\n" {
		t.Errorf("park 1: %q (%d)", out, code)
	}
}

func TestParkRejectsABadDefault(t *testing.T) {
	tr := setup(t)
	want := "crew: hold park <default> must be a positive integer number of seconds\n"
	for _, def := range []string{"", "0", "-5", "abc", "1.5", "99999999999999999999"} {
		_, err, code := tr.run(t, Request{Action: "park", Default: def})
		if code != 1 || err != want {
			t.Errorf("park %q: code %d, stderr %q", def, code, err)
		}
	}
}

func TestDigitsLiteral(t *testing.T) {
	cases := map[string]string{"7": "7", "007": "7", "0": "0", "0000": "0", "1800000000": "1800000000"}
	for in, want := range cases {
		if got := digitsLiteral(in); got != want {
			t.Errorf("digitsLiteral(%q) = %q", in, got)
		}
	}
}

func TestOrNulAndElems(t *testing.T) {
	if orNull("") != nil || orNull("x") == nil || *orNull("x") != "x" {
		t.Error(`orNull: "" must be null and "x" a string`)
	}
	if elems("x") != nil {
		t.Error("elems of a non-array is no input at all")
	}
	if got := elems([]any{}); got == nil || len(got) != 0 {
		t.Error("elems of an empty array must be an empty slice, so `.` is []")
	}
}

// seedHold is a seed row shaped like `_build_hold`'s output, written straight to the
// bus so a test can plant a matured hold (`add` refuses a past --resets-at).
func seedHold(t *testing.T, crew, id, resetsAt string) string {
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

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	// Object keys are sorted here; the fold under test reads values, not order.
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// idsOf are the ids one of the arm's --json arrays prints, in its order.
func idsOf(t *testing.T, s string) []string {
	t.Helper()
	var held []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(s), &held); err != nil {
		t.Fatalf("not a JSON array (%v): %q", err, s)
	}
	ids := make([]string, len(held))
	for i, h := range held {
		ids[i] = h.ID
	}
	return ids
}

func jsonLen(t *testing.T, s string) int {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not a JSON array (%v): %q", err, s)
	}
	return len(v)
}

func TestWorkerAndRoleSessionsCannotWriteHolds(t *testing.T) {
	for _, env := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID"} {
		p := sender.Probes{Getenv: func(name string) string {
			if name == env {
				return "x"
			}
			return ""
		}}
		reason := sender.Check(t.Context(), "dispatcher:c1", "c1", p)
		if reason == "" {
			t.Fatalf("%s does not trip the sender check", env)
		}
		t.Run(env+"/add", func(t *testing.T) {
			tr := setup(t)
			spec := filepath.Join(tr.dir, "spec.md")
			if err := os.WriteFile(spec, []byte("the spec body\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err, code := tr.runOpts(t, Options{Sender: p}, fullAdd(func(a *AddRequest) { a.Spec, a.Title = spec, []string{"refused"} }))
			if want := "crew: hold add: refusing sender 'dispatcher:c1' — " + reason + "\n"; code != 1 || out != "" || err != want {
				t.Fatalf("code %d, stdout %q, stderr %q, want 1, %q", code, out, err, want)
			}
			if _, serr := os.Stat(tr.paths.Dir); !os.IsNotExist(serr) {
				t.Fatalf("bus dir created on a refused sender: %v", serr)
			}
		})
		t.Run(env+"/release", func(t *testing.T) {
			tr := setup(t)
			tr.write(t, seedHold(t, "c1", "h1", "9999999999"))
			_, err, code := tr.runOpts(t, Options{Sender: p}, Request{Action: "release", ID: "h1"})
			if want := "crew: hold release: refusing sender 'dispatcher:c1' — " + reason + "\n"; code != 1 || err != want {
				t.Fatalf("code %d, stderr %q, want 1, %q", code, err, want)
			}
			if rows := tr.rows(t); len(rows) != 1 {
				t.Fatalf("rows after a refused release: %d", len(rows))
			}
		})
	}
}
