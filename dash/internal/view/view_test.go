package view

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func TestOrigin(t *testing.T) {
	cases := []struct {
		row  map[string]any
		want Origin
	}{
		{map[string]any{}, OriginLocal},
		{map[string]any{"host": "halo"}, OriginRemote},
		{map[string]any{"host": "mbp", "peer": true}, OriginPeer},
	}
	for _, c := range cases {
		if got := OriginOf(c.row); got != c.want {
			t.Errorf("OriginOf(%v) = %q, want %q", c.row, got, c.want)
		}
	}
}

func verbs(acts []Action) []string {
	var v []string
	for _, a := range acts {
		v = append(v, a.Key)
	}
	return v
}

func TestActionsByOrigin(t *testing.T) {
	crew := []string{"crew"}
	pr := 12
	if got := verbs(WorkerActions(OriginLocal, crew, "c1", "feat/a", "otter", nil)); !slices.Equal(got, []string{"p", "n", "r"}) {
		t.Errorf("local no PR keys = %v", got)
	}
	if got := verbs(WorkerActions(OriginRemote, crew, "c1", "feat/a", "", &pr)); !slices.Equal(got, []string{"p", "n", "r", "m"}) {
		t.Errorf("remote PR keys = %v", got)
	}
	peer := WorkerActions(OriginPeer, crew, "c1", "feat/a", "", &pr)
	if peer == nil || len(peer) != 0 {
		t.Errorf("peer actions = %#v, want empty non-nil", peer)
	}
}

func TestCommandQuoting(t *testing.T) {
	pr := 12
	acts := WorkerActions(OriginLocal, []string{"crew"}, "c1", "feat/a", "", &pr)
	byKey := map[string]Action{}
	for _, a := range acts {
		byKey[a.Key] = a
	}
	reply := byKey["r"]
	if want := []string{"crew", "reply", "--crew", "c1", "--", "worker:feat/a", "<text>"}; !slices.Equal(reply.Argv, want) {
		t.Errorf("reply argv = %v", reply.Argv)
	}
	if want := "crew reply --crew c1 -- worker:feat/a '<text>'"; reply.Command != want || !reply.NeedsInput {
		t.Errorf("reply command = %q needs_input=%v", reply.Command, reply.NeedsInput)
	}
	merge := byKey["m"]
	if want := []string{"crew", "merge", "12", "--crew", "c1"}; !slices.Equal(merge.Argv, want) {
		t.Errorf("merge argv = %v", merge.Argv)
	}
	if merge.Command != "crew merge 12 --crew c1" || merge.Label != "squash-merge" {
		t.Errorf("merge = %+v", merge)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
	if got := byKey["n"].Argv; got[len(got)-1] != "feat/a" {
		t.Errorf("nudge target = %v", got)
	}
}

func TestPRNumber(t *testing.T) {
	if n := PRNumber("https://github.com/o/r/pull/12"); n == nil || *n != 12 {
		t.Errorf("PRNumber = %v", n)
	}
	for _, u := range []string{"", "https://github.com/o/r/issues/12", "nope"} {
		if PRNumber(u) != nil {
			t.Errorf("PRNumber(%q) != nil", u)
		}
	}
}

func fullInputs(t *testing.T) Inputs {
	t.Helper()
	b, err := os.ReadFile("../ui/testdata/snapshot-full.json")
	if err != nil {
		t.Fatal(err)
	}
	var snap data.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	return Inputs{Snap: snap, NowMS: snap.Now * 1000, Crew: []string{"crew"}, Loaded: Loaded{true, true, true, true, true}}
}

func TestBuildRoster(t *testing.T) {
	in := fullInputs(t)
	v := BuildRoster(in)
	if v.SchemaVersion != 1 || v.View != "roster" || len(v.Crews) != len(in.Snap.Roster.Crews) {
		t.Fatalf("view = %+v", v)
	}
	if len(v.Crews[0].Workers) != 2 || v.Crews[0].Workers[0].Origin != OriginLocal {
		t.Errorf("c1 workers = %+v", v.Crews[0].Workers)
	}
	if len(v.Crews[0].Holds) != 1 || v.Crews[0].Holds[0].ID != "h1" {
		t.Errorf("holds = %+v", v.Crews[0].Holds)
	}
	if h := v.Crews[0].Holds[0]; h.Matured != (*h.ResetAt <= in.Snap.Now) {
		t.Errorf("matured = %v", h.Matured)
	}
	out, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, ok := m["peers"].([]any); !ok {
		t.Errorf("peers not []: %s", out)
	}
}

func TestHoldMatured(t *testing.T) {
	h := func(kind string, at float64) map[string]any {
		return map[string]any{"id": "h", "wait": map[string]any{"kind": kind, "resets_at": at}}
	}
	if !HoldRowOf(h("window", 100), 100).Matured {
		t.Error("window at now should be matured")
	}
	if HoldRowOf(h("window", 101), 100).Matured || HoldRowOf(h("fleet", 1), 100).Matured {
		t.Error("future window / fleet must not be matured")
	}
}

func TestBuildTabs(t *testing.T) {
	in := fullInputs(t)
	if v := BuildBudget(in); v.View != "budget" || v.Warnings == nil || v.Now != in.Snap.Now {
		t.Errorf("budget = %+v", v)
	}
	if v := BuildRuns(in); v.View != "runs" || v.RatingsGroups == nil {
		t.Errorf("runs = %+v", v)
	}
	if v := BuildSettings(in); v.View != "settings" || v.Rows == nil || v.Warnings == nil {
		t.Errorf("settings = %+v", v)
	}
	m := BuildMap(in)
	if m.View != "map" || len(m.Workers) != 2 || m.Workers[0].Crew != "c1" {
		t.Errorf("map = %+v", m)
	}
}

func TestBuildDispatch(t *testing.T) {
	in := fullInputs(t)
	for _, n := range []string{"roster", "budget", "runs", "settings", "map"} {
		if _, err := Build(n, in); err != nil {
			t.Errorf("Build(%s): %v", n, err)
		}
	}
	if _, err := Build("nope", in); err == nil {
		t.Error("unknown view should error")
	}
}
