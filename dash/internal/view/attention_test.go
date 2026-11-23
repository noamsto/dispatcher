package view_test

import (
	"maps"
	"slices"
	"strings"
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
		{"hold_matured:c1:h-due", "hold_matured", "warning", "inspect", "", []string{"crew", "hold", "list", "--crew", "c1"}},
		{"blocked:halo/c1:feat/r", "blocked", "warning", "reply", "r", nil},
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
	a := item(t, view.BuildOverview(viewtest.Inputs()), "blocked:mbp/c1:feat/p")
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

// setWorker edits the fixture roster row of branch in place.
func setWorker(in view.Inputs, branch string, fields map[string]any) {
	for _, c := range in.Snap.Roster.Crews {
		for _, w := range c.Workers {
			if w["branch"] == branch {
				maps.Copy(w, fields)
			}
		}
	}
}

func TestAttentionBlockedAgeStartsAtBlockedSince(t *testing.T) {
	in := viewtest.Inputs()
	setWorker(in, "feat/c", map[string]any{"blocked_since": float64(1794800000000)})
	v := view.BuildOverview(in)
	if a := item(t, v, "blocked:c1:feat/c"); a.AgeSec != 600 {
		t.Errorf("age_sec = %d, want 600 from blocked_since, not the 300s re-stamp age", a.AgeSec)
	}
	if w := worker(t, v, "feat/c"); w.BlockedSince == nil || *w.BlockedSince != 1794800000000 || w.AnsweredAt != nil {
		t.Errorf("worker blocked_since/answered_at = %v/%v", w.BlockedSince, w.AnsweredAt)
	}
}

func TestAttentionAnsweredWorkerHasNoItem(t *testing.T) {
	in := viewtest.Inputs()
	setWorker(in, "feat/c", map[string]any{"state": "answered", "answered_at": float64(1794800400000)})
	v := view.BuildOverview(in)
	if slices.Contains(ids(v), "blocked:c1:feat/c") {
		t.Errorf("answered worker still needs attention: %v", ids(v))
	}
	if w := worker(t, v, "feat/c"); w.State != "answered" || w.AnsweredAt == nil || *w.AnsweredAt != 1794800400000 {
		t.Errorf("worker = %+v", w)
	}
}

func staleFields() map[string]any {
	return map[string]any{
		"needs_cleanup":    true,
		"cleanup_reason":   "uncommitted changes; issue closed",
		"cleanup_worktree": "/w/h",
		"cleanup_inspect":  "git -C '/w/h' status --short",
		"cleanup_remove":   "git worktree remove --force '/w/h' && git branch -D 'feat/h'",
	}
}

func TestAttentionStaleReplacesFailed(t *testing.T) {
	base := view.BuildOverview(viewtest.Inputs())
	in := viewtest.Inputs()
	setWorker(in, "feat/h", staleFields())
	v := view.BuildOverview(in)

	if slices.Contains(ids(v), "failed:c1:feat/h") {
		t.Errorf("stale worker still raises failed: %v", ids(v))
	}
	a := item(t, v, "stale:c1:feat/h")
	if a.Kind != view.KindStale || a.Severity != view.SeverityWarning {
		t.Errorf("kind/severity = %s/%s", a.Kind, a.Severity)
	}
	if want := "stale: needs human cleanup — uncommitted changes; issue closed"; a.Message != want {
		t.Errorf("message = %q, want %q", a.Message, want)
	}
	if want := "inspect: git -C '/w/h' status --short; remove: git worktree remove --force '/w/h' && git branch -D 'feat/h'"; a.Clears != want {
		t.Errorf("clears = %q, want %q", a.Clears, want)
	}
	if len(v.Attention) != len(base.Attention) {
		t.Errorf("attention = %d, want %d (stale replaces failed)", len(v.Attention), len(base.Attention))
	}

	w := worker(t, v, "feat/h")
	if !w.NeedsCleanup {
		t.Error("worker not flagged needs_cleanup")
	}
	if v.Workers[len(v.Workers)-1].Branch != "feat/h" {
		t.Errorf("stale worker not sorted last: %s", v.Workers[len(v.Workers)-1].Branch)
	}
	if v.Summary.Workers != base.Summary.Workers {
		t.Errorf("workers = %d, want %d", v.Summary.Workers, base.Summary.Workers)
	}
	if v.Summary.Agents != base.Summary.Agents-1 || len(v.Agents) != len(base.Agents)-1 {
		t.Errorf("agents = %d/%d, want one fewer than %d/%d", v.Summary.Agents, len(v.Agents), base.Summary.Agents, len(base.Agents))
	}
	for _, ag := range v.Agents {
		if ag.Branch == "feat/h" {
			t.Error("stale worker listed as an agent")
		}
	}
}

func TestAttentionReapedStaleRowYieldsNoItem(t *testing.T) {
	in := viewtest.Inputs()
	fields := staleFields()
	fields["state"] = "reaped"
	setWorker(in, "feat/h", fields)
	v := view.BuildOverview(in)
	for _, a := range v.Attention {
		if a.Branch == "feat/h" {
			t.Errorf("reaped row produced %s", a.ID)
		}
	}
	for _, w := range v.Workers {
		if w.Branch == "feat/h" {
			t.Error("reaped row listed")
		}
	}
}

func TestAttentionStaleFarHostPointsAtReapDryRun(t *testing.T) {
	in := viewtest.Inputs()
	setWorker(in, "feat/h", map[string]any{
		"host": "halo", "needs_cleanup": true, "cleanup_reason": "uncommitted changes; issue closed",
	})
	a := item(t, view.BuildOverview(in), "stale:halo/c1:feat/h")
	if strings.Contains(a.Clears, "inspect: ;") || !strings.Contains(a.Clears, "crew reap --dry-run") {
		t.Errorf("clears = %q", a.Clears)
	}
}

func TestAttentionStaleWithoutReasonHasNoDash(t *testing.T) {
	in := viewtest.Inputs()
	setWorker(in, "feat/h", map[string]any{"needs_cleanup": true})
	if a := item(t, view.BuildOverview(in), "stale:c1:feat/h"); a.Message != "stale: needs human cleanup" {
		t.Errorf("message = %q", a.Message)
	}
}

func TestAttentionMigratedIsNotAttention(t *testing.T) {
	in := viewtest.Inputs()
	in.Snap.Roster.Crews[0].Workers = append(in.Snap.Roster.Crews[0].Workers,
		map[string]any{"branch": "feat/mig", "state": "migrated", "to_host": "nuc", "host": "", "live": false})
	v := view.BuildOverview(in)
	for _, a := range v.Attention {
		if a.Branch == "feat/mig" {
			t.Errorf("migrated worker produced %s", a.ID)
		}
	}
	w := worker(t, v, "feat/mig")
	if w.Detail == nil || *w.Detail != "moved to nuc" {
		t.Errorf("migrated detail = %v, want %q", w.Detail, "moved to nuc")
	}
	if v.Summary.ByState["migrated"] != 1 {
		t.Errorf("by_state = %v", v.Summary.ByState)
	}
	for _, wd := range v.Monitors.Watchdogs {
		if wd.Branch == "feat/mig" {
			t.Error("migrated worker is watched")
		}
	}

	fin := viewtest.FleetInputs()
	c := crewOf(fin, "/r/a/.git", "c1")
	c.Workers = append(c.Workers, map[string]any{"branch": "feat/mig", "state": "migrated", "to_host": "nuc", "host": ""})
	for _, a := range view.BuildFleet(fin).Attention {
		if a.Branch == "feat/mig" {
			t.Errorf("fleet attention for migrated worker: %s", a.ID)
		}
	}
}

// laneInputs is a one-crew roster whose lane is in state; peers are dropped so
// only the crew under test can raise items.
func laneInputs(id, state string, workers []map[string]any, holds []map[string]any) view.Inputs {
	in := viewtest.Inputs()
	in.Snap.Roster.Peers = nil
	in.Snap.Roster.Crews = []data.RosterCrew{{
		ID: id, Workers: workers, Holds: holds,
		Lane: &data.LaneStatus{State: state}, Dispatcher: &data.DispatcherLoc{Where: "here"},
	}}
	return in
}

func laneWorker(state string) map[string]any {
	return map[string]any{"branch": "feat/w", "name": "feat/w", "state": state, "tier": "standard", "engine": "claude", "live": true, "age_s": float64(30)}
}

func maturedHold() map[string]any {
	return map[string]any{"id": "h1", "wait": map[string]any{"kind": "quota", "resets_at": float64(1794800000)}}
}

func hasLane(v view.OverviewView) bool {
	return slices.ContainsFunc(v.Attention, func(a view.AttentionItem) bool {
		return a.Kind == view.KindLaneDead || a.Kind == view.KindLaneStale
	})
}

func TestLaneItemsNeedActNow(t *testing.T) {
	const (
		old    = "1794000000-123"
		recent = "1794800500-123"
		past   = "1794800200-123"
	)
	work, done := []map[string]any{laneWorker("working")}, []map[string]any{laneWorker("done")}
	cases := []struct {
		name    string
		id      string
		state   string
		workers []map[string]any
		holds   []map[string]any
		where   string
		want    view.Kind
	}{
		{"dead, nothing running", old, "dead", nil, nil, "here", ""},
		{"dead, terminal workers only", old, "dead", done, nil, "here", ""},
		{"dead with a working worker", old, "dead", work, nil, "here", view.KindLaneDead},
		{"stale with a working worker", old, "stale", work, nil, "here", view.KindLaneStale},
		{"matured hold, no worker", old, "dead", nil, []map[string]any{maturedHold()}, "here", view.KindLaneDead},
		{"launched 100s ago", recent, "dead", work, nil, "here", ""},
		{"launched 400s ago", past, "dead", work, nil, "here", view.KindLaneDead},
		{"unparseable id", "c1", "dead", work, nil, "here", view.KindLaneDead},
		{"dispatcher elsewhere", old, "dead", work, nil, "elsewhere", ""},
		{"no dispatcher", old, "dead", work, nil, "none", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := laneInputs(c.id, c.state, c.workers, c.holds)
			in.Snap.Roster.Crews[0].Dispatcher.Where = c.where
			v := view.BuildOverview(in)
			var got view.Kind
			for _, a := range v.Attention {
				if a.Kind == view.KindLaneDead || a.Kind == view.KindLaneStale {
					got = a.Kind
				}
			}
			if got != c.want {
				t.Errorf("lane item = %q, want %q (items %v)", got, c.want, ids(v))
			}
		})
	}
}

func TestLaneGraceFollowsDispatcherSince(t *testing.T) {
	for name, tc := range map[string]struct {
		ago  int64
		want bool
	}{"relaunched 100s ago": {100, false}, "relaunched 400s ago": {400, true}} {
		t.Run(name, func(t *testing.T) {
			in := laneInputs("1794000000-1", "dead", []map[string]any{laneWorker("working")}, nil)
			since := in.NowMS/1000 - tc.ago
			in.Snap.Roster.Crews[0].Dispatcher.Since = &since
			if got := hasLane(view.BuildOverview(in)); got != tc.want {
				t.Errorf("lane item = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLaneItemWithoutDispatcherProbe(t *testing.T) {
	in := laneInputs("1794000000-1", "dead", []map[string]any{laneWorker("working")}, nil)
	in.Snap.Roster.Crews[0].Dispatcher = nil
	if v := view.BuildOverview(in); !hasLane(v) {
		t.Errorf("unprobed crew lost its lane item: %v", ids(v))
	}
}

func TestRoleFailedNeedsWorkingWorker(t *testing.T) {
	for state, want := range map[string]bool{"done": false, "working": true} {
		in := viewtest.Inputs()
		setWorker(in, "feat/a", map[string]any{"state": state})
		got := slices.Contains(ids(view.BuildOverview(in)), "role_failed:c1:feat/a")
		if got != want {
			t.Errorf("state %s: role_failed raised = %v, want %v", state, got, want)
		}
	}
}
