package view_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

const (
	keyA1 = "lap|/r/a/.git|c1"
	keyB1 = "lap|/r/b/.git|c1"
	keyA3 = "lap|/r/a/.git|c3"
	keyA4 = "lap|/r/a/.git|c4"
	keyA6 = "lap|/r/a/.git|c6"
	keyB5 = "lap|/r/b/.git|c5"
)

func fleetCrew(t *testing.T, v view.FleetView, key string) view.FleetCrewRow {
	t.Helper()
	for _, c := range v.Crews {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("no crew row %q in %v", key, crewKeys(v))
	return view.FleetCrewRow{}
}

func crewKeys(v view.FleetView) []string {
	var out []string
	for _, c := range v.Crews {
		out = append(out, c.Key)
	}
	return out
}

func fleetItem(t *testing.T, v view.FleetView, id string) view.AttentionItem {
	t.Helper()
	for _, a := range v.Attention {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no attention item %q in %v", id, fleetIDs(v))
	return view.AttentionItem{}
}

func fleetIDs(v view.FleetView) []string {
	var out []string
	for _, a := range v.Attention {
		out = append(out, a.ID)
	}
	return out
}

func fleetHost(t *testing.T, v view.FleetView, name string) view.FleetHostRow {
	t.Helper()
	for _, h := range v.Hosts {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no host %q", name)
	return view.FleetHostRow{}
}

func crewOf(in view.FleetInputs, common, crew string) *data.FleetCrew {
	for i := range in.Fleet.Status.Repos {
		r := &in.Fleet.Status.Repos[i]
		if r.Common != common {
			continue
		}
		for j := range r.Crews {
			if r.Crews[j].CrewID == crew {
				return &r.Crews[j]
			}
		}
	}
	panic("no crew " + crew)
}

func strp(s string) *string { return &s }

func TestFleetActiveFilter(t *testing.T) {
	v := view.BuildFleet(viewtest.FleetInputs())
	for _, key := range []string{keyA1, keyB1, keyA3, keyB5} {
		fleetCrew(t, v, key)
	}
	if slices.Contains(crewKeys(v), keyA4) {
		t.Errorf("closed crew with only terminal workers shown: %v", crewKeys(v))
	}
	if v.Summary.HiddenCrews != 2 || v.Summary.Crews != countHeads(v) {
		t.Errorf("summary crews/hidden = %d/%d, rows %d", v.Summary.Crews, v.Summary.HiddenCrews, len(v.Crews))
	}

	in := viewtest.FleetInputs()
	c4 := crewOf(in, "/r/a/.git", "c4")
	c4.Workers = append(c4.Workers, map[string]any{"branch": "feat/d3", "state": nil, "host": "far", "host_error": "timed out"})
	v = view.BuildFleet(in)
	if c := fleetCrew(t, v, keyA4); !c.Orphaned {
		t.Errorf("unknown-state worker should keep c4 active and orphaned: %+v", c)
	} else if len(c.Actions) != 0 {
		t.Errorf("closed dispatcher offers relaunch: %+v", c.Actions)
	}
	it := fleetItem(t, v, keyA4+"#dispatcher_gone:c4:")
	if it.Severity != view.SeverityWarning || it.Action != nil || it.Message != "dispatcher closed, 1 workers" {
		t.Errorf("closed orphan item = %+v", it)
	}
}

func TestFleetStaleWorkersDoNotKeepCrewActive(t *testing.T) {
	in := viewtest.FleetInputs()
	v := view.BuildFleet(in)
	if slices.Contains(crewKeys(v), keyA6) {
		t.Errorf("dead crew with only stale workers shown: %v", crewKeys(v))
	}
	for _, id := range fleetIDs(v) {
		if strings.Contains(id, "dispatcher_gone:c6:") {
			t.Errorf("stale-only crew produced %s", id)
		}
	}
	if v.Summary.HiddenCrews != 2 {
		t.Errorf("hidden = %d, want c4 and c6", v.Summary.HiddenCrews)
	}

	sec := data.FleetSection{Status: &data.FleetStatus{Repos: []data.FleetRepo{{
		Repo: "a", Common: "/r/a/.git", Crews: []data.FleetCrew{*crewOf(in, "/r/a/.git", "c6")},
	}}}}
	if got := view.ActiveCommons(sec); len(got) != 0 {
		t.Errorf("ActiveCommons = %v, want none for stale-only crew", got)
	}

	crewOf(in, "/r/a/.git", "c6").Workers[0]["stale"] = false
	v = view.BuildFleet(in)
	c := fleetCrew(t, v, keyA6)
	if !c.Orphaned || c.Workers.Total != 2 {
		t.Errorf("one live-looking worker should orphan c6 with total 2: %+v", c)
	}
	if it := fleetItem(t, v, keyA6+"#dispatcher_gone:c6:"); !strings.Contains(it.Message, "1 workers") {
		t.Errorf("message = %q, want 1 workers", it.Message)
	}
}

func TestFleetCrewSpanningRepos(t *testing.T) {
	v := view.BuildFleet(viewtest.FleetInputs())
	a, b := fleetCrew(t, v, keyA1), fleetCrew(t, v, keyB1)
	if *a.Repo != "a" || *b.Repo != "b" || *a.Common != "/r/a/.git" || *b.Common != "/r/b/.git" {
		t.Errorf("repos = %v %v", *a.Repo, *b.Repo)
	}
	d := b.Dispatcher
	if d == nil || d.State != "live" || *d.PID != 4242 || *d.Lane != "alive" || *d.LastEventS != 4 {
		t.Errorf("repo b row did not inherit the home dispatcher: %+v", d)
	}
	if b.HomeRepo == nil || *b.HomeRepo != "a" || *a.HomeRepo != "a" {
		t.Errorf("home_repo = %v / %v", b.HomeRepo, a.HomeRepo)
	}
	if b.Orphaned || *b.Dir != "/r/b" || *a.Dir != "/r/a" {
		t.Errorf("b orphaned=%v dir=%v a dir=%v", b.Orphaned, *b.Dir, *a.Dir)
	}
	if b.Workers.Total != 1 || b.Workers.ByState["pr_open"] != 1 {
		t.Errorf("b workers = %+v", b.Workers)
	}
	if a.RemoteWorkers != 3 || a.Workers.Total != 5 {
		t.Errorf("a remote/total = %d/%d", a.RemoteWorkers, a.Workers.Total)
	}
}

func TestFleetDispatcherGone(t *testing.T) {
	v := view.BuildFleet(viewtest.FleetInputs())
	want := []string{"dispatcher", "--relaunch", "c3", "--repo", "/r/a"}
	c3 := fleetCrew(t, v, keyA3)
	if !c3.Orphaned || c3.Dispatcher.State != "dead" || len(c3.Actions) != 1 {
		t.Fatalf("c3 row = %+v", c3)
	}
	check := func(a view.Action) {
		t.Helper()
		if a.Verb != "relaunch" || a.Label != "relaunch dispatcher" || a.Key != "" || a.Dir != nil || !slices.Equal(a.Argv, want) {
			t.Errorf("relaunch action = %+v", a)
		}
	}
	check(c3.Actions[0])
	it := fleetItem(t, v, keyA3+"#dispatcher_gone:c3:")
	if it.Severity != view.SeverityCritical || it.Kind != view.KindDispatcherGone || it.Message != "dispatcher gone, 2 workers" {
		t.Errorf("item = %+v", it)
	}
	if it.CrewKey == nil || *it.CrewKey != keyA3 || it.Action == nil {
		t.Fatalf("item crew_key/action = %v %v", it.CrewKey, it.Action)
	}
	check(*it.Action)
	for _, key := range []string{keyA1, keyB1, keyB5} {
		if c := fleetCrew(t, v, key); c.Orphaned || len(c.Actions) != 0 {
			t.Errorf("%s orphaned=%v actions=%v", key, c.Orphaned, c.Actions)
		}
	}
}

func TestFleetLinkHealth(t *testing.T) {
	v := view.BuildFleet(viewtest.FleetInputs())
	relay := fleetItem(t, v, "relay_lost::tp")
	if relay.Severity != view.SeverityCritical || relay.CrewKey != nil || relay.Host != "tp" ||
		relay.Origin != view.OriginRemote || relay.Action != nil || relay.Message != "relay to tp lost (1 remote workers)" {
		t.Errorf("relay_lost = %+v", relay)
	}
	if slices.Contains(fleetIDs(v), "relay_lost::halo") {
		t.Error("halo has a live relay")
	}
	stale := fleetItem(t, v, "peer_stale::mbp")
	if stale.Severity != view.SeverityWarning || stale.CrewKey != nil || stale.Origin != view.OriginPeer || stale.Action != nil {
		t.Errorf("peer_stale = %+v", stale)
	}
	if slices.Contains(fleetIDs(v), "peer_stale::box") {
		t.Error("box synced 5 s ago")
	}
	dead := fleetItem(t, v, keyA1+"#remote_watch_dead:far/c1:feat/far")
	if dead.Severity != view.SeverityCritical || dead.Host != "far" || dead.Origin != view.OriginRemote ||
		dead.Message != "far on far: ssh: connect to host far port 22: timed out" {
		t.Errorf("remote_watch_dead = %+v", dead)
	}
	if a := dead.Action; a == nil || a.Verb != "peek" || a.Dir == nil || *a.Dir != "/r/a" ||
		!slices.Equal(a.Argv, []string{"crew", "where", "--crew", "c1", "--", "feat/far"}) {
		t.Errorf("remote_watch_dead action = %+v", dead.Action)
	}

	in := viewtest.FleetInputs()
	crewOf(in, "/r/a/.git", "c1").Dispatcher.State = "dead"
	v = view.BuildFleet(in)
	for _, id := range fleetIDs(v) {
		if strings.HasPrefix(id, "relay_lost") || strings.Contains(id, "remote_watch_dead") {
			t.Errorf("dead dispatcher still raises %s", id)
		}
	}
	if s := fleetHost(t, v, "tp").Relay.State; s != "idle" {
		t.Errorf("tp relay with dead dispatcher = %s", s)
	}
}

func TestFleetAttentionRollup(t *testing.T) {
	v := view.BuildFleet(viewtest.FleetInputs())
	for _, id := range []string{
		keyB5 + "#blocked:c5:feat/c5",
		keyA1 + "#role_failed:c1:feat/b",
		keyA1 + "#hold_matured:c1:h-q1",
		keyB5 + "#lane_stale:c5:",
	} {
		fleetItem(t, v, id)
	}
	seen := map[string]bool{}
	var merge []string
	for i, a := range v.Attention {
		if seen[a.ID] {
			t.Errorf("duplicate id %s", a.ID)
		}
		seen[a.ID] = true
		if a.CrewKey != nil && !strings.HasPrefix(a.ID, *a.CrewKey+"#") {
			t.Errorf("id %s lacks its crew_key prefix", a.ID)
		}
		if a.Kind == view.KindMergeReady {
			merge = append(merge, a.ID)
		}
		if i == 0 {
			continue
		}
		p := v.Attention[i-1]
		rank := map[view.Severity]int{view.SeverityCritical: 0, view.SeverityWarning: 1, view.SeverityReady: 2}
		switch {
		case rank[p.Severity] > rank[a.Severity]:
			t.Errorf("severity order: %s before %s", p.ID, a.ID)
		case rank[p.Severity] == rank[a.Severity] && (p.AgeSec < a.AgeSec || (p.AgeSec == a.AgeSec && p.ID > a.ID)):
			t.Errorf("age/id order: %s before %s", p.ID, a.ID)
		}
	}
	if !slices.Equal(merge, []string{keyA1 + "#merge_ready:c1:feat/b"}) {
		t.Errorf("merge_ready items = %v, want only repo a's PR 12", merge)
	}
	if v.Summary.Attention != len(v.Attention) || fleetCrew(t, v, keyB5).Attention != 2 {
		t.Errorf("summary attention %d, c5 attention %d", v.Summary.Attention, fleetCrew(t, v, keyB5).Attention)
	}

	in := viewtest.FleetInputs()
	c5 := crewOf(in, "/r/b/.git", "c5")
	c5.Dispatcher.State, c5.Dispatcher.Lane = "closed", strp("dead")
	for _, id := range fleetIDs(view.BuildFleet(in)) {
		if strings.Contains(id, "lane_") {
			t.Errorf("closed dispatcher raises %s", id)
		}
	}
}

func TestFleetPeerCrews(t *testing.T) {
	in := viewtest.FleetInputs()
	in.Fleet.Status.Repos[0].Peers.Rows[3]["state"] = "blocked"
	v := view.BuildFleet(in)
	var peers []string
	for _, c := range v.Crews {
		if c.Origin != view.OriginPeer {
			if len(peers) > 0 {
				t.Errorf("local row %s after a peer row", c.Key)
			}
			continue
		}
		peers = append(peers, c.Key)
		if c.Dispatcher != nil || c.Repo != nil || c.Common != nil || c.Dir != nil || c.Actions == nil || len(c.Actions) != 0 {
			t.Errorf("peer row %s = %+v", c.Key, c)
		}
		if c.Workers.Total != 1 {
			t.Errorf("peer row %s workers = %+v, want 1 after dedupe", c.Key, c.Workers)
		}
	}
	slices.Sort(peers)
	if want := []string{"box||p2", "box||p3", "mbp||p1", "mbp||p2"}; !slices.Equal(peers, want) {
		t.Errorf("peer rows = %v, want %v", peers, want)
	}
	if v.Summary.Dropped != 2 {
		t.Errorf("dropped = %d, want repo a's 1 + the cross-repo c3 row", v.Summary.Dropped)
	}
	it := fleetItem(t, v, "box||p3#blocked:box/p3:feat/p3")
	if it.Action != nil || it.Origin != view.OriginPeer || *it.CrewKey != "box||p3" {
		t.Errorf("peer item = %+v", it)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walk(doc, func(m map[string]any) {
		if m["origin"] != "peer" {
			return
		}
		if a, ok := m["action"]; ok && a != nil {
			t.Errorf("peer item with action: %v", m)
		}
		if acts, ok := m["actions"]; ok && len(acts.([]any)) != 0 {
			t.Errorf("peer row with actions: %v", m)
		}
		if d, ok := m["dir"]; ok && d != nil {
			t.Errorf("peer row with dir: %v", m)
		}
	})
}

func TestFleetCapacity(t *testing.T) {
	in := viewtest.FleetInputs()
	c := view.BuildFleet(in).Capacity
	if *c.Budget != 8 || *c.Weight != 5 || *c.Pending != 1 || *c.Free != 3 || *c.Load1 != 1.5 || *c.Cores != 8 || *c.LoadOver {
		t.Errorf("machine scalars = %+v", c)
	}
	if len(c.Leads) != 2 || len(c.LocalModels) != 1 || c.LocalModels[0] != (data.FleetLocalModel{ID: "qwen", MaxConcurrent: 2, InUse: 1}) {
		t.Errorf("leads/local_models = %+v %+v", c.Leads, c.LocalModels)
	}
	if len(c.Engines) != 2 || c.Engines[0].Engine != "claude" || c.Engines[1].Engine != "codex" ||
		!c.Engines[1].LimitReached || !slices.Equal(c.Engines[1].HotWindows, []string{"5h"}) || c.Engines[0].HotWindows == nil {
		t.Errorf("engines = %+v", c.Engines)
	}
	want := view.BuildOverview(viewtest.Inputs()).Monitors.Budgets
	if len(want) == 0 || !reflect.DeepEqual(c.Budgets, want) {
		t.Errorf("budgets = %+v, want the Overview's %+v", c.Budgets, want)
	}

	in.Fleet.Status.Machine = nil
	c = view.BuildFleet(in).Capacity
	if c.Budget != nil || c.Weight != nil || c.Pending != nil || c.Free != nil || c.Load1 != nil || c.Cores != nil || c.LoadOver != nil {
		t.Errorf("null machine scalars = %+v", c)
	}
	raw, _ := json.Marshal(c)
	for _, k := range []string{`"leads":[]`, `"local_models":[]`, `"engines":[]`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("null machine capacity %s lacks %s", raw, k)
		}
	}
}

func TestFleetHolds(t *testing.T) {
	holds := view.BuildFleet(viewtest.FleetInputs()).Holds
	var got []string
	for _, h := range holds {
		got = append(got, h.ID)
	}
	if want := []string{"h-f1", "h-q1", "h-f2", "h-p1"}; !slices.Equal(got, want) {
		t.Fatalf("hold order = %v, want %v", got, want)
	}
	f1, q1, p1 := holds[0], holds[1], holds[3]
	if f1.Kind != "fleet" || *f1.Prio != "high" || *f1.RepoPrio != 2 || *f1.Weight != 2 || f1.State != "due" || !f1.Matured || *f1.Branch != "feat/q1" || f1.Repo != "a" || f1.Crew != "c1" {
		t.Errorf("fleet hold = %+v", f1)
	}
	if q1.Kind != "quota" || q1.State != "due" || !q1.Matured || q1.Prio != nil || q1.Branch != nil || q1.WaitedS != 120 || q1.Repo != "a" || q1.Crew != "c1" {
		t.Errorf("quota hold = %+v", q1)
	}
	if p1.State != "parked" || *p1.Reason != "oversize" || p1.Matured {
		t.Errorf("parked hold = %+v", p1)
	}
}

func TestFleetHosts(t *testing.T) {
	in := viewtest.FleetInputs()
	v := view.BuildFleet(in)
	var names []string
	for _, h := range v.Hosts {
		names = append(names, h.Name)
	}
	if !slices.Equal(names, []string{"box", "halo", "mbp", "tp"}) {
		t.Errorf("hosts = %v", names)
	}
	halo := fleetHost(t, v, "halo")
	if halo.Relay.State != "up" || !slices.Equal(halo.Relay.Crews, []string{"c1"}) || halo.Sync.State != "n/a" || halo.Peer || halo.SSH != "halo.lan" {
		t.Errorf("halo = %+v", halo)
	}
	if r := halo.Reach; *r.Source != "tailscale" || !*r.Online || *r.LastSeen != 1794821400 || r.Error != nil {
		t.Errorf("halo reach = %+v", r)
	}
	tp := fleetHost(t, v, "tp")
	if tp.Relay.State != "down" || len(tp.Relay.Crews) != 0 || *tp.Reach.Online {
		t.Errorf("tp = %+v", tp)
	}
	mbp := fleetHost(t, v, "mbp")
	if s := mbp.Sync; s.State != "failed" || *s.LastOKTS != 1794799400000 || *s.LagSec != 610 || *s.Error != "ssh: Connection refused" {
		t.Errorf("mbp sync = %+v", s)
	}
	box := fleetHost(t, v, "box")
	if s := box.Sync; s.State != "ok" || *s.LagSec != 5 || s.Error != nil || box.Relay.State != "idle" {
		t.Errorf("box = %+v", box)
	}
	if *box.Reach.Source != "tailscale" || *box.Reach.Online {
		t.Errorf("box reach = %+v", box.Reach)
	}
	if v.Summary.Hosts != 4 || v.Summary.HostsUnhealthy != 3 {
		t.Errorf("summary hosts = %d/%d", v.Summary.Hosts, v.Summary.HostsUnhealthy)
	}
	var suggested []string
	for _, s := range v.SuggestedHosts {
		suggested = append(suggested, s.Name)
		if slices.Contains(names, s.Name) {
			t.Errorf("suggested host %q is configured", s.Name)
		}
	}
	if !slices.Equal(suggested, []string{"evil\x1b[31mhost", "phone"}) {
		t.Errorf("suggested = %q", suggested)
	}

	for i := range in.Fleet.Status.Repos {
		links := in.Fleet.Status.Repos[i].Peers.Links
		links[0].ErrTS = nil
		links[1].OKTS, links[1].ErrTS = nil, nil
	}
	in.Fleet.Hosts = append(in.Fleet.Hosts, data.HostConfig{Name: "nuc", SSH: "nuc", Peer: true}, data.HostConfig{Name: "zed", SSH: "zed"})
	in.Fleet.Reach = append(in.Fleet.Reach, data.HostReach{Host: "nuc", Source: "tailscale", Error: strp("not in tailnet")})
	v = view.BuildFleet(in)
	if s := fleetHost(t, v, "mbp").Sync; s.State != "stale" || s.Error != nil {
		t.Errorf("mbp without error = %+v", s)
	}
	if s := fleetHost(t, v, "box").Sync; s.State != "never" || s.LastOKTS != nil || s.LagSec != nil {
		t.Errorf("box without links = %+v", s)
	}
	nuc := fleetHost(t, v, "nuc")
	if nuc.Sync.State != "never" || *nuc.Reach.Error != "not in tailnet" || nuc.Reach.Online != nil {
		t.Errorf("nuc = %+v", nuc)
	}
	if r := fleetHost(t, v, "zed").Reach; r.Source != nil || r.Online != nil {
		t.Errorf("zed without reach = %+v", r)
	}
	fleetItem(t, v, "peer_stale::mbp")
	if slices.Contains(fleetIDs(v), "peer_stale::box") {
		t.Error("never-synced box raised peer_stale")
	}
}

func TestFleetActionsCarryDir(t *testing.T) {
	in := viewtest.FleetInputs()
	in.Fleet.Status.Repos[1].Common = "/x.git"
	in.Digests["/x.git"] = in.Digests["/r/b/.git"]
	v := view.BuildFleet(in)
	roots := map[string]string{}
	for _, c := range v.Crews {
		if c.Dir != nil {
			roots[c.Key] = *c.Dir
		}
	}
	if roots[keyA1] != "/r/a" || roots["lap|/x.git|c5"] != "/x.git" {
		t.Fatalf("roots = %v", roots)
	}
	check := func(where string, a view.Action, root string) {
		t.Helper()
		if a.Verb == "relaunch" {
			return
		}
		if a.Dir == nil || *a.Dir != root || !strings.HasPrefix(a.Command, "cd "+root+" && ") {
			t.Errorf("%s %s: dir %v command %q, want %s", where, a.Verb, a.Dir, a.Command, root)
		}
	}
	n := 0
	for _, it := range v.Attention {
		if it.Action == nil || it.CrewKey == nil {
			continue
		}
		n++
		check(it.ID, *it.Action, roots[*it.CrewKey])
	}
	for key, root := range roots {
		ci, ok := view.CrewInputs(in, key)
		if !ok {
			t.Fatalf("no inputs for %s", key)
		}
		for _, w := range view.BuildOverview(ci).Workers {
			for _, a := range w.Actions {
				n++
				check(key+" "+w.Branch, a, root)
			}
		}
	}
	if n == 0 {
		t.Fatal("no actions checked")
	}
}

func TestCrewInputsDrill(t *testing.T) {
	in := viewtest.FleetInputs()
	ci, ok := view.CrewInputs(in, keyA1)
	if !ok || ci.Dir == nil || *ci.Dir != "/r/a" {
		t.Fatalf("local inputs ok=%v dir=%v", ok, ci.Dir)
	}
	crews := ci.Snap.Roster.Crews
	if len(crews) != 1 || crews[0].ID != "c1" || len(crews[0].Workers) != 5 || len(crews[0].Holds) != 2 ||
		crews[0].Lane == nil || crews[0].Lane.State != "alive" || len(ci.Snap.Roster.Peers) != 0 {
		t.Fatalf("roster = %+v", ci.Snap.Roster)
	}
	if ci.Bus.PRs[12] == nil || ci.Bus.PRs[12].Checks != "SUCCESS" || len(ci.Bus.Recent) == 0 {
		t.Errorf("repo a digest = %+v", ci.Bus.PRs[12])
	}
	for _, e := range ci.Bus.Recent {
		if !strings.HasPrefix(e.From, "worker:feat/a#") && !strings.HasPrefix(e.From, "worker:feat/b#") && e.From != "dispatcher:c1" {
			t.Errorf("recent event from %q kept", e.From)
		}
	}
	if !slices.ContainsFunc(view.BuildOverview(ci).Attention, func(a view.AttentionItem) bool { return a.Kind == view.KindHoldMatured }) {
		t.Error("fleet quota hold did not raise hold_matured in the drill-down")
	}

	ci, _ = view.CrewInputs(in, keyB1)
	if ci.Bus.PRs[12].Checks != "PENDING" {
		t.Errorf("repo b row reads PR 12 checks %s", ci.Bus.PRs[12].Checks)
	}
	for _, e := range ci.Bus.Recent {
		if !strings.HasPrefix(e.From, "worker:feat/b2#") {
			t.Errorf("repo b c1 recent from %q", e.From)
		}
	}

	c5 := crewOf(in, "/r/b/.git", "c5")
	c5.Dispatcher.State = "closed"
	if ci, _ = view.CrewInputs(in, keyB5); ci.Snap.Roster.Crews[0].Lane != nil {
		t.Errorf("closed dispatcher lane = %+v", ci.Snap.Roster.Crews[0].Lane)
	}

	ci, ok = view.CrewInputs(in, "box||p2")
	if !ok || ci.Dir != nil || len(ci.Snap.Roster.Crews) != 0 || len(ci.Snap.Roster.Peers) != 1 || len(ci.Bus.Recent) != 0 || len(ci.Bus.PRs) != 0 {
		t.Errorf("peer inputs ok=%v %+v", ok, ci)
	}
	if _, ok := view.CrewInputs(in, "lap|/r/a/.git|nope"); ok {
		t.Error("unknown key resolved")
	}
}

func encodeGolden(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGoldenFleet(t *testing.T) {
	drill := func(key string) any {
		ci, ok := view.CrewInputs(viewtest.FleetInputs(), key)
		if !ok {
			t.Fatalf("no crew %s", key)
		}
		return view.BuildOverview(ci)
	}
	cases := []struct {
		name, view string
		build      func() any
	}{
		{"fleet", "fleet", func() any { return view.BuildFleet(viewtest.FleetInputs()) }},
		{"fleet_notailscale", "fleet", func() any { return view.BuildFleet(viewtest.FleetInputsNoTailscale()) }},
		{"fleet_drill_local", "overview", func() any { return drill(keyA1) }},
		{"fleet_drill_peer", "overview", func() any { return drill("box||p3") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := encodeGolden(t, c.build())
			var probe struct {
				SchemaVersion int    `json:"schema_version"`
				View          string `json:"view"`
			}
			if err := json.Unmarshal(got, &probe); err != nil {
				t.Fatal(err)
			}
			if probe.SchemaVersion != 1 || probe.View != c.view {
				t.Errorf("schema_version/view = %d/%q", probe.SchemaVersion, probe.View)
			}
			path := filepath.Join("testdata", "golden", c.name+".json")
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
				t.Errorf("%s differs from golden; run go test ./internal/view -run TestGoldenFleet -update and review the diff", c.name)
			}
		})
	}
}

func TestFleetNoTailscaleReach(t *testing.T) {
	v := view.BuildFleet(viewtest.FleetInputsNoTailscale())
	for _, h := range v.Hosts {
		if h.Reach.Source == nil || *h.Reach.Source != "ssh" {
			t.Errorf("%s reach = %+v", h.Name, h.Reach)
		}
	}
	if tp := fleetHost(t, v, "tp").Reach; *tp.Online || *tp.Error != "ssh: connect to host tp port 22: No route to host" {
		t.Errorf("tp reach = %+v", tp)
	}
	if len(v.SuggestedHosts) != 0 || !slices.Contains(v.Errors, "tailscale: exit 127") {
		t.Errorf("suggested %v errors %v", v.SuggestedHosts, v.Errors)
	}

	in := viewtest.FleetInputsNoTailscale()
	in.Snap.Settings.Rows = in.Snap.Settings.Rows[1:]
	if slices.ContainsFunc(view.BuildFleet(in).Errors, func(e string) bool { return strings.HasPrefix(e, "tailscale") }) {
		t.Error("tailscale error listed with dashTailscale off")
	}
}

func TestFleetErrors(t *testing.T) {
	in := viewtest.FleetInputs()
	in.Fleet.QueueError = strp("exit 2")
	in.Fleet.Status.Repos[1].PeersError = strp("permission denied")
	in.DigestErrs["/r/a/.git"] = "open events.jsonl: no such file"
	errs := view.BuildFleet(in).Errors
	for _, want := range []string{"fleet queue: exit 2", "repo b: peers: permission denied", "repo a: bus: open events.jsonl: no such file"} {
		if !slices.Contains(errs, want) {
			t.Errorf("errors = %v, missing %q", errs, want)
		}
	}

	in = viewtest.FleetInputs()
	in.Fleet.Status, in.Fleet.StatusError = nil, strp("crew: not found")
	v := view.BuildFleet(in)
	if !slices.Contains(v.Errors, "fleet status: crew: not found") || v.Crews == nil || len(v.Crews) != 0 || v.Capacity.Budget != nil {
		t.Errorf("no status: errors %v crews %v", v.Errors, v.Crews)
	}
}

func TestFleetPeerStringsRaw(t *testing.T) {
	in := viewtest.FleetInputs()
	row := in.Fleet.Status.Repos[0].Peers.Rows[3]
	row["branch"], row["state"] = "feat/\x1b]0;x\x07p3", "blocked"
	for i := range in.Fleet.Status.Repos {
		in.Fleet.Status.Repos[i].Peers.Links[0].Detail = strp("refused\x1b[2J\u009b")
	}
	raw, err := json.Marshal(view.BuildFleet(in))
	if err != nil {
		t.Fatal(err)
	}
	var v view.FleetView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if it := fleetItem(t, v, "box||p3#blocked:box/p3:feat/\x1b]0;x\x07p3"); it.Branch != "feat/\x1b]0;x\x07p3" || it.Action != nil {
		t.Errorf("peer branch = %q", it.Branch)
	}
	if e := fleetHost(t, v, "mbp").Sync.Error; e == nil || *e != "refused\x1b[2J\u009b" {
		t.Errorf("peer link detail = %q", deref(e))
	}
	if !slices.ContainsFunc(v.SuggestedHosts, func(s data.SuggestedHost) bool { return s.Name == "evil\x1b[31mhost" }) {
		t.Errorf("tailscale host names = %+v", v.SuggestedHosts)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func wk(branch, state string) map[string]any {
	return map[string]any{"branch": branch, "name": branch, "state": state, "tier": "standard", "engine": "claude", "live": true}
}

func fleetIn(repos ...data.FleetRepo) view.FleetInputs {
	return view.FleetInputs{
		Fleet:      data.FleetSection{Status: &data.FleetStatus{Repos: repos}},
		Digests:    map[string]data.BusDigest{},
		DigestErrs: map[string]string{},
		Loaded:     view.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
		NowMS:      viewtest.FleetNowMS,
		Crew:       []string{"crew"},
	}
}

func crewIn(id, state string, workers ...map[string]any) data.FleetCrew {
	return data.FleetCrew{CrewID: id, Dispatcher: data.FleetDispatcher{State: state}, Workers: workers}
}

func TestFleetSymlinkedReposDedupe(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real", ".git")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "real"), link); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(link, ".git")
	w := wk("feat/x", "working")
	in := fleetIn(
		data.FleetRepo{Host: "lap", Repo: "r", Common: linked, Crews: []data.FleetCrew{crewIn("c1", "live", w)}},
		data.FleetRepo{Host: "lap", Repo: "r", Common: real, Crews: []data.FleetCrew{crewIn("c1", "live", w)}},
	)
	v := view.BuildFleet(in)
	if len(v.Crews) != 1 {
		t.Fatalf("rows = %v, want one", crewKeys(v))
	}
	if c := v.Crews[0]; c.IdleRepos != 0 || *c.Common != real {
		t.Errorf("row = idle %d common %s, want 0 and the real path", c.IdleRepos, *c.Common)
	}
	if v.Summary.Workers != 1 {
		t.Errorf("workers = %d, want 1", v.Summary.Workers)
	}
	if got := view.ActiveCommons(in.Fleet); len(got) != 1 {
		t.Errorf("ActiveCommons = %v, want one", got)
	}
}

func TestFleetMultiRepoCrewFoldsIdle(t *testing.T) {
	in := fleetIn(
		data.FleetRepo{Host: "lap", Repo: "a", Common: "/r/a/.git", Crews: []data.FleetCrew{crewIn("c1", "live", wk("f/a", "working"))}},
		data.FleetRepo{Host: "lap", Repo: "b", Common: "/r/b/.git", Crews: []data.FleetCrew{crewIn("c1", "live", wk("f/b", "pr_open"))}},
		data.FleetRepo{Host: "lap", Repo: "c", Common: "/r/c/.git", Crews: []data.FleetCrew{crewIn("c1", "live")}},
		data.FleetRepo{Host: "lap", Repo: "d", Common: "/r/d/.git", Crews: []data.FleetCrew{crewIn("c1", "live")}},
	)
	v := view.BuildFleet(in)
	if len(v.Crews) != 2 {
		t.Fatalf("rows = %v, want head + one sub", crewKeys(v))
	}
	head, sub := v.Crews[0], v.Crews[1]
	if head.Sub || !sub.Sub || *head.Repo != "a" || *sub.Repo != "b" {
		t.Errorf("head/sub = %v/%v repos %s/%s", head.Sub, sub.Sub, *head.Repo, *sub.Repo)
	}
	if head.IdleRepos != 2 || sub.IdleRepos != 0 {
		t.Errorf("idle repos = %d/%d, want 2/0", head.IdleRepos, sub.IdleRepos)
	}
	if v.Summary.Crews != 1 {
		t.Errorf("summary crews = %d, want 1 head", v.Summary.Crews)
	}
}

func TestFleetSectionOrder(t *testing.T) {
	in := fleetIn(data.FleetRepo{
		Host: "lap", Repo: "a", Common: "/r/a/.git",
		Crews: []data.FleetCrew{
			crewIn("dead", "dead", wk("f/d", "failed")),
			crewIn("orph", "dead", wk("f/o", "working")),
			crewIn("live", "live", wk("f/l", "working")),
		},
		Peers: data.FleetPeers{Rows: []map[string]any{
			{"host": "box", "crew_id": "p1", "branch": "f/p", "state": "working"},
		}},
	})
	v := view.BuildFleet(in)
	var got []string
	for _, c := range v.Crews {
		got = append(got, c.Crew+":"+c.Section)
	}
	want := []string{"live:live", "orph:orphaned", "p1:peer", "dead:dead"}
	if !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

func countHeads(v view.FleetView) int {
	n := 0
	for _, c := range v.Crews {
		if !c.Sub {
			n++
		}
	}
	return n
}
