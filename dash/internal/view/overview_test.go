package view_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

var update = flag.Bool("update", false, "rewrite golden JSON files")

func worker(t *testing.T, v view.OverviewView, branch string) view.OverviewWorker {
	t.Helper()
	for _, w := range v.Workers {
		if w.Branch == branch {
			return w
		}
	}
	t.Fatalf("no worker %q", branch)
	return view.OverviewWorker{}
}

func walk(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, c := range x {
			walk(c, fn)
		}
	case []any:
		for _, c := range x {
			walk(c, fn)
		}
	}
}

func TestPeerRowsHaveNoActions(t *testing.T) {
	raw, err := json.Marshal(view.BuildOverview(viewtest.Inputs()))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	peers := 0
	walk(doc, func(m map[string]any) {
		if m["origin"] != "peer" {
			return
		}
		peers++
		if acts, ok := m["actions"]; ok {
			if l, isList := acts.([]any); !isList || len(l) != 0 {
				t.Errorf("peer row actions = %v", acts)
			}
		}
		if act, ok := m["action"]; ok && act != nil {
			t.Errorf("peer attention action = %v", act)
		}
	})
	if peers < 3 {
		t.Errorf("only %d peer objects (worker, agent, attention expected)", peers)
	}
}

func TestRemoteUnknownFields(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	w := worker(t, v, "feat/r")
	if w.Origin != view.OriginRemote || w.Phase != nil || w.Acceptance != nil {
		t.Errorf("remote worker = %+v", w)
	}
	for _, a := range v.Agents {
		if a.Branch == "feat/r" && a.Roles != nil {
			t.Error("remote roles not null")
		}
	}
	for _, wd := range v.Monitors.Watchdogs {
		if wd.Branch == "feat/r" && wd.Activity != nil {
			t.Error("remote activity not null")
		}
	}
	item(t, v, "blocked:c1:feat/r")
}

func TestPhase(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	got := map[string]string{}
	for _, s := range worker(t, v, "feat/a").Phase.Steps {
		got[s.Name] = s.Status
	}
	want := map[string]string{"spec": "done", "plan": "done", "execute": "current", "gate": "pending", "review": "pending", "ship": "pending"}
	for k, s := range want {
		if got[k] != s {
			t.Errorf("feat/a %s = %s, want %s", k, got[k], s)
		}
	}
	for _, s := range worker(t, v, "feat/b").Phase.Steps {
		if s.Status != "done" {
			t.Errorf("pr_open step %s = %s", s.Name, s.Status)
		}
	}
	for _, s := range worker(t, v, "feat/j").Phase.Steps {
		if s.Name == "plan" && s.Status != "skipped" {
			t.Errorf("never-entered plan = %s", s.Status)
		}
	}
}

func TestAcceptance(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	if a := worker(t, v, "feat/b").Acceptance; a == nil || a.Done != 5 || a.Total != 5 {
		t.Errorf("feat/b acceptance = %+v", a)
	}
	if a := worker(t, v, "feat/a").Acceptance; a != nil {
		t.Errorf("feat/a acceptance = %+v, want nil", a)
	}
}

func TestBudgetPace(t *testing.T) {
	in := viewtest.Inputs()
	pts := func(n int64) *int64 { return &n }
	in.Snap.Budget.Report = &data.BudgetReport{Engines: map[string]*data.EngineBudget{
		"e": {Windows: []data.Window{
			{Key: "a", AheadPts: pts(5)}, {Key: "b", AheadPts: pts(-5)},
			{Key: "c", AheadPts: pts(0)}, {Key: "d"},
		}},
	}}
	got := map[string]string{}
	for _, b := range view.BuildOverview(in).Monitors.Budgets {
		got[b.Label] = b.Pace
	}
	want := map[string]string{"e a": "ahead", "e b": "behind", "e c": "on_pace", "e d": "n/a"}
	for k, p := range want {
		if got[k] != p {
			t.Errorf("pace %s = %q, want %q (%v)", k, got[k], p, got)
		}
	}
}

func TestActivityNonEmpty(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	for _, wd := range v.Monitors.Watchdogs {
		if wd.Branch != "feat/a" {
			continue
		}
		sum := 0
		for _, n := range wd.Activity {
			sum += n
		}
		if len(wd.Activity) != 30 || sum == 0 {
			t.Errorf("feat/a activity = %v", wd.Activity)
		}
		return
	}
	t.Error("no feat/a watchdog row")
}

func TestGoldenViews(t *testing.T) {
	for _, name := range view.Names {
		t.Run(name, func(t *testing.T) {
			v, err := view.Build(name, viewtest.Inputs())
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')

			var probe struct {
				SchemaVersion int    `json:"schema_version"`
				View          string `json:"view"`
			}
			if err := json.Unmarshal(got, &probe); err != nil {
				t.Fatal(err)
			}
			if probe.SchemaVersion != 1 || probe.View != name {
				t.Errorf("schema_version/view = %d/%q", probe.SchemaVersion, probe.View)
			}

			path := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(got) {
				t.Errorf("%s differs from golden; run go test ./internal/view -update and review the diff", name)
			}
		})
	}
}
