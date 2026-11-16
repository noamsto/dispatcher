package view_test

import (
	"slices"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func item(t *testing.T, v view.OverviewView, id string) view.AttentionItem {
	t.Helper()
	for _, a := range v.Attention {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no attention item %q in %v", id, ids(v))
	return view.AttentionItem{}
}

func ids(v view.OverviewView) []string {
	var out []string
	for _, a := range v.Attention {
		out = append(out, a.ID)
	}
	return out
}

func TestAttentionKinds(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	cases := []struct {
		id, kind, severity, verb, key string
		argv                          []string
	}{
		{"blocked:c1:feat/c", "blocked", "warning", "reply", "r", nil},
		{"prompt:c1:feat/d", "prompt", "warning", "peek", "p", nil},
		{"stall:c1:feat/e", "stall", "warning", "nudge", "n", nil},
		{"watchdog:c1:feat/f", "watchdog", "warning", "peek", "p", nil},
		{"dead:c1:feat/g", "dead", "critical", "peek", "p", nil},
		{"failed:c1:feat/h", "failed", "critical", "peek", "p", nil},
		{"role_failed:c1:feat/a", "role_failed", "critical", "peek", "p", []string{"crew", "where", "--crew", "c1", "--", "feat/a"}},
		{"merge_ready:c1:feat/b", "merge_ready", "ready", "merge", "m", []string{"crew", "merge", "12", "--crew", "c1"}},
		{"lane_dead:c2:", "lane_dead", "critical", "inspect", "", []string{"crew", "stream", "--crew", "c2", "--status"}},
		{"hold_matured:c1:h-due", "hold_matured", "warning", "inspect", "", []string{"crew", "hold", "due", "--crew", "c1"}},
		{"blocked:c1:feat/r", "blocked", "warning", "reply", "r", nil},
	}
	for _, c := range cases {
		a := item(t, v, c.id)
		if string(a.Kind) != c.kind || string(a.Severity) != c.severity {
			t.Errorf("%s: kind/severity = %s/%s", c.id, a.Kind, a.Severity)
		}
		if a.Action == nil {
			t.Errorf("%s: nil action", c.id)
			continue
		}
		if a.Action.Verb != c.verb || a.Action.Key != c.key {
			t.Errorf("%s: action = %s/%q", c.id, a.Action.Verb, a.Action.Key)
		}
		if c.argv != nil && !slices.Equal(a.Action.Argv, c.argv) {
			t.Errorf("%s: argv = %v, want %v", c.id, a.Action.Argv, c.argv)
		}
	}
}

func TestAttentionPeerFallback(t *testing.T) {
	a := item(t, view.BuildOverview(viewtest.Inputs()), "blocked:c1:feat/p")
	if a.Origin != view.OriginPeer || a.Action != nil || a.Severity != view.SeverityWarning {
		t.Errorf("peer item = %+v", a)
	}
}

func TestAttentionExcludes(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	for _, a := range v.Attention {
		if a.Branch == "feat/i" {
			t.Errorf("reaped worker produced %s", a.ID)
		}
		if a.Branch == "feat/c" && a.Kind == view.KindRoleFailed {
			t.Errorf("'no assignment' role failure produced %s", a.ID)
		}
	}
	for _, w := range v.Workers {
		if w.Branch == "feat/i" {
			t.Error("reaped worker listed")
		}
	}
	if len(v.Attention) != 12 {
		t.Errorf("attention = %v", ids(v))
	}
}

func TestAttentionSortAndIDs(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	rank := map[view.Severity]int{view.SeverityCritical: 0, view.SeverityWarning: 1, view.SeverityReady: 2}
	seen := map[string]bool{}
	for i, a := range v.Attention {
		if seen[a.ID] {
			t.Errorf("duplicate id %s", a.ID)
		}
		seen[a.ID] = true
		if i == 0 {
			continue
		}
		p := v.Attention[i-1]
		if rank[p.Severity] > rank[a.Severity] || (p.Severity == a.Severity && p.AgeSec < a.AgeSec) {
			t.Errorf("order broken between %s and %s", p.ID, a.ID)
		}
	}
	again := view.BuildOverview(viewtest.Inputs())
	if !slices.Equal(ids(v), ids(again)) {
		t.Error("ids unstable across rebuilds")
	}
}

func TestMergeReadyNeedsLedger(t *testing.T) {
	in := viewtest.Inputs()
	for _, w := range in.Bus.Workers {
		w.Ledger = ""
	}
	for _, a := range view.BuildOverview(in).Attention {
		if a.Kind == view.KindMergeReady {
			t.Errorf("merge_ready without a ledger: %s", a.ID)
		}
	}
}

func TestLaneStaleRaisesWarning(t *testing.T) {
	in := viewtest.Inputs()
	in.Snap.Roster.Crews[0].Lane = &data.LaneStatus{State: "stale"}
	a := item(t, view.BuildOverview(in), "lane_stale:c1:")
	if a.Severity != view.SeverityWarning || a.Message != "crew stream lane is stale" || a.Action == nil || a.Action.Key != "" ||
		!slices.Equal(a.Action.Argv, []string{"crew", "stream", "--crew", "c1", "--status"}) {
		t.Errorf("lane_stale = %+v action %+v", a, a.Action)
	}
}
