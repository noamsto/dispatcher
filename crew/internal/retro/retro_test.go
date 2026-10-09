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
func fixture(t *testing.T, events string) bus.Paths {
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

func run(t *testing.T, events string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, fixture(t, events), &out, &errb)
	return out.String(), errb.String(), code
}

func TestBareRowsMatchTheArm(t *testing.T) {
	out, errb, code := run(t, oneRun)
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
	out, errb, code := run(t, oneRun, "--report")
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
	out, errb, code := run(t, oneRun, "--report", "--json")
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
	out, _, code := run(t, events, "--report")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "oops     2     1") {
		t.Errorf("the two hits should fold onto one row:\n%s", out)
	}
	if !strings.HasSuffix(out, "1 unrecognized tag(s): oops\n") {
		t.Errorf("unknown tag trailer missing:\n%s", out)
	}
	jsonOut, _, _ := run(t, events, "--report", "--json")
	if !strings.Contains(jsonOut, `unknown`) || !strings.Contains(jsonOut, `"oops"`) {
		t.Errorf("the JSON shape should list the unknown tag:\n%s", jsonOut)
	}
}

func TestBareRowCountsRepeatedTags(t *testing.T) {
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"other\",\"detail\":\"x\"}"}` + "\n" +
		`{"ts":1150,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"gate\",\"tag\":\"other\",\"detail\":\"y\"}"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n"
	out, _, _ := run(t, events)
	if !strings.HasSuffix(out, "failed\tother x2\n") {
		t.Errorf("the bare row counts repeats: %q", out)
	}
}

func TestCleanRunPrintsNoRow(t *testing.T) {
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n"
	out, errb, code := run(t, events)
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	if out != "branch\tengine\tmodel\ttier\toutcome\ttags\n" {
		t.Errorf("a clean run is silent but printed %q", out)
	}
}

func TestNoLogIsSilent(t *testing.T) {
	// Not one byte — the arm exits before its header printf.
	for _, args := range [][]string{{}, {"--report"}, {"--report", "--json"}} {
		out, errb, code := run(t, "", args...)
		if code != 0 || out != "" || errb != "" {
			t.Errorf("%v on a missing bus: exit %d, stdout %q, stderr %q", args, code, out, errb)
		}
	}
}

func TestFlagErrorsCostOneLine(t *testing.T) {
	// The arm's flag loop and its `--json` without `--report` test share one
	// message and one status, and neither reaches the bus.
	for _, args := range [][]string{{"--json"}, {"--bogus"}, {"c1"}, {"--report", "--bogus"}} {
		out, errb, code := run(t, oneRun, args...)
		if code != 1 || out != "" || errb != usage+"\n" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out, errb)
		}
	}
}

func TestFlagsAreOrderFree(t *testing.T) {
	plain, _, _ := run(t, oneRun, "--report", "--json")
	swapped, _, _ := run(t, oneRun, "--json", "--report")
	if plain != swapped {
		t.Errorf("--json --report differs from --report --json:\n%s\n%s", plain, swapped)
	}
	twice, _, code := run(t, oneRun, "--report", "--report")
	if code != 0 || !strings.Contains(twice, "gate_thrash") {
		t.Errorf("a repeated flag should be harmless: exit %d, %q", code, twice)
	}
}

func TestTornTailFailsTheWholeFold(t *testing.T) {
	// `jq -s` has no tolerance: the break costs every row, header included in
	// the bare mode because the arm prints it before jq starts.
	out, errb, code := run(t, `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}`+
		"\n"+`{"ts":1100,"crew_id":"c1","kind":"msg","from":"worker:feat/a`)
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
	code := Run(nil, paths, &out, &errb)
	if code != 2 {
		t.Fatalf("an unreadable bus should exit 2, got %d (%s)", code, errb.String())
	}
	if !strings.HasPrefix(errb.String(), "crew: retro: ") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestCorruptBusIsOurOwnLine(t *testing.T) {
	// One `crew: retro: <log>: ...` line at exit 5, after the header the arm had
	// already printed — and no $JQ_COLORS warning, mirrored or not.
	t.Setenv("JQ_COLORS", "bad")
	var out, errb bytes.Buffer
	paths := fixture(t, "{not json\n")
	code := Run(nil, paths, &out, &errb)
	if code != 5 {
		t.Fatalf("exit %d", code)
	}
	if out.String() != "branch\tengine\tmodel\ttier\toutcome\ttags\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.HasPrefix(errb.String(), "crew: retro: ") || strings.Contains(errb.String(), "JQ_COLORS") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestFoldTypeFailureExitsFive(t *testing.T) {
	// A non-object row has no `.kind` to index, and a note whose engine column
	// is an object has no place in a @tsv row. Both are the arm's exit 5, and
	// unlike the arm Go prints no row it had already streamed (docs/crew-go-port.md).
	out, errb, code := run(t, oneRun+`{"ts":3000,"crew_id":"c1","kind":"dispatch","branch":"feat/b","engine":{"nested":true},"model":"m","tier":"standard","title":"t"}`+
		"\n"+`{"ts":3100,"crew_id":"c1","kind":"msg","from":"worker:feat/b#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"other\",\"detail\":\"d\"}"}`+"\n")
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
	out, errb, code := run(t, events)
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
	out, _, code := run(t, events)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "feat/日本語\tclaude\tm\tstandard\tdone\tother") {
		t.Errorf("bare row: %q", out)
	}
	report, _, _ := run(t, events, "--report")
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
	out, _, code := run(t, events, "--report")
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

func TestJSONIsNeverColoured(t *testing.T) {
	// jq paints a terminal's JSON; this port prints plain, because that output
	// is compared by value.
	for _, env := range []string{"", "0;32:1;33:0;36:1;35:0;34:1;32:1;35"} {
		t.Setenv("JQ_COLORS", env)
		out, _, code := run(t, oneRun, "--report", "--json")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if strings.Contains(out, "\x1b[") {
			t.Errorf("JQ_COLORS=%q leaked colour: %q", env, out)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
