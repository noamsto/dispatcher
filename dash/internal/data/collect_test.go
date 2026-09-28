package data

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return b
}

func baseConfig() Config {
	return Config{
		Crew:              []string{"crew"},
		DispatchConfigBin: "dispatch-config",
		RefreshBudget:     "refresh-budget",
		Git:               "git",
		Now:               1790000000,
	}
}

// allOKRunner returns a FakeRunner whose five JSON sources all succeed with
// the captured contract fixtures, and whose `crews` call reports no live
// crew (empty roster).
func allOKRunner(t *testing.T) *FakeRunner {
	t.Helper()
	return &FakeRunner{
		Responses: map[string]FakeResponse{
			"dispatch-config --show-origin":  {Stdout: readTestdata(t, "settings_show_origin.json")},
			"dispatch-config --layers":       {Stdout: readTestdata(t, "settings_layers.json")},
			"refresh-budget --report --json": {Stdout: readTestdata(t, "budget_report.json")},
			"crew retro --report --json":     {Stdout: []byte("")},
			"crew rate --report --json":      {Stdout: readTestdata(t, "rate_report.json")},
			"crew crews":                     {Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n")},
		},
	}
}

func TestCollectAllSourcesOK(t *testing.T) {
	r := allOKRunner(t)
	snap := Collect(context.Background(), r, baseConfig())

	if snap.Now != 1790000000 {
		t.Errorf("Now = %d, want 1790000000", snap.Now)
	}
	if snap.Settings.Error != nil {
		t.Errorf("Settings.Error = %v, want nil", *snap.Settings.Error)
	}
	if len(snap.Settings.Rows) == 0 {
		t.Errorf("expected settings rows, got none")
	}
	if snap.Budget.Error != nil {
		t.Errorf("Budget.Error = %v, want nil", *snap.Budget.Error)
	}
	if snap.Budget.Report == nil || snap.Budget.Report.Engines["claude"] == nil {
		t.Errorf("expected a claude budget engine")
	}
	if snap.Runs.RetroError != nil {
		t.Errorf("RetroError = %v, want nil", *snap.Runs.RetroError)
	}
	if snap.Runs.Retro == nil || len(snap.Runs.Retro.Rows) != 0 {
		t.Errorf("expected the empty retro report for empty stdout, got %+v", snap.Runs.Retro)
	}
	if snap.Runs.RatingsError != nil {
		t.Errorf("RatingsError = %v, want nil", *snap.Runs.RatingsError)
	}
	if len(snap.Runs.Ratings) == 0 {
		t.Errorf("expected pass-through ratings JSON")
	}
	if snap.Roster.Error != nil {
		t.Errorf("Roster.Error = %v, want nil", *snap.Roster.Error)
	}
	if len(snap.Roster.Crews) != 0 {
		t.Errorf("expected no live crews, got %d", len(snap.Roster.Crews))
	}
}

// TestEachSourceDegradesAlone checks that a failing source degrades only its
// own pane; every other pane still collects normally.
func TestEachSourceDegradesAlone(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"settings", "dispatch-config --show-origin"},
		{"budget", "refresh-budget --report --json"},
		{"retro", "crew retro --report --json"},
		{"ratings", "crew rate --report --json"},
		{"roster", "crew crews"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := allOKRunner(t)
			r.Responses[tc.key] = FakeResponse{Stderr: []byte("boom\n"), Err: FakeExitError{Code: 1}}
			snap := Collect(context.Background(), r, baseConfig())

			switch tc.name {
			case "settings":
				if snap.Settings.Error == nil || *snap.Settings.Error != "boom" {
					t.Errorf("Settings.Error = %v, want \"boom\"", snap.Settings.Error)
				}
				if snap.Budget.Error != nil || snap.Runs.RetroError != nil || snap.Roster.Error != nil {
					t.Errorf("a settings failure must not degrade other panes: %+v", snap)
				}
			case "budget":
				if snap.Budget.Error == nil || *snap.Budget.Error != "boom" {
					t.Errorf("Budget.Error = %v, want \"boom\"", snap.Budget.Error)
				}
				if snap.Settings.Error != nil || snap.Runs.RetroError != nil || snap.Roster.Error != nil {
					t.Errorf("a budget failure must not degrade other panes: %+v", snap)
				}
			case "retro":
				if snap.Runs.RetroError == nil || *snap.Runs.RetroError != "boom" {
					t.Errorf("RetroError = %v, want \"boom\"", snap.Runs.RetroError)
				}
				if snap.Settings.Error != nil || snap.Budget.Error != nil || snap.Roster.Error != nil {
					t.Errorf("a retro failure must not degrade other panes: %+v", snap)
				}
			case "ratings":
				if snap.Runs.RatingsError == nil || *snap.Runs.RatingsError != "boom" {
					t.Errorf("RatingsError = %v, want \"boom\"", snap.Runs.RatingsError)
				}
				if snap.Settings.Error != nil || snap.Budget.Error != nil || snap.Roster.Error != nil {
					t.Errorf("a ratings failure must not degrade other panes: %+v", snap)
				}
			case "roster":
				if snap.Roster.Error == nil || *snap.Roster.Error != "boom" {
					t.Errorf("Roster.Error = %v, want \"boom\"", snap.Roster.Error)
				}
				if snap.Settings.Error != nil || snap.Budget.Error != nil || snap.Runs.RetroError != nil {
					t.Errorf("a roster failure must not degrade other panes: %+v", snap)
				}
			}
		})
	}
}

func TestSourceFailureWithNoStderrReportsExitCode(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["dispatch-config --show-origin"] = FakeResponse{Err: FakeExitError{Code: 3}}
	snap := Collect(context.Background(), r, baseConfig())
	if snap.Settings.Error == nil || *snap.Settings.Error != "exit 3" {
		t.Errorf("Settings.Error = %v, want \"exit 3\"", snap.Settings.Error)
	}
}

func TestRetroEmptyStdoutIsEmptyReportNotFailure(t *testing.T) {
	r := allOKRunner(t) // already stubs retro with empty stdout, rc 0
	snap := Collect(context.Background(), r, baseConfig())
	if snap.Runs.RetroError != nil {
		t.Fatalf("RetroError = %v, want nil", *snap.Runs.RetroError)
	}
	if snap.Runs.Retro == nil {
		t.Fatal("expected the empty retro report, got nil")
	}
	want := `{"tags":[],"unknown":[],"rows":[]}`
	got, _ := json.Marshal(snap.Runs.Retro)
	if string(got) != want {
		t.Errorf("empty retro report = %s, want %s", got, want)
	}
}

// TestRetroReportWithoutRowsField checks that an older crew build whose
// retro --report --json carries no "rows" field does not panic and folds to
// zero rows, not an error.
func TestRetroReportWithoutRowsField(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["crew retro --report --json"] = FakeResponse{Stdout: []byte(`{"tags":[],"unknown":[]}`)}
	snap := Collect(context.Background(), r, baseConfig())
	if snap.Runs.RetroError != nil {
		t.Fatalf("RetroError = %v, want nil", *snap.Runs.RetroError)
	}
	if snap.Runs.Retro == nil || len(snap.Runs.Retro.Rows) != 0 {
		t.Errorf("expected zero rows, got %+v", snap.Runs.Retro)
	}
}

func TestCollectRetroRows(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["crew retro --report --json"] = FakeResponse{Stdout: readTestdata(t, "retro_rows.json")}
	snap := Collect(context.Background(), r, baseConfig())
	if snap.Runs.RetroError != nil {
		t.Fatalf("RetroError = %v, want nil", *snap.Runs.RetroError)
	}
	if len(snap.Runs.Retro.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(snap.Runs.Retro.Rows))
	}
	run := snap.Runs.Retro.Rows[0]
	if run.Kind != "run" || run.Crew == nil || *run.Crew != "c1" || run.Branch != "feat/x" {
		t.Errorf("unexpected run row: %+v", run)
	}
	if len(run.Notes) != 1 || run.Notes[0].Tag != "gate_thrash" {
		t.Errorf("unexpected run notes: %+v", run.Notes)
	}
}

func TestSnapshotJSONRoundTripsCapturedContract(t *testing.T) {
	r := allOKRunner(t)
	// This fixture's own retro/settings testdata was captured with an empty
	// bus (no events.jsonl), so `crews` alive-only listing is empty too —
	// exactly the scenario the captured snapshot.json fixture describes.
	snap := Collect(context.Background(), r, baseConfig())

	got, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var gotAny, wantAny map[string]any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	want := readCapturedSnapshot(t)
	if err := json.Unmarshal(want, &wantAny); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	for _, key := range []string{"now", "settings", "budget", "roster"} {
		if !jsonEqual(t, gotAny[key], wantAny[key]) {
			t.Errorf("snapshot[%q] mismatch:\ngot:  %#v\nwant: %#v", key, gotAny[key], wantAny[key])
		}
	}
}

func readCapturedSnapshot(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../once/testdata/snapshot.json")
	if err != nil {
		t.Fatalf("reading captured snapshot.json: %v", err)
	}
	return b
}

// TestSettingsLayersUnmarshalFailureWarns checks that a --layers response
// that is valid JSON but fails to unmarshal into Layers is not dropped
// silently — it appends a "layers: <err>" warning instead.
func TestSettingsLayersUnmarshalFailureWarns(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["dispatch-config --layers"] = FakeResponse{Stdout: []byte(`{"base": 123}`)}
	snap := Collect(context.Background(), r, baseConfig())
	if snap.Settings.Error != nil {
		t.Fatalf("Settings.Error = %v, want nil", *snap.Settings.Error)
	}
	if snap.Settings.Layers != nil {
		t.Errorf("Layers = %+v, want nil on unmarshal failure", snap.Settings.Layers)
	}
	var found bool
	for _, w := range snap.Settings.Warnings {
		if strings.HasPrefix(w, "layers: ") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want a \"layers: ...\" warning", snap.Settings.Warnings)
	}
}

// TestRosterPerCrewFailureSetsError checks that `crew crews` reporting one
// alive crew, but `crew roster <id>` itself failing, surfaces as
// RosterCrew.Error, not an empty (indistinguishable-from-idle) worker list.
func TestRosterPerCrewFailureSetsError(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["crew crews"] = FakeResponse{
		Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t0\t1\tyes\n"),
	}
	r.Responses["crew roster c1"] = FakeResponse{Stderr: []byte("boom\n"), Err: FakeExitError{Code: 1}}
	r.Responses["crew hold list --crew c1 --json"] = FakeResponse{Stdout: []byte("[]")}
	snap := Collect(context.Background(), r, baseConfig())

	if len(snap.Roster.Crews) != 1 {
		t.Fatalf("expected 1 crew, got %d", len(snap.Roster.Crews))
	}
	c := snap.Roster.Crews[0]
	if c.WorkersError == nil || *c.WorkersError != "boom" {
		t.Errorf("Crew.WorkersError = %v, want \"boom\"", c.WorkersError)
	}
	if len(c.Workers) != 0 {
		t.Errorf("Workers = %+v, want empty on a roster failure", c.Workers)
	}
}

// TestRosterPerCrewUnparseableJSONSetsError checks the "unparseable JSON"
// case: rc 0 but a body that isn't a JSON array.
func TestRosterPerCrewUnparseableJSONSetsError(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["crew crews"] = FakeResponse{
		Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t0\t1\tyes\n"),
	}
	r.Responses["crew roster c1"] = FakeResponse{Stdout: []byte("not json")}
	r.Responses["crew hold list --crew c1 --json"] = FakeResponse{Stdout: []byte("[]")}
	snap := Collect(context.Background(), r, baseConfig())

	if len(snap.Roster.Crews) != 1 {
		t.Fatalf("expected 1 crew, got %d", len(snap.Roster.Crews))
	}
	if snap.Roster.Crews[0].WorkersError == nil {
		t.Errorf("expected Crew.WorkersError to be set for unparseable roster JSON")
	}
}

// TestRosterHoldsFailureIsIndependentOfWorkers covers the split of
// RosterCrew.Error into WorkersError/HoldsError: a `crew roster <id>`
// success alongside a `crew hold list` failure must not blank out the
// workers that did load, and must not be reported as a WorkersError.
func TestRosterHoldsFailureIsIndependentOfWorkers(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["crew crews"] = FakeResponse{
		Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t0\t1\tyes\n"),
	}
	r.Responses["crew roster c1"] = FakeResponse{Stdout: []byte(`[{"name":"w1"},{"name":"w2"}]`)}
	r.Responses["crew hold list --crew c1 --json"] = FakeResponse{Stderr: []byte("hold boom\n"), Err: FakeExitError{Code: 1}}
	snap := Collect(context.Background(), r, baseConfig())

	if len(snap.Roster.Crews) != 1 {
		t.Fatalf("expected 1 crew, got %d", len(snap.Roster.Crews))
	}
	c := snap.Roster.Crews[0]
	if c.WorkersError != nil {
		t.Errorf("WorkersError = %v, want nil", *c.WorkersError)
	}
	if c.HoldsError == nil || *c.HoldsError != "hold boom" {
		t.Errorf("HoldsError = %v, want \"hold boom\"", c.HoldsError)
	}
	if len(c.Workers) != 2 {
		t.Errorf("Workers len = %d, want 2", len(c.Workers))
	}
}

// TestRatingsWrongShapeSetsError checks that `rate --report --json` exiting
// 0 with valid-but-wrong-shape JSON (an object instead of the documented
// array) does not silently render as "no runs swept".
func TestRatingsWrongShapeSetsError(t *testing.T) {
	r := allOKRunner(t)
	r.Responses["crew rate --report --json"] = FakeResponse{Stdout: []byte(`{"groups":[]}`)}
	snap := Collect(context.Background(), r, baseConfig())

	if snap.Runs.RatingsError == nil {
		t.Fatal("expected RatingsError to be set for wrong-shape ratings JSON")
	}
	if !strings.HasPrefix(*snap.Runs.RatingsError, "unparseable crew rate output:") {
		t.Errorf("RatingsError = %q, want prefix \"unparseable crew rate output:\"", *snap.Runs.RatingsError)
	}
	if len(snap.Runs.RatingsGroups) != 0 {
		t.Errorf("RatingsGroups = %+v, want empty on a decode failure", snap.Runs.RatingsGroups)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var an, bn any
	_ = json.Unmarshal(ab, &an)
	_ = json.Unmarshal(bb, &bn)
	return reflect.DeepEqual(an, bn)
}
