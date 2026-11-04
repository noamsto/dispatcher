package retro

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// fixture gives the tests a real repo: the arm resolves the bus through
// `git rev-parse`, so a temp dir with a .git is what `paths` must point at.
func fixture(t testing.TB, events string) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if events != "" {
		if err := os.WriteFile(filepath.Join(dir, ".git", "crew", "events.jsonl"), []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return bus.Paths{Common: filepath.Join(dir, ".git"), Dir: filepath.Join(dir, ".git", "crew"), Log: filepath.Join(dir, ".git", "crew", "events.jsonl")}
}

// oneRun is the bus the bats suite's happy path writes: one failed standard run
// with a gate_thrash note, one clean run that must produce no row at all.
const oneRun = `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"sonnet","tier":"standard","title":"t"}` + "\n" +
	`{"ts":1200,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"gate_thrash\",\"detail\":\"circled build\"}"}` + "\n" +
	`{"ts":1400,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n" +
	`{"ts":2000,"crew_id":"c1","kind":"dispatch","branch":"feat/clean","engine":"pi","model":"lemonade","tier":"trivial","title":"t"}` + "\n" +
	`{"ts":2100,"crew_id":"c1","kind":"status","from":"worker:feat/clean#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n"

func run(t *testing.T, events string, req Request) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(req, fixture(t, events), &out, &errb, Options{})
	return out.String(), errb.String(), code
}

func TestBareRowsMatchTheArm(t *testing.T) {
	out, errb, code := run(t, oneRun, Request{})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	// The arm's header, then one tab-separated row per run with notes; the
	// fixture's clean run stays silent.
	if out != "branch\tengine\tmodel\ttier\toutcome\ttags\nfeat/a\tclaude\tsonnet\tstandard\tfailed\tgate_thrash\n" {
		t.Errorf("stdout = %q", out)
	}
}

func TestReportTableMatchesTheArm(t *testing.T) {
	out, errb, code := run(t, oneRun, Request{Report: true})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	// The numeric columns are right-aligned (the arm's $left mask), so the
	// padding sits before the hits/runs/nondone cells.
	want := "tag          hits  runs  engines  nondone  sample\n" +
		"gate_thrash     1     1  claude       100  circled build\n"
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
}

func TestReportJSONIsFoldedAndOrdered(t *testing.T) {
	out, errb, code := run(t, oneRun, Request{Report: true, JSON: true})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	// The arm's jq printed the object pretty, at indent 2.
	if !strings.HasPrefix(out, "{\n  ") || !strings.HasSuffix(out, "}\n") {
		t.Errorf("--report --json is not indent-2 pretty-printed:\n%s", out)
	}
	var got struct {
		Tags    []map[string]any `json:"tags"`
		Unknown []string         `json:"unknown"`
		Rows    []struct {
			Kind   string `json:"kind"`
			Branch string `json:"branch"`
			Notes  []struct {
				Tag string `json:"tag"`
			} `json:"notes"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--report --json is not parseable: %v\n%s", err, out)
	}
	if len(got.Tags) != 1 || got.Tags[0]["tag"] != "gate_thrash" {
		t.Errorf("tags = %v", got.Tags)
	}
	if got.Tags[0]["known"] != true {
		t.Errorf("gate_thrash should be known: %v", got.Tags[0])
	}
	// nondone_pct is a number, and one of one run not done is 100.
	if pct, ok := got.Tags[0]["nondone_pct"].(float64); !ok || pct != 100 {
		t.Errorf("nondone_pct = %v", got.Tags[0]["nondone_pct"])
	}
	if len(got.Rows) != 1 || got.Rows[0].Branch != "feat/a" || got.Rows[0].Kind != "run" {
		t.Errorf("rows = %+v", got.Rows)
	}
}

func TestUnknownTagsAreListedOnce(t *testing.T) {
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"oops\",\"detail\":\"x\"}"}` + "\n" +
		`{"ts":1150,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"gate\",\"tag\":\"oops\",\"detail\":\"y\"}"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n"
	out, _, code := run(t, events, Request{Report: true})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "oops     2     1") {
		t.Errorf("the two hits should fold onto one row:\n%s", out)
	}
	if !strings.HasSuffix(out, "1 unrecognized tag(s): oops\n") {
		t.Errorf("unknown tag trailer missing:\n%s", out)
	}
	jsonOut, _, _ := run(t, events, Request{Report: true, JSON: true})
	if !strings.Contains(jsonOut, `unknown`) || !strings.Contains(jsonOut, `"oops"`) {
		t.Errorf("the JSON shape should list the unknown tag:\n%s", jsonOut)
	}
}

func TestBareRowCountsRepeatedTags(t *testing.T) {
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"other\",\"detail\":\"x\"}"}` + "\n" +
		`{"ts":1150,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"gate\",\"tag\":\"other\",\"detail\":\"y\"}"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n"
	out, _, _ := run(t, events, Request{})
	if !strings.HasSuffix(out, "failed\tother x2\n") {
		t.Errorf("the bare row counts repeats: %q", out)
	}
}

func TestCleanRunPrintsNoRow(t *testing.T) {
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n"
	out, errb, code := run(t, events, Request{})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	if out != "branch\tengine\tmodel\ttier\toutcome\ttags\n" {
		t.Errorf("a clean run is silent but printed %q", out)
	}
}

func TestNoLogIsSilent(t *testing.T) {
	// Not one byte — the arm exits before its header printf.
	for _, req := range []Request{{}, {Report: true}, {Report: true, JSON: true}} {
		out, errb, code := run(t, "", req)
		if code != 0 || out != "" || errb != "" {
			t.Errorf("%+v on a missing bus: exit %d, stdout %q, stderr %q", req, code, out, errb)
		}
	}
}

func TestJSONWithoutReportCostsOneLine(t *testing.T) {
	// The refusal never reaches the bus.
	out, errb, code := run(t, oneRun, Request{JSON: true})
	if code != 1 || out != "" || errb != Usage+"\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
}

func TestTornTailFailsTheWholeFold(t *testing.T) {
	// `jq -s` has no tolerance: the break costs every row, header included in
	// the bare mode because the arm prints it before jq starts.
	out, errb, code := run(t, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}`+
		"\n"+`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a`, Request{})
	if code != 5 {
		t.Fatalf("a torn tail should exit 5, got %d (%s)", code, errb)
	}
	if out != "branch\tengine\tmodel\ttier\toutcome\ttags\n" {
		t.Errorf("stdout = %q", out)
	}
	if !strings.HasPrefix(errb, "crew: retro: "+fixtureName(t, errb)+": ") {
		t.Errorf("stderr = %q", errb)
	}
}

func fixtureName(t *testing.T, errb string) string {
	t.Helper()
	parts := strings.SplitN(strings.TrimPrefix(errb, "crew: retro: "), ": ", 2)
	if len(parts) != 2 {
		t.Fatalf("stderr is not the arm's line: %q", errb)
	}
	return parts[0]
}

func TestUnreadableLogExitsTwo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this case uses to make the log unreadable")
	}
	paths := fixture(t, oneRun)
	if err := os.Chmod(paths.Log, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(paths.Log, 0o644) })
	var out, errb bytes.Buffer
	code := Run(Request{}, paths, &out, &errb, Options{})
	if code != 2 {
		t.Fatalf("an unreadable bus should exit 2, got %d (%s)", code, errb.String())
	}
	if !strings.HasPrefix(errb.String(), "crew: retro: ") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestCorruptBusIsOurOwnLine(t *testing.T) {
	// One `crew: retro: <log>: ...` line at exit 5, after the header the arm had
	// already printed.
	var out, errb bytes.Buffer
	paths := fixture(t, "{not json\n")
	code := Run(Request{}, paths, &out, &errb, Options{})
	if code != 5 {
		t.Fatalf("exit %d", code)
	}
	if out.String() != "branch\tengine\tmodel\ttier\toutcome\ttags\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.HasPrefix(errb.String(), "crew: retro: ") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestFoldTypeFailureExitsFive(t *testing.T) {
	// A non-object row has no `.kind` to index, and a note whose engine column
	// is an object has no place in a @tsv row. Both are the arm's exit 5, and
	// unlike the arm Go prints no row it had already streamed (docs/crew-go-port.md).
	out, errb, code := run(t, oneRun+`{"ts":3000,"crew_id":"c1","kind":"dispatch","branch":"feat/b","engine":{"nested":true},"model":"m","tier":"standard","title":"t"}`+
		"\n"+`{"ts":3100,"crew_id":"c1","kind":"msg","from":"worker:feat/b#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"other\",\"detail\":\"d\"}"}`+"\n", Request{})
	if code != 5 {
		t.Fatalf("a fold type error should exit 5, got %d (%s)", code, errb)
	}
	if out != "branch\tengine\tmodel\ttier\toutcome\ttags\n" {
		t.Errorf("stdout = %q", out)
	}
	if !strings.HasPrefix(errb, "crew: retro: ") {
		t.Errorf("stderr = %q", errb)
	}
}

func TestUnparseableNoteBodyDoesNotAbortTheFold(t *testing.T) {
	// One broken body costs that note, not the run (#25).
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"not json at all"}` + "\n" +
		`{"ts":1150,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"other\",\"detail\":\"kept\"}"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n"
	out, errb, code := run(t, events, Request{})
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errb)
	}
	if !strings.HasSuffix(out, "failed\tother\n") {
		t.Errorf("the readable note should survive: %q", out)
	}
}

func TestNonASCIIIsUntouched(t *testing.T) {
	// length/.[0:60] are codepoint math in jq and gojq alike, so a CJK detail
	// samples at the same place, and a branch name rides through unescaped.
	detail := strings.Repeat("日本語", 40)
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/日本語","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/日本語#s1","to":"retro:c1","body":` +
		jsonString(`{"seam":"execute","tag":"other","detail":"`+detail+`"}`) + `}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/日本語#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n"
	out, _, code := run(t, events, Request{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "feat/日本語\tclaude\tm\tstandard\tdone\tother") {
		t.Errorf("bare row: %q", out)
	}
	report, _, _ := run(t, events, Request{Report: true})
	if !strings.Contains(report, strings.Repeat("日本語", 20)+"…") {
		t.Errorf("the sample should cut at 60 codepoints:\n%s", report)
	}
}

func TestControlCharactersAreStripped(t *testing.T) {
	// `clean` turns tab/newline/CR into a space and drops the other control and
	// bidi characters, so a note cannot repaint the padded row above it. The
	// \u escapes are required: a raw control byte would make the bus line itself
	// invalid JSON, which jq rejects too.
	body := `{"seam":"execute","tag":"other","detail":"benign\u001b[1A\u001b[2KFAKE\u202eR\u2066"}`
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":` + jsonString(body) + `}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n"
	out, _, code := run(t, events, Request{Report: true})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.ContainsAny(out, "\x1b\x0e\x0f\u202e\u2066") {
		t.Errorf("control and bidi characters survived into the table:\n%q", out)
	}
	if !strings.Contains(out, "benign[1A[2KFAKER") {
		t.Errorf("the printable text should remain: %q", out)
	}
}

func TestCleanDropsInvisibleFixtureCases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "invisible", "testdata", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct{ Name, In, Escaped, Dropped string }
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		t.Run(r.Name, func(t *testing.T) {
			body := `{"seam":"execute","tag":"other","detail":` + jsonString(r.In) + `}`
			events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
				`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":` + jsonString(body) + `}` + "\n" +
				`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n"
			out, _, code := run(t, events, Request{Report: true})
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(out, r.Dropped) {
				t.Errorf("want %q in output: %q", r.Dropped, out)
			}
			if r.In != r.Dropped && strings.Contains(out, r.In) {
				t.Errorf("input %q survived: %q", r.In, out)
			}
		})
	}
}

func TestJSONIsNeverColoured(t *testing.T) {
	// This port prints plain, because that output is compared by value.
	out, _, code := run(t, oneRun, Request{Report: true, JSON: true})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("colour leaked: %q", out)
	}
}

// windowedBus exercises what the per-run lookups touch: feat/a is dispatched
// twice (the window splits at ts 5000), resume rows inside, before and outside
// every window, events without a `from`, a dispatcher `from`, a branch that was
// never dispatched, ties on ts across branches and within one, and plan-required
// runs that are flagged (feat/a's second window, feat/c) or saved by a resume
// (feat/a's first window) or a plan seam (feat/b).
const windowedBus = `{"ts":500,"crew_id":"c1","kind":"resume","branch":"feat/a"}` + "\n" +
	`{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"sonnet","tier":"standard","plan":"required","title":"t"}` + "\n" +
	`{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/b","engine":"claude","model":"opus","tier":"standard","plan":"required","title":"t"}` + "\n" +
	`{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/c","engine":"pi","model":"lemonade","tier":"standard","plan":"required","title":"t"}` + "\n" +
	`{"ts":1100,"crew_id":"c1","kind":"status","to":"dispatcher:c1","body":{"state":"working"}}` + "\n" +
	`{"ts":1100,"crew_id":"c1","kind":"status","from":"dispatcher:c1","to":"worker:feat/a#s1","body":{"state":"idle"}}` + "\n" +
	`{"ts":1150,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"metrics:c1","body":"{\"review_mode\":\"full\",\"plan_critic_first_pass\":null,\"notes\":[{\"seam\":\"gate\",\"tag\":\"other\",\"detail\":\"a-win1-snap\"}]}"}` + "\n" +
	`{"ts":1200,"crew_id":"c1","kind":"resume","branch":"feat/a"}` + "\n" +
	`{"ts":1250,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"gate_thrash\",\"detail\":\"a-win1-seam\"}"}` + "\n" +
	`{"ts":1300,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n" +
	`{"ts":1300,"crew_id":"c1","kind":"status","from":"worker:feat/orphan#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n" +
	`{"ts":1350,"crew_id":"c1","kind":"msg","from":"worker:feat/orphan#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"other\",\"detail\":\"orphan\"}"}` + "\n" +
	`{"ts":1400,"crew_id":"c1","kind":"resume","branch":"feat/zzz"}` + "\n" +
	`{"ts":2000,"crew_id":"c1","kind":"msg","from":"worker:feat/b#s1","to":"metrics:c1","body":"{\"review_mode\":\"full\",\"plan_critic_first_pass\":null,\"notes\":[{\"seam\":\"gate\",\"tag\":\"other\",\"detail\":\"b-snap\"}]}"}` + "\n" +
	`{"ts":2000,"crew_id":"c1","kind":"msg","from":"worker:feat/c#s1","to":"metrics:c1","body":"{\"review_mode\":\"full\",\"plan_critic_first_pass\":null,\"notes\":[{\"seam\":\"gate\",\"tag\":\"other\",\"detail\":\"c-snap\"}]}"}` + "\n" +
	`{"ts":2000,"crew_id":"c1","kind":"msg","from":"worker:feat/b#s1","to":"review:c1","body":"{\"seam\":\"plan\",\"plan_critic_first_pass\":\"accept\"}"}` + "\n" +
	`{"ts":2100,"crew_id":"c1","kind":"status","from":"worker:feat/b#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n" +
	`{"ts":2100,"crew_id":"c1","kind":"status","from":"worker:feat/c#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n" +
	`{"ts":3000,"crew_id":"c1","kind":"status","from":"feat/a#s9","to":"dispatcher:c1","body":{"state":"working"}}` + "\n" +
	`{"ts":4000,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n" +
	`{"ts":4000,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"stuck"}}` + "\n" +
	`{"ts":5000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"sonnet","tier":"standard","plan":"required","title":"t"}` + "\n" +
	`{"ts":5000,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s2","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"approach_abandoned\",\"detail\":\"a-boundary\"}"}` + "\n" +
	`{"ts":5050,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s2","to":"metrics:c1","body":"{\"review_mode\":\"full\",\"plan_critic_first_pass\":null,\"notes\":[]}"}` + "\n" +
	`{"ts":5100,"crew_id":"c1","kind":"status","from":"worker:feat/a#s2","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n" +
	`{"ts":6000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"retro:c1","body":"{\"seam\":\"dispatch\",\"tag\":\"fanout_binder\",\"detail\":\"d1\"}"}` + "\n" +
	`{"ts":6000,"crew_id":"c2","kind":"msg","from":"dispatcher:c2","to":"retro:c2","body":"{\"seam\":\"dispatch\",\"tag\":\"misrouted\",\"detail\":\"d2\"}"}` + "\n"

// The goldens are what the unindexed fold printed, captured before the
// per-run scans became lookups: the index must change speed, not output. The
// guard cases pin what the old filters did with a malformed row: a dispatch
// without a string branch matches nothing and costs nothing, and a non-string
// `from` aborts the fold only when some dispatch row makes it look at the bus.
func TestIndexedFoldMatchesOnWindowedBus(t *testing.T) {
	const header = "branch\tengine\tmodel\ttier\toutcome\ttags\n"
	foldError := [3]string{header, "", ""}
	cases := []struct {
		name   string
		events string
		code   int
		want   [3]string
	}{
		{"windowed", windowedBus, 0, windowedGolden},
		{"dispatch-without-branch", oneRun +
			`{"ts":2500,"crew_id":"c1","kind":"dispatch","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
			`{"ts":2600,"crew_id":"c1","kind":"status","to":"dispatcher:c1","body":{"state":"working"}}` + "\n" +
			`{"ts":2700,"crew_id":"c1","kind":"resume"}` + "\n", 0, oneRunGolden},
		{"dispatch-with-numeric-branch", oneRun +
			`{"ts":2500,"crew_id":"c1","kind":"dispatch","branch":42,"engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
			`{"ts":2600,"crew_id":"c1","kind":"status","from":"worker:42#s1","to":"dispatcher:c1","body":{"state":"working"}}` + "\n", 0, oneRunGolden},
		{"numeric-from-without-dispatch",
			`{"ts":1000,"crew_id":"c1","kind":"status","from":7,"to":"dispatcher:c1","body":{"state":"working"}}` + "\n" +
				`{"ts":1100,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"retro:c1","body":"{\"seam\":\"dispatch\",\"tag\":\"fanout_binder\",\"detail\":\"d1\"}"}` + "\n", 0, numericFromNoDispatchGolden},
		{"numeric-from-with-dispatch", oneRun +
			`{"ts":3000,"crew_id":"c1","kind":"status","from":7,"to":"dispatcher:c1","body":{"state":"working"}}` + "\n", 5, foldError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, req := range []Request{{}, {Report: true}, {Report: true, JSON: true}} {
				out, errb, code := run(t, tc.events, req)
				if code != tc.code {
					t.Fatalf("%+v: exit %d, want %d (%s)", req, code, tc.code, errb)
				}
				if out != tc.want[i] {
					t.Errorf("%+v: stdout =\n%s\nwant\n%s", req, out, tc.want[i])
				}
				if code != 0 && !strings.HasPrefix(errb, "crew: retro: ") {
					t.Errorf("%+v: stderr = %q", req, errb)
				}
			}
		})
	}
}

// Stdout of the bare, --report and --report --json shapes.
var (
	windowedGolden = [3]string{
		"branch\tengine\tmodel\ttier\toutcome\ttags\nfeat/a\tclaude\tsonnet\tstandard\tstuck\tgate_thrash,other\nfeat/a\tclaude\tsonnet\tstandard\tfailed\tapproach_abandoned,plan_required_unaudited\nfeat/b\tclaude\topus\tstandard\tdone\tother\nfeat/c\tpi\tlemonade\tstandard\tfailed\tother,plan_required_unaudited\ndispatcher:c1\t—\t—\t—\t—\tfanout_binder\ndispatcher:c2\t—\t—\t—\t—\tmisrouted\n",
		"tag                      hits  runs  engines    nondone  sample\ngate_thrash                 1     1  claude         100  a-win1-seam\napproach_abandoned          1     1  claude         100  a-boundary\nplan_required_unaudited     2     2  claude,pi      100  plan: required but no plan seam and plan_critic_first_pass i…\nother                       3     3  claude,pi       67  a-win1-snap\nmisrouted                   1     0  —                —  d2\nfanout_binder               1     0  —                —  d1\n",
		`{
  "rows": [
    {
      "branch": "feat/a",
      "crew": "c1",
      "engine": "claude",
      "kind": "run",
      "model": "sonnet",
      "notes": [
        {
          "detail": "a-win1-seam",
          "seam": "execute",
          "tag": "gate_thrash"
        },
        {
          "detail": "a-win1-snap",
          "seam": "gate",
          "tag": "other"
        }
      ],
      "outcome": "stuck",
      "t0": 1000,
      "tier": "standard"
    },
    {
      "branch": "feat/a",
      "crew": "c1",
      "engine": "claude",
      "kind": "run",
      "model": "sonnet",
      "notes": [
        {
          "detail": "a-boundary",
          "seam": "execute",
          "tag": "approach_abandoned"
        },
        {
          "detail": "plan: required but no plan seam and plan_critic_first_pass is null in the metrics snapshot",
          "seam": "plan",
          "tag": "plan_required_unaudited"
        }
      ],
      "outcome": "failed",
      "t0": 5000,
      "tier": "standard"
    },
    {
      "branch": "feat/b",
      "crew": "c1",
      "engine": "claude",
      "kind": "run",
      "model": "opus",
      "notes": [
        {
          "detail": "b-snap",
          "seam": "gate",
          "tag": "other"
        }
      ],
      "outcome": "done",
      "t0": 1000,
      "tier": "standard"
    },
    {
      "branch": "feat/c",
      "crew": "c1",
      "engine": "pi",
      "kind": "run",
      "model": "lemonade",
      "notes": [
        {
          "detail": "c-snap",
          "seam": "gate",
          "tag": "other"
        },
        {
          "detail": "plan: required but no plan seam and plan_critic_first_pass is null in the metrics snapshot",
          "seam": "plan",
          "tag": "plan_required_unaudited"
        }
      ],
      "outcome": "failed",
      "t0": 1000,
      "tier": "standard"
    },
    {
      "branch": "dispatcher:c1",
      "crew": "c1",
      "engine": "—",
      "kind": "dispatcher",
      "model": "—",
      "notes": [
        {
          "detail": "d1",
          "seam": "dispatch",
          "tag": "fanout_binder"
        }
      ],
      "outcome": "—",
      "t0": 6000,
      "tier": "—"
    },
    {
      "branch": "dispatcher:c2",
      "crew": "c2",
      "engine": "—",
      "kind": "dispatcher",
      "model": "—",
      "notes": [
        {
          "detail": "d2",
          "seam": "dispatch",
          "tag": "misrouted"
        }
      ],
      "outcome": "—",
      "t0": 6000,
      "tier": "—"
    }
  ],
  "tags": [
    {
      "details": [
        "a-win1-seam"
      ],
      "engines": [
        "claude"
      ],
      "hits": 1,
      "known": true,
      "nondone_pct": 100,
      "runs": 1,
      "tag": "gate_thrash"
    },
    {
      "details": [
        "a-boundary"
      ],
      "engines": [
        "claude"
      ],
      "hits": 1,
      "known": true,
      "nondone_pct": 100,
      "runs": 1,
      "tag": "approach_abandoned"
    },
    {
      "details": [
        "plan: required but no plan seam and plan_critic_first_pass is null in the metrics snapshot",
        "plan: required but no plan seam and plan_critic_first_pass is null in the metrics snapshot"
      ],
      "engines": [
        "claude",
        "pi"
      ],
      "hits": 2,
      "known": true,
      "nondone_pct": 100,
      "runs": 2,
      "tag": "plan_required_unaudited"
    },
    {
      "details": [
        "a-win1-snap",
        "b-snap",
        "c-snap"
      ],
      "engines": [
        "claude",
        "pi"
      ],
      "hits": 3,
      "known": true,
      "nondone_pct": 67,
      "runs": 3,
      "tag": "other"
    },
    {
      "details": [
        "d2"
      ],
      "engines": [],
      "hits": 1,
      "known": true,
      "nondone_pct": null,
      "runs": 0,
      "tag": "misrouted"
    },
    {
      "details": [
        "d1"
      ],
      "engines": [],
      "hits": 1,
      "known": true,
      "nondone_pct": null,
      "runs": 0,
      "tag": "fanout_binder"
    }
  ],
  "unknown": []
}
`,
	}
	oneRunGolden = [3]string{
		"branch\tengine\tmodel\ttier\toutcome\ttags\nfeat/a\tclaude\tsonnet\tstandard\tfailed\tgate_thrash\n",
		"tag          hits  runs  engines  nondone  sample\ngate_thrash     1     1  claude       100  circled build\n",
		`{
  "rows": [
    {
      "branch": "feat/a",
      "crew": "c1",
      "engine": "claude",
      "kind": "run",
      "model": "sonnet",
      "notes": [
        {
          "detail": "circled build",
          "seam": "execute",
          "tag": "gate_thrash"
        }
      ],
      "outcome": "failed",
      "t0": 1000,
      "tier": "standard"
    }
  ],
  "tags": [
    {
      "details": [
        "circled build"
      ],
      "engines": [
        "claude"
      ],
      "hits": 1,
      "known": true,
      "nondone_pct": 100,
      "runs": 1,
      "tag": "gate_thrash"
    }
  ],
  "unknown": []
}
`,
	}
	numericFromNoDispatchGolden = [3]string{
		"branch\tengine\tmodel\ttier\toutcome\ttags\ndispatcher:c1\t—\t—\t—\t—\tfanout_binder\n",
		"tag            hits  runs  engines  nondone  sample\nfanout_binder     1     0  —              —  d1\n",
		`{
  "rows": [
    {
      "branch": "dispatcher:c1",
      "crew": "c1",
      "engine": "—",
      "kind": "dispatcher",
      "model": "—",
      "notes": [
        {
          "detail": "d1",
          "seam": "dispatch",
          "tag": "fanout_binder"
        }
      ],
      "outcome": "—",
      "t0": 1100,
      "tier": "—"
    }
  ],
  "tags": [
    {
      "details": [
        "d1"
      ],
      "engines": [],
      "hits": 1,
      "known": true,
      "nondone_pct": null,
      "runs": 0,
      "tag": "fanout_binder"
    }
  ],
  "unknown": []
}
`,
	}
)

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
