package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

const scopeE2ENowMS = 1794800600000

func scopeE2ERunner() *data.FakeRunner {
	r := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew crews": {Stdout: []byte("crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n" +
			"c1\t0\t0\t1\t1\tyes\nc2\t0\t0\t1\t1\tyes\n")},
	}}
	for _, id := range []string{"c1", "c2"} {
		worker := fmt.Sprintf(`[{"name":"w-%[1]s","branch":"feat/%[1]s","state":"blocked","detail":"needs input","live":true,"age_s":30}]`, id)
		r.Responses["crew roster "+id+" --peers"] = data.FakeResponse{Stdout: []byte(worker)}
		r.Responses["crew hold list --crew "+id+" --json"] = data.FakeResponse{Stdout: []byte("[]")}
		r.Responses["crew stream --crew "+id+" --status"] = data.FakeResponse{Stdout: []byte(`{"state":"dead"}`)}
	}
	return r
}

func scopeE2EDigest(t *testing.T) data.BusDigest {
	t.Helper()
	var sb strings.Builder
	for i, id := range []string{"c1", "c2"} {
		ts := scopeE2ENowMS - int64(60_000*(i+1))
		fmt.Fprintf(&sb, `{"ts":%d,"crew_id":%q,"from":"worker:feat/%s#s1","to":"dispatcher:%s","kind":"status","body":{"state":"blocked","detail":"needs input"}}`+"\n", ts, id, id, id)
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := data.NewBusScanner(path).Scan(scopeE2ENowMS)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestScopeEndToEnd(t *testing.T) {
	asciiProfile(t)
	digest := scopeE2EDigest(t)
	cfg := func() data.Config {
		return data.Config{Crew: []string{"crew"}, Now: scopeE2ENowMS / 1000}
	}
	build := func(t *testing.T, cfg data.Config) (Model, vm.OverviewView) {
		t.Helper()
		roster := data.CollectRoster(context.Background(), scopeE2ERunner(), cfg)
		for i := range roster.Crews {
			roster.Crews[i].Lane = &data.LaneStatus{State: "dead"}
		}
		m := headerModel(t, roster.Scope, roster.Crews...)
		m.snap.Roster = roster
		m.digest = digest
		in := vm.Inputs{
			Snap:   m.snap,
			Bus:    digest,
			Loaded: vm.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
			NowMS:  scopeE2ENowMS,
			Crew:   cfg.Crew,
		}
		return m, vm.BuildOverview(in)
	}

	t.Run("scoped to c1", func(t *testing.T) {
		c := cfg()
		c.CrewID, c.CrewIDSource = "c1", "flag"
		m, ov := build(t, c)
		header := stripped(m.crewHeader())
		if !strings.Contains(header, "c1") || strings.Contains(header, "c2") {
			t.Errorf("header = %q, want c1 only", header)
		}
		if ov.Scope.Crew == nil || *ov.Scope.Crew != "c1" {
			t.Errorf("scope.crew = %v, want c1", ov.Scope.Crew)
		}
		if len(ov.Attention) == 0 || len(ov.Workers) == 0 || len(ov.Events) == 0 {
			t.Fatalf("empty lanes: attention=%d workers=%d events=%d", len(ov.Attention), len(ov.Workers), len(ov.Events))
		}
		for _, a := range ov.Attention {
			if a.Crew != "c1" {
				t.Errorf("attention item %+v outside c1", a)
			}
		}
		for _, w := range ov.Workers {
			if w.Crew != "c1" {
				t.Errorf("worker %s/%s outside c1", w.Crew, w.Branch)
			}
		}
		for _, e := range ov.Events {
			if e.Crew != "c1" {
				t.Errorf("event %+v outside c1", e)
			}
		}
	})

	t.Run("unscoped", func(t *testing.T) {
		m, ov := build(t, cfg())
		if header := stripped(m.crewHeader()); !strings.Contains(header, "unscoped (2 crews)") {
			t.Errorf("header = %q, want unscoped (2 crews)", header)
		}
		if ov.Scope.Crew != nil {
			t.Errorf("scope.crew = %q, want nil", *ov.Scope.Crew)
		}
		seen := map[string]map[string]bool{"attention": {}, "workers": {}, "events": {}}
		for _, a := range ov.Attention {
			seen["attention"][a.Crew] = true
		}
		for _, w := range ov.Workers {
			seen["workers"][w.Crew] = true
		}
		for _, e := range ov.Events {
			seen["events"][e.Crew] = true
		}
		for lane, crews := range seen {
			if !crews["c1"] || !crews["c2"] {
				t.Errorf("%s crews = %v, want c1 and c2", lane, crews)
			}
		}
	})
}
