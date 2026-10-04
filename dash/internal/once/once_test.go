package once

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

var updateGolden = flag.Bool("update", false, "update golden files")

// checkGolden compares got against testdata/name, or rewrites it under -update
// (the same mechanism dash/internal/ui/golden_test.go uses).
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v (run with -update to generate)", path, err)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

func loadSnapshot(t *testing.T) data.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot.json")
	if err != nil {
		t.Fatalf("reading testdata/snapshot.json: %v", err)
	}
	var snap data.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshaling snapshot.json: %v", err)
	}
	return snap
}

func TestRenderMatchesGolden(t *testing.T) {
	snap := loadSnapshot(t)
	checkGolden(t, "once.golden", Render(snap, false))
}

func TestRenderMatchesColorGolden(t *testing.T) {
	snap := loadSnapshot(t)
	checkGolden(t, "once-color.golden", Render(snap, true))
}

// TestEngineLinesLimitReachedAndUnlimited checks the renderer surfaces the two
// states the text report() prints: an unlimited plan (no window table) and a
// limit-reached reason, with the reason run through cleanText.
func TestEngineLinesLimitReachedAndUnlimited(t *testing.T) {
	eb := &data.EngineBudget{
		Source:       "usage_summary",
		Unlimited:    true,
		LimitReached: []byte(`{"reason":"plan\tusage at 100%"}`),
	}
	var texts []string
	for _, l := range engineLines("cursor", eb) {
		texts = append(texts, l.text)
	}
	out := strings.Join(texts, "\n")
	if !strings.Contains(out, "unlimited") {
		t.Fatalf("unlimited line missing:\n%s", out)
	}
	if !strings.Contains(out, "limit reached: plan usage at 100%") {
		t.Fatalf("cleaned limit-reached reason missing:\n%s", out)
	}
}

// TestBackslashNotDoubled checks that a settings value with one literal
// backslash renders as the single json-escaped "a\\b", never doubled.
func TestBackslashNotDoubled(t *testing.T) {
	snap := loadSnapshot(t)
	snap.Settings.Rows = []data.SettingRow{
		{Path: []string{"repoTrackers", "noamsto/dispatcher"}, Value: json.RawMessage(`"a\\b"`), Origin: "user"},
	}
	got := Render(snap, false)
	// The row's JSON value is `"a\\b"` (one escaped backslash); the rendered
	// text must carry that same single escape, not a doubled `"a\\\\b"`.
	if !strings.Contains(got, `noamsto/dispatcher: "a\\b"`) {
		t.Errorf("expected a single-escaped backslash in output, got:\n%s", got)
	}
	if strings.Contains(got, `noamsto/dispatcher: "a\\\\b"`) {
		t.Errorf("backslash was doubled in output:\n%s", got)
	}
}

// TestLargeValueDoesNotPanic checks that a very large settings value
// (ARG_MAX-adjacent) still renders all four sections.
func TestLargeValueDoesNotPanic(t *testing.T) {
	snap := loadSnapshot(t)
	big := make([]byte, 300000)
	for i := range big {
		big[i] = 'a'
	}
	bigJSON, err := json.Marshal(string(big))
	if err != nil {
		t.Fatal(err)
	}
	snap.Settings.Rows = []data.SettingRow{
		{Path: []string{"profile"}, Value: bigJSON, Origin: "user"},
	}
	got := Render(snap, false)
	for _, want := range []string{"== Settings ==", "== Budget ==", "== Runs ==", "== Roster =="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing section %q", want)
		}
	}
}

// TestCJKNotTruncated checks that --once never truncates a long locked
// value, CJK included.
func TestCJKNotTruncated(t *testing.T) {
	snap := loadSnapshot(t)
	needle := "ディレクトリ/and-more"
	if !strings.Contains(Render(snap, false), needle) {
		t.Errorf("expected untruncated CJK value %q in output", needle)
	}
}

// TestRosterCrewErrorShown checks that a per-crew roster/hold source
// failure surfaces as "unavailable: <error>" under that crew's heading,
// instead of an empty worker list indistinguishable from an idle crew.
func TestRosterCrewErrorShown(t *testing.T) {
	snap := loadSnapshot(t)
	msg := "boom"
	snap.Roster.Crews = []data.RosterCrew{{ID: "c1", WorkersError: &msg}}
	got := Render(snap, false)
	if !strings.Contains(got, "unavailable: boom") {
		t.Errorf("crew roster failure should read unavailable: boom:\n%s", got)
	}
}

// TestRosterHoldsErrorShownWorkersStillRender checks that a holds-only
// failure still renders the crew's worker table under --once, alongside a
// "holds unavailable: <err>" line.
func TestRosterHoldsErrorShownWorkersStillRender(t *testing.T) {
	snap := loadSnapshot(t)
	msg := "hold boom"
	snap.Roster.Crews = []data.RosterCrew{{
		ID: "c1",
		Workers: []map[string]any{
			{"name": "w1", "branch": "feat/x", "state": "working", "tier": "deep", "engine": "claude", "model": "opus", "age_s": float64(5), "pr_url": "-"},
		},
		HoldsError: &msg,
	}}
	got := Render(snap, false)
	if !strings.Contains(got, "w1") {
		t.Errorf("holds-only failure should still render the crew's workers:\n%s", got)
	}
	if !strings.Contains(got, "holds unavailable: hold boom") {
		t.Errorf("holds-only failure should read holds unavailable: hold boom:\n%s", got)
	}
}

// TestRosterPRURLInjectionIsCleaned checks that a worker's raw "pr_url"
// field does not carry a terminal escape or bidi override into the
// rendered pr column under --once.
func TestRosterPRURLInjectionIsCleaned(t *testing.T) {
	snap := loadSnapshot(t)
	snap.Roster.Crews = []data.RosterCrew{{
		ID: "c1",
		Workers: []map[string]any{
			{"name": "w1", "branch": "feat/x", "state": "working", "tier": "deep", "engine": "claude", "model": "opus", "age_s": float64(5), "pr_url": "\x1b[2Jhttps://pr‮"},
		},
	}}
	got := Render(snap, false)
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("rendered roster pane contains a raw ESC from an injected pr_url:\n%q", got)
	}
	if strings.ContainsRune(got, 0x202e) {
		t.Errorf("rendered roster pane contains U+202E from an injected pr_url:\n%q", got)
	}
}

// TestTagsLineInjectionIsCleaned checks that a retro note's Tag does not
// carry a raw terminal escape or bidi override into the "tags:" line
// tagstrDash formats under --once.
func TestTagsLineInjectionIsCleaned(t *testing.T) {
	snap := loadSnapshot(t)
	crew := "c1"
	snap.Runs.Retro = &data.RetroReport{Rows: []data.RetroRow{{
		Kind: "dispatcher",
		Crew: &crew,
		T0:   1,
		Notes: []data.Note{
			{Tag: "session_summary", Detail: "did stuff"},
			{Tag: "tag\x1b]0;x\x07‮", Detail: "detail"},
		},
	}}}
	got := Render(snap, false)
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("rendered runs pane contains a raw ESC from an injected tag:\n%q", got)
	}
	if strings.ContainsRune(got, 0x202e) {
		t.Errorf("rendered runs pane contains U+202E from an injected tag:\n%q", got)
	}
}

// TestRunsPaneRatingsWrongShapeIsUnavailable checks that a wrong-shape (but
// valid) Ratings payload decode-fails, rather than silently rendering as "no
// runs swept for this repo yet". Unmarshaling into data.RunsSection directly
// exercises the same decode path Collect uses.
func TestRunsPaneRatingsWrongShapeIsUnavailable(t *testing.T) {
	var runs data.RunsSection
	raw := []byte(`{"retro":null,"retro_error":null,"ratings":{"groups":[]},"ratings_error":null}`)
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if runs.RatingsError == nil {
		t.Fatalf("expected the RunsSection decode itself to set RatingsError")
	}

	snap := loadSnapshot(t)
	snap.Runs = runs
	got := Render(snap, false)
	if !strings.Contains(got, "unavailable: unparseable crew rate output:") {
		t.Errorf("wrong-shape ratings should read unavailable: unparseable crew rate output: ...:\n%s", got)
	}
}
