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
