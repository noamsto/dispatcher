package data

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stderr: []byte("boom\n"), Err: FakeExitError{Code: 1}}
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
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte("not json")}
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
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte(`[{"name":"w1"},{"name":"w2"}]`)}
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

// barrierRunner holds every call until n calls have arrived, so Collect only
// finishes before the context deadline if its sources really overlap.
type barrierRunner struct {
	inner *FakeRunner
	n     int

	mu      sync.Mutex
	arrived int
	all     chan struct{}
}

func newBarrierRunner(inner *FakeRunner, n int) *barrierRunner {
	return &barrierRunner{inner: inner, n: n, all: make(chan struct{})}
}

func (b *barrierRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return b.RunIn(ctx, "", name, args...)
}

func (b *barrierRunner) RunIn(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	return b.inner.RunIn(ctx, dir, name, args...)
}

// TestCollectRunsSourcesConcurrently: dispatch-config's two calls,
// refresh-budget, retro, rate and `crew crews` are six calls that must all
// be in flight at once.
func TestCollectRunsSourcesConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snap := Collect(ctx, newBarrierRunner(allOKRunner(t), 6), baseConfig())

	if ctx.Err() != nil {
		t.Fatal("Collect did not finish before the deadline: sources ran serially")
	}
	if snap.Settings.Error != nil || len(snap.Settings.Rows) == 0 || snap.Settings.Layers == nil {
		t.Errorf("settings not populated: %+v", snap.Settings)
	}
	if snap.Budget.Error != nil || snap.Budget.Report == nil {
		t.Errorf("budget not populated: %+v", snap.Budget)
	}
	if snap.Runs.RetroError != nil || snap.Runs.Retro == nil {
		t.Errorf("retro not populated: %+v", snap.Runs)
	}
	if snap.Runs.RatingsError != nil || len(snap.Runs.RatingsGroups) == 0 {
		t.Errorf("ratings not populated: %+v", snap.Runs)
	}
	if snap.Roster.Error != nil {
		t.Errorf("roster errored: %v", *snap.Roster.Error)
	}
}

// TestCollectRosterRunsCrewsConcurrentlyKeepsOrder gates the first crew's
// roster call until the second crew's has started, then checks the result
// still follows `crew crews` order.
func TestCollectRosterRunsCrewsConcurrentlyKeepsOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	gate := make(chan struct{})
	r := allOKRunner(t)
	r.Responses["crew crews"] = FakeResponse{
		Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t0\t1\tyes\nc2\t0\t0\t0\t1\tyes\n"),
	}
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte(`[{"name":"w1"}]`), Wait: gate}
	r.Responses["crew roster c2 --peers"] = FakeResponse{Stdout: []byte(`[{"name":"w2"}]`)}
	r.Responses["crew hold list --crew c1 --json"] = FakeResponse{Stdout: []byte("[]")}
	r.Responses["crew hold list --crew c2 --json"] = FakeResponse{Stdout: []byte("[]")}

	go func() {
		for ctx.Err() == nil {
			for _, c := range r.CallsSnapshot() {
				if fakeKey(c.Name, c.Args) == "crew roster c2 --peers" {
					close(gate)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	sec := CollectRoster(ctx, r, baseConfig())
	if len(sec.Crews) != 2 {
		t.Fatalf("expected 2 crews, got %d", len(sec.Crews))
	}
	if sec.Crews[0].ID != "c1" || sec.Crews[1].ID != "c2" {
		t.Errorf("crew order = %s, %s; want c1, c2", sec.Crews[0].ID, sec.Crews[1].ID)
	}
	if sec.Crews[0].WorkersError != nil {
		t.Errorf("c1 WorkersError = %v, want nil (c2 never started while c1 was gated)", *sec.Crews[0].WorkersError)
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

func twoCrewRunner(t *testing.T) *FakeRunner {
	t.Helper()
	r := allOKRunner(t)
	r.Responses["crew crews"] = FakeResponse{
		Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t0\t1\tyes\nc2\t0\t0\t0\t1\tyes\n"),
	}
	for _, id := range []string{"c1", "c2"} {
		r.Responses["crew hold list --crew "+id+" --json"] = FakeResponse{Stdout: []byte("[]")}
	}
	return r
}

func TestCollectRosterSplitsPeers(t *testing.T) {
	r := twoCrewRunner(t)
	peer := `{"name":"p","crew_id":"x","branch":"feat/p","host":"mbp","peer":true,"ts":1}`
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte(`[{"name":"local","branch":"feat/a"},{"name":"remote","host":"halo","branch":"feat/r"},` + peer + `]`)}
	r.Responses["crew roster c2 --peers"] = FakeResponse{Stdout: []byte(`[` + peer + `]`)}

	sec := CollectRoster(context.Background(), r, baseConfig())

	c1 := sec.Crews[0]
	if len(c1.Workers) != 2 || c1.Workers[0]["name"] != "local" || c1.Workers[1]["name"] != "remote" {
		t.Errorf("c1 Workers = %+v, want local and host rows only", c1.Workers)
	}
	if len(sec.Crews[1].Workers) != 0 {
		t.Errorf("c2 Workers = %+v, want none", sec.Crews[1].Workers)
	}
	if len(sec.Peers) != 1 || sec.Peers[0]["name"] != "p" {
		t.Errorf("Peers = %+v, want the peer row once", sec.Peers)
	}
}

func TestCollectRosterLane(t *testing.T) {
	cases := []struct {
		name      string
		resp      FakeResponse
		wantState string
		wantErr   bool
	}{
		{"alive", FakeResponse{Stdout: []byte(`{"stream":"status","state":"alive"}` + "\n")}, "alive", false},
		{"dead", FakeResponse{Stdout: []byte(`{"stream":"status","state":"dead"}` + "\n"), Err: FakeExitError{Code: 2}}, "dead", false},
		{"garbage", FakeResponse{Stdout: []byte("nope\n"), Stderr: []byte("bad\n"), Err: FakeExitError{Code: 1}}, "unknown", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := allOKRunner(t)
			r.Responses["crew crews"] = FakeResponse{
				Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t0\t1\tyes\n"),
			}
			r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte("[]")}
			r.Responses["crew hold list --crew c1 --json"] = FakeResponse{Stdout: []byte("[]")}
			r.Responses["crew stream --crew c1 --status"] = tc.resp

			lane := CollectRoster(context.Background(), r, baseConfig()).Crews[0].Lane
			if lane == nil || lane.State != tc.wantState {
				t.Fatalf("Lane = %+v, want state %q", lane, tc.wantState)
			}
			if (lane.Error != nil) != tc.wantErr {
				t.Errorf("Lane.Error = %v, want set=%v", lane.Error, tc.wantErr)
			}
		})
	}
}

func TestSnapshotJSONOmitsPeersAndLane(t *testing.T) {
	snap := Snapshot{Roster: RosterSection{
		Crews: []RosterCrew{{ID: "c1", Lane: &LaneStatus{State: "alive"}, Dispatcher: &DispatcherLoc{Where: "here"}}},
		Peers: []map[string]any{{"name": "p"}},
	}}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"peers"`, `"lane"`, `"dispatcher"`, `"Dispatcher"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("snapshot JSON contains %s: %s", key, b)
		}
	}
}

func TestCollectRosterLeavesAgeAbsentWithoutTS(t *testing.T) {
	r := twoCrewRunner(t)
	placeholder := `{"branch":null,"name":null,"state":null,"live":null,"host":"mbp","peer":true,"peer_error":"no snapshot"}`
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte(`[{"name":"w","branch":"feat/a","ts":1000},` + placeholder + `]`)}

	sec := CollectRoster(context.Background(), r, baseConfig())

	if _, ok := sec.Crews[0].Workers[0]["age_s"]; !ok {
		t.Errorf("worker with ts lost age_s: %+v", sec.Crews[0].Workers[0])
	}
	if len(sec.Peers) != 1 {
		t.Fatalf("Peers = %+v", sec.Peers)
	}
	if age, ok := sec.Peers[0]["age_s"]; ok {
		t.Errorf("placeholder has age_s = %v, want absent", age)
	}
}

func TestFakeRunnerRunInRecordsDir(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResponse{"crew x": {}}}
	if _, _, err := fr.RunIn(context.Background(), "/r/x", "crew", "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fr.Run(context.Background(), "crew", "x"); err != nil {
		t.Fatal(err)
	}
	calls := fr.CallsSnapshot()
	if len(calls) != 2 || calls[0].Dir != "/r/x" || calls[1].Dir != "" {
		t.Errorf("calls = %+v", calls)
	}
}

const foreignCrewsTSV = "crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n" +
	"c1\t0\t0\t1\t1\tyes\n" +
	"f1\t0\t0\t2\t\t\u2014\n" +
	"d1\t0\t0\t1\t\tno\n" +
	"i1\t0\t0\t0\t\tno\n"

const foreignFleet = `{"repos":[
 {"repo":"x","common":"/r/x/.git","crews":[{"crew_id":"d1","dispatcher":{"state":"dead"}}]},
 {"repo":"a","common":"/r/a/.git","crews":[{"crew_id":"f1","dispatcher":{"state":"live"}},{"crew_id":"d1","dispatcher":{"state":"closed"}}]}]}`

func foreignRunner(t *testing.T) *FakeRunner {
	t.Helper()
	r := allOKRunner(t)
	r.Responses["crew crews"] = FakeResponse{Stdout: []byte(foreignCrewsTSV)}
	r.Responses["crew crews --home"] = FakeResponse{Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t1\t1\tyes\n")}
	r.Responses["crew fleet status --json"] = FakeResponse{Stdout: []byte(foreignFleet)}
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte(`[{"name":"w0","branch":"feat/0"}]`)}
	r.Responses["crew roster f1 --peers"] = FakeResponse{Stdout: []byte(`[{"name":"w1","branch":"feat/1"},{"name":"w2","branch":"feat/2"}]`)}
	for _, id := range []string{"c1", "f1"} {
		r.Responses["crew hold list --crew "+id+" --json"] = FakeResponse{Stdout: []byte("[]")}
	}
	r.Responses["crew stream --crew c1 --status"] = FakeResponse{Stdout: []byte(`{"state":"alive"}`)}
	return r
}

func TestCollectRosterListsCrewWhoseDispatcherIsElsewhere(t *testing.T) {
	r := foreignRunner(t)
	sec := CollectRoster(context.Background(), r, baseConfig())

	if sec.Error != nil || len(sec.Crews) != 2 || sec.Crews[0].ID != "c1" || sec.Crews[1].ID != "f1" {
		t.Fatalf("roster = %+v", sec)
	}
	if d := sec.Crews[0].Dispatcher; d == nil || d.Where != "here" || d.Repo != nil {
		t.Errorf("c1 dispatcher = %+v", d)
	}
	f := sec.Crews[1]
	if d := f.Dispatcher; d == nil || d.Where != "elsewhere" || *d.Repo != "a" || *d.Common != "/r/a/.git" {
		t.Errorf("f1 dispatcher = %+v", d)
	}
	if len(f.Workers) != 2 || f.Lane != nil {
		t.Errorf("f1 workers = %+v lane = %+v", f.Workers, f.Lane)
	}
	for _, c := range r.CallsSnapshot() {
		if k := fakeKey(c.Name, c.Args); k == "crew stream --crew f1 --status" || strings.Contains(k, "d1") || strings.Contains(k, "i1") {
			t.Errorf("unexpected call %q", k)
		}
	}
}

func TestCollectRosterForeignLookupFailureOmitsCandidates(t *testing.T) {
	for _, key := range []string{"crew crews --home", "crew fleet status --json"} {
		t.Run(key, func(t *testing.T) {
			r := foreignRunner(t)
			r.Responses[key] = FakeResponse{Stderr: []byte("boom\n"), Err: FakeExitError{Code: 1}}
			sec := CollectRoster(context.Background(), r, baseConfig())
			if sec.Error != nil || sec.ForeignError == nil || *sec.ForeignError != "foreign crews: boom" || len(sec.Crews) != 1 || sec.Crews[0].ID != "c1" {
				t.Errorf("roster = %+v", sec)
			}
		})
	}
}

func TestCollectRosterNoCandidatesNoExtraCalls(t *testing.T) {
	r := twoCrewRunner(t)
	r.Responses["crew roster c1 --peers"] = FakeResponse{Stdout: []byte("[]")}
	r.Responses["crew roster c2 --peers"] = FakeResponse{Stdout: []byte("[]")}
	CollectRoster(context.Background(), r, baseConfig())
	for _, c := range r.CallsSnapshot() {
		if k := fakeKey(c.Name, c.Args); strings.Contains(k, "--home") || strings.Contains(k, "fleet") {
			t.Errorf("unexpected call %q", k)
		}
	}
}

func TestCollectRosterForeignCrewHomedHereIsNotListed(t *testing.T) {
	r := foreignRunner(t)
	r.Responses["crew crews --home"] = FakeResponse{Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc1\t0\t0\t1\t1\tyes\nf1\t0\t0\t2\t\t\u2014\n")}
	sec := CollectRoster(context.Background(), r, baseConfig())
	if len(sec.Crews) != 1 {
		t.Errorf("crews = %+v", sec.Crews)
	}
}

func TestCollectRosterForeignLookupTimeout(t *testing.T) {
	old := fleetStatusTimeout
	fleetStatusTimeout = 20 * time.Millisecond
	t.Cleanup(func() { fleetStatusTimeout = old })
	r := foreignRunner(t)
	r.Responses["crew fleet status --json"] = FakeResponse{Wait: make(chan struct{})}
	sec := CollectRoster(context.Background(), r, baseConfig())
	if sec.ForeignError == nil || *sec.ForeignError != "foreign crews: timed out after 20ms" {
		t.Errorf("ForeignError = %v", sec.ForeignError)
	}
}

func TestCollectRosterForeignLookupParentCancelIsNotTimeout(t *testing.T) {
	r := foreignRunner(t)
	r.Responses["crew fleet status --json"] = FakeResponse{Wait: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	sec := CollectRoster(ctx, r, baseConfig())
	if sec.ForeignError == nil || strings.Contains(*sec.ForeignError, "timed out") {
		t.Errorf("ForeignError = %v, want a non-timeout message", sec.ForeignError)
	}
}
