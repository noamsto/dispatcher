package once

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

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

	golden, err := os.ReadFile("testdata/once.golden")
	if err != nil {
		t.Fatalf("reading golden: %v", err)
	}
	got := Render(snap, false)
	if got != string(golden) {
		t.Errorf("Render(color=false) mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

func TestRenderMatchesColorGolden(t *testing.T) {
	snap := loadSnapshot(t)

	golden, err := os.ReadFile("testdata/once-color.golden")
	if err != nil {
		t.Fatalf("reading color golden: %v", err)
	}
	got := Render(snap, true)
	if got != string(golden) {
		t.Errorf("Render(color=true) mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

// TestBackslashNotDoubled guards the once-shows-a-backslash-once-escaped
// bats regression (#10): a settings value with one literal backslash must
// render as the single json-escaped "a\\b", never doubled.
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

// TestLargeValueDoesNotPanic guards the ARG_MAX-adjacent bats regression
// (#11): a very large settings value must still render all four sections.
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

// TestCJKNotTruncated guards bats regression #7: --once never truncates a
// long locked value, CJK included.
func TestCJKNotTruncated(t *testing.T) {
	snap := loadSnapshot(t)
	needle := "ディレクトリ/and-more"
	if !strings.Contains(Render(snap, false), needle) {
		t.Errorf("expected untruncated CJK value %q in output", needle)
	}
}

// TestRosterCrewErrorShown matches finding 2: a per-crew roster/hold source
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

// TestRosterHoldsErrorShownWorkersStillRender matches the WorkersError/
// HoldsError split: a holds-only failure must still render the crew's
// worker table under --once, alongside a "holds unavailable: <err>" line.
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

// TestRosterPRURLInjectionIsCleaned matches fix 2: a worker's raw "pr_url"
// field must not carry a terminal escape or bidi override into the
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

// TestTagsLineInjectionIsCleaned matches fix 3: a retro note's Tag must not
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

// TestRunsPaneRatingsWrongShapeIsUnavailable matches finding 3: a wrong-shape
// (but valid) Ratings payload must decode-fail, not silently render as "no
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
