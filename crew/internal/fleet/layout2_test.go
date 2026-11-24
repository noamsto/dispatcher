package fleet

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lane"
)

func (f *queueFix) stateRoot() string { return bus.StateRoot(f.opts.Getenv) }

// tag is the realpath a layout-2 meta names a repo by.
func (f *queueFix) tag(repo string) *string {
	f.t.Helper()
	tag, err := bus.RepoTag(f.commons[repo])
	if err != nil {
		f.t.Fatal(err)
	}
	return &tag
}

// layout2 makes a live layout-2 crew homed at home (nil: no repo) whose log
// holds rows, and returns its dir.
func (f *queueFix) layout2(id string, home *string, rows ...string) string {
	f.t.Helper()
	m := bus.Meta{ID: id, Created: 1, HomeHost: "box", HomeRepo: home, CreatedBy: "register"}
	if err := bus.CreateMeta(f.stateRoot(), m); err != nil {
		f.t.Fatal(err)
	}
	dir := bus.StoreDir(f.stateRoot(), id)
	for _, r := range rows {
		f.appendTo(filepath.Join(dir, "events.jsonl"), r+"\n")
	}
	return dir
}

// archived makes an ended layout-2 crew: archive/<id>/ with its meta and log.
func (f *queueFix) archived(id string, rows ...string) {
	f.t.Helper()
	dir := bus.ArchiveDir(f.stateRoot(), id)
	f.write(filepath.Join(dir, "meta.json"), `{"layout":2,"id":"`+id+`","created":1,"home_host":"box","home_repo":null,"visiting":false,"created_by":"register"}`+"\n")
	for _, r := range rows {
		f.appendTo(filepath.Join(dir, "events.jsonl"), r+"\n")
	}
}

func (f *queueFix) status() StatusDoc {
	f.t.Helper()
	return CollectStatus(f.t.Context(), StatusRequest{}, os.Stderr, f.statusOpts())
}

func (d StatusDoc) repo(t *testing.T, label string) RepoStatus {
	t.Helper()
	for _, r := range d.Repos {
		if r.Repo == label {
			return r
		}
	}
	t.Fatalf("no repo %q in %+v", label, d.Repos)
	return RepoStatus{}
}

func (d StatusDoc) crewIDs() []string {
	var ids []string
	for _, r := range d.Repos {
		for _, c := range r.Crews {
			ids = append(ids, c.CrewID)
		}
	}
	slices.Sort(ids)
	return ids
}

func TestLayout2QueueListsBothLayouts(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.add("alpha", "c1", "h1", 1, 100)
	f.register("alpha", "c1", livePID)
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	d := f.doc()
	if got := d.ids(); !slices.Equal(got, []string{"h1", "h2"}) {
		t.Fatalf("holds = %v, want h1 and h2", got)
	}
	for _, id := range []string{"h1", "h2"} {
		if r := d.row(t, id); r.Common != f.commons["alpha"] || r.Repo != "acme/alpha" || r.State == "parked" {
			t.Errorf("%s = %+v, want alpha's, not parked", id, r)
		}
	}
}

func TestLayout2QueueIneligibleWithoutALiveDispatcher(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))

	r := f.doc().row(t, "h2")
	if r.State != "parked" || reasonOf(r) != "ineligible" {
		t.Errorf("h2 = %+v, want parked ineligible", r)
	}
}

func TestLayout2QueueHoldOnlyCrew(t *testing.T) {
	f := newQueueFix(t, "alpha")
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	d := f.doc()
	if got := d.ids(); !slices.Equal(got, []string{"h2"}) || d.dueID() != "h2" {
		t.Errorf("holds = %v due %q, want h2 listed and due", got, d.dueID())
	}
}

func TestLayout2QueueArchivedCrewIsNotListed(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.archived("gone", fleetAddRow(f.ms(90), "gone", "h9", "feat/h9", "normal", 1))

	if got := f.doc().ids(); len(got) != 0 {
		t.Errorf("holds = %v, want none", got)
	}
}

func TestLayout2QueueIDInALayout1BusIsListedOnce(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.add("alpha", "l2", "old", 1, 200)
	f.register("alpha", "l2", livePID)
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(90), "l2", "new", "feat/new", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	if got := f.doc().ids(); !slices.Equal(got, []string{"new"}) {
		t.Errorf("holds = %v, want only the layout-2 hold", got)
	}
}

func TestLayout2QueueNullHomeIsNoRepo(t *testing.T) {
	f := newQueueFix(t, "alpha")
	dir := f.layout2("l2", nil, fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	r := f.doc().row(t, "h2")
	if r.Common != "" || r.Repo != "(no repo)" || r.State == "parked" {
		t.Errorf("h2 = %+v, want common empty, (no repo), not parked", r)
	}
}

func TestLayout2QueueUnregisteredHomeUsesItsMetaPath(t *testing.T) {
	f := newQueueFix(t)
	f.repo("gamma", false)
	dir := f.layout2("l2", f.tag("gamma"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	r := f.doc().row(t, "h2")
	if want := *f.tag("gamma"); r.Common != want || r.Repo != "acme/gamma" {
		t.Errorf("h2 = %+v, want common %q, acme/gamma", r, want)
	}
}

func TestLayout2QueueUnreadableLayout1RepoStillListsItsLayout2Crews(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.add("alpha", "c1", "h1", 1, 100)
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")
	if err := os.Chmod(f.busPath("alpha"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(f.busPath("alpha")); err == nil {
		t.Skip("permissions are not enforced")
	}

	if got := f.doc().ids(); !slices.Equal(got, []string{"h2"}) {
		t.Errorf("holds = %v, want h2 alone", got)
	}
}

func TestLayout2DueMatchesByCrewIDWhateverTheCwdRepo(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	for _, cwd := range []string{"alpha", "beta"} {
		if v := f.due(cwd, "l2", false); !slices.Equal(v.Due, []string{"h2"}) {
			t.Errorf("Due from %s = %+v, want h2", cwd, v)
		}
	}
	if v := f.due("alpha", "other", false); len(v.Due) != 0 {
		t.Errorf("Due for another crew = %+v, want none", v)
	}
}

func TestLayout2StatusListsBothLayouts(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.post("alpha", wstatus(10, "c1", "feat/a", "working", ""))
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/b", "working", ""))

	d := f.status()
	if got := d.crewIDs(); !slices.Equal(got, []string{"c1", "l2"}) {
		t.Fatalf("crews = %v, want c1 and l2", got)
	}
	if len(d.Repos) != 1 || d.Repos[0].Common != f.commons["alpha"] || len(d.Skipped) != 0 {
		t.Errorf("repos = %+v skipped = %+v, want one alpha group", d.Repos, d.Skipped)
	}
	if w := d.crew(t, "l2").worker(t, "feat/b"); w["state"] != "working" {
		t.Errorf("l2 worker = %v", w)
	}
}

func TestLayout2StatusHoldOnlyCrew(t *testing.T) {
	f := newQueueFix(t, "alpha")
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	c := f.status().crew(t, "l2")
	if len(c.Workers) != 0 || len(c.Holds) != 1 || c.Holds[0].ID != "h2" || c.Holds[0].Kind != "fleet" || !c.Holds[0].Due {
		t.Errorf("l2 = %+v, want one due fleet hold h2 and no workers", c)
	}
	if c.Dispatcher.State != "live" {
		t.Errorf("dispatcher = %+v, want live from the layout-2 dir", c.Dispatcher)
	}
}

func TestLayout2StatusArchivedCrewIsNotListed(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.archived("gone", wstatus(10, "gone", "feat/a", "working", ""))

	if got := f.status().crewIDs(); len(got) != 0 {
		t.Errorf("crews = %v, want none", got)
	}
}

func TestLayout2StatusIDInALayout1BusIsListedOnce(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.post("alpha", wstatus(10, "l2", "feat/old", "working", ""))
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/new", "working", ""))

	d := f.status()
	if got := d.crewIDs(); !slices.Equal(got, []string{"l2"}) {
		t.Fatalf("crews = %v, want l2 once", got)
	}
	if ws := d.crew(t, "l2").Workers; len(ws) != 1 || ws[0]["branch"] != "feat/new" {
		t.Errorf("workers = %v, want the layout-2 row only", ws)
	}
}

func TestLayout2StatusHomeWithoutALayout1Log(t *testing.T) {
	f := newQueueFix(t, "alpha")
	if err := os.Remove(f.busPath("alpha")); err != nil {
		t.Fatal(err)
	}
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/b", "working", ""))

	d := f.status()
	if got := d.crewIDs(); !slices.Equal(got, []string{"l2"}) || len(d.Skipped) != 0 {
		t.Errorf("crews = %v skipped = %+v, want l2 and no skip", got, d.Skipped)
	}
	if r := d.repo(t, "acme/alpha"); r.Common != f.commons["alpha"] {
		t.Errorf("repo = %+v, want alpha's registry path", r)
	}
}

func TestLayout2StatusSymlinkedRegistryPath(t *testing.T) {
	f := newQueueFix(t)
	real := f.repo("alpha", false)
	if err := os.Remove(f.busPath("alpha")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(filepath.Dir(real), link); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(link, ".git")
	f.appendTo(f.opts.Registry, linked+"\n")
	f.origins[linked] = f.origins[real]
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/b", "working", ""))

	d := f.status()
	if got := d.crewIDs(); !slices.Equal(got, []string{"l2"}) || len(d.Repos) != 1 || len(d.Skipped) != 0 {
		t.Fatalf("repos = %+v skipped = %+v, want l2 in one group", d.Repos, d.Skipped)
	}
	if d.Repos[0].Common != linked {
		t.Errorf("common = %q, want the registry path %q", d.Repos[0].Common, linked)
	}
}

func TestLayout2StatusUnreadableLayout1RepoKeepsOnlyItsSkip(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/b", "working", ""))
	if err := os.Chmod(f.busPath("alpha"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(f.busPath("alpha")); err == nil {
		t.Skip("permissions are not enforced")
	}

	d := f.status()
	if len(d.Skipped) != 1 || d.Skipped[0].Path != f.commons["alpha"] {
		t.Errorf("skipped = %+v, want alpha's read error", d.Skipped)
	}
	if len(d.Repos) != 0 {
		t.Errorf("repos = %+v, want alpha absent: its skip and a group would share one key", d.Repos)
	}
}

func TestLayout2StatusNullHomeIsNoRepo(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.layout2("l2", nil, wstatus(20, "l2", "feat/b", "working", ""))

	d := f.status()
	r := d.repo(t, "(no repo)")
	if r.Common != "" || r.Key != "" || len(r.Crews) != 1 || r.Crews[0].CrewID != "l2" || r.Peers != nil {
		t.Errorf("group = %+v, want l2 under an empty common", r)
	}
}

func TestLayout2StatusUnregisteredHomeIsItsOwnGroup(t *testing.T) {
	f := newQueueFix(t)
	f.repo("gamma", false)
	f.layout2("l2", f.tag("gamma"), wstatus(20, "l2", "feat/b", "working", ""))

	r := f.status().repo(t, "acme/gamma")
	if r.Common != *f.tag("gamma") || len(r.Crews) != 1 {
		t.Errorf("group = %+v, want l2 under gamma's meta path", r)
	}
}

func TestLayout2PrioAcceptsALayout2OnlyHome(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.repo("gamma", false)
	gamma := *f.tag("gamma")
	dir := f.layout2("l2", f.tag("gamma"), fleetAddRow(f.ms(90), "l2", "h2", "feat/h2", "normal", 1))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")
	f.layout2("nohome", nil)

	if code, out, errb := f.prio("acme/gamma", "7"); code != 0 {
		t.Fatalf("prio gamma: exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if code, out, errb := f.prio("acme/alpha", "3"); code != 0 {
		t.Fatalf("prio alpha: exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if got := readPriorities(priorityPath(f.opts.Getenv)); got[gamma] != 7 || got[f.commons["alpha"]] != 3 {
		t.Errorf("priorities = %v, want gamma 7 and alpha 3", got)
	}
	if r := f.doc().row(t, "h2"); r.RepoPrio != 7 {
		t.Errorf("h2 repo_prio = %d, want 7", r.RepoPrio)
	}
	if code, _, _ := f.prio("acme/nowhere", "1"); code != 1 {
		t.Errorf("a crew with no home registered a repo: exit %d", code)
	}
}

func TestLayout2PrioSymlinkedHomeCountsOnce(t *testing.T) {
	f := newQueueFix(t)
	real := f.repo("alpha", false)
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(filepath.Dir(real), link); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(link, ".git")
	f.appendTo(f.opts.Registry, linked+"\n")
	f.origins[linked] = f.origins[real]
	f.layout2("l2", f.tag("alpha"))

	if code, out, errb := f.prio("acme/alpha", "4"); code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if got := readPriorities(priorityPath(f.opts.Getenv)); got[linked] != 4 || len(got) != 1 {
		t.Errorf("priorities = %v, want the registry path %q at 4", got, linked)
	}
}

func TestLayout2StatusCrewDirInAnotherRepoIsNotListedAgain(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.layout2("x", f.tag("alpha"), wstatus(20, "x", "feat/b", "working", ""))
	f.register("beta", "x", livePID)

	d := f.status()
	if got := d.crewIDs(); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("crews = %v, want x once", got)
	}
	if r := d.repo(t, "acme/alpha"); len(r.Crews) != 1 {
		t.Errorf("alpha = %+v, want x under its home", r)
	}
}

func TestLayout2DueClockRunsWhileTheHomeLayout1BusIsUnreadable(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.add("alpha", "c1", "h1", 1, 100)
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(100), "l2", "h2", "feat/h2", "normal", 2))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")
	if err := os.Chmod(f.busPath("alpha"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(f.busPath("alpha")); err == nil {
		t.Skip("permissions are not enforced")
	}

	k := holdKey{"", "l2", "h2"}
	f.due("alpha", "l2", true)
	f.advance(headTimeout)
	f.due("alpha", "l2", true)
	if got := f.record(); got[k].Demoted == 0 {
		t.Errorf("record %v, want h2 demoted at the timeout", got)
	}
}

func TestLayout2DueRecordSurvivesTheHomeBeingRegistered(t *testing.T) {
	f := newQueueFix(t)
	real := f.repo("alpha", false)
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(filepath.Dir(real), link); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(link, ".git")
	f.origins[linked] = f.origins[real]
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(100), "l2", "h2", "feat/h2", "normal", 2))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")

	k := holdKey{"", "l2", "h2"}
	f.due("alpha", "l2", true)
	first := f.record()[k]
	if first.FirstDue == 0 {
		t.Fatalf("record %v, want h2's clock running", f.record())
	}
	f.appendTo(f.opts.Registry, linked+"\n")
	f.advance(headTimeout)
	f.due("alpha", "l2", true)
	if got := f.record(); got[k].Demoted == 0 || len(got) != 1 {
		t.Errorf("record %v, want h2's one entry carried over and demoted", got)
	}
}

func TestAnUnreadableLayout2CrewLogKeepsItsClocks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	f := newQueueFix(t, "alpha")
	f.add("alpha", "c1", "h1", 1, 100)
	dir := f.layout2("l2", f.tag("alpha"), fleetAddRow(f.ms(100), "l2", "h2", "feat/h2", "normal", 2))
	f.write(filepath.Join(dir, "pid"), livePID+"\n")
	log := filepath.Join(dir, "events.jsonl")
	k := holdKey{"", "l2", "h2"}

	f.due("alpha", "l2", true)
	if got := f.record(); got[k].FirstDue == 0 {
		t.Fatalf("record %v, want h2's clock running", got)
	}
	if err := os.Chmod(log, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(log, 0o644) })
	if _, err := os.ReadFile(log); err == nil {
		t.Skip("permissions are not enforced")
	}

	f.advance(headTimeout)
	f.due("alpha", "l2", true)
	got, ok := f.record()[k]
	if !ok || got.Accrued <= 0 || got.FirstDue != 0 {
		t.Errorf("record %v, want h2's entry kept with its due stretch banked", f.record())
	}
}

func TestLayout2UnreadableMetaIsNotListedButNoted(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/b", "working", ""))
	f.write(filepath.Join(bus.StoreDir(f.stateRoot(), "mismatch"), "meta.json"),
		`{"layout":2,"id":"other","created":1,"home_host":"box","home_repo":null,"visiting":false,"created_by":"register"}`+"\n")
	f.write(filepath.Join(bus.StoreDir(f.stateRoot(), "badlayout"), "meta.json"),
		`{"layout":1,"id":"badlayout","created":1,"home_host":"box","home_repo":null,"visiting":false,"created_by":"register"}`+"\n")

	var stderr bytes.Buffer
	d := CollectStatus(t.Context(), StatusRequest{}, &stderr, f.statusOpts())
	if got := d.crewIDs(); !slices.Equal(got, []string{"c1", "l2"}) {
		t.Errorf("crews = %v, want the healthy c1 and l2 only", got)
	}
	for _, id := range []string{"mismatch", "badlayout"} {
		want := "crew fleet: crew " + id + ": unreadable meta.json ("
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q lacks %q", stderr.String(), want)
		}
	}
	if n := strings.Count(stderr.String(), "— not listed\n"); n != 2 {
		t.Errorf("%d not-listed notes in %q, want one per unreadable crew", n, stderr.String())
	}
}

func TestLayout2UnreadableMetaIsNotedWhenServed(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	f.write(filepath.Join(bus.StoreDir(f.stateRoot(), "mismatch"), "meta.json"),
		`{"layout":2,"id":"other","created":1,"home_host":"box","home_repo":null,"visiting":false,"created_by":"register"}`+"\n")

	var stderr bytes.Buffer
	o := f.statusOpts()
	o.Served = &ServedStatus{Origin: "halo", Since: 0}
	CollectStatus(t.Context(), StatusRequest{}, &stderr, o)
	if n := strings.Count(stderr.String(), "crew fleet: crew mismatch: unreadable meta.json ("); n != 1 {
		t.Errorf("served status printed %d notes for the bad crew in %q, want exactly one", n, stderr.String())
	}
}

// toOverseer is crew's dispatcher msg to overseer:box, agoS seconds ago.
func (f *queueFix) toOverseer(crew string, agoS int64) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"msg","from":"dispatcher:%s","to":"overseer:box","body":{"kind":"ack"}}`, f.ms(agoS), crew, crew)
}

func (f *queueFix) writeCursor(path string, agoS int64) {
	f.t.Helper()
	f.write(path, fmt.Sprintf("%d\n", f.ms(agoS)))
}

func inboxTS(c CrewStatus) []int64 {
	var out []int64
	for _, m := range c.OverseerInbox {
		out = append(out, m.TS)
	}
	return out
}

func TestLayout2StatusInboxStartsAtTheCrewCursor(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	f.post("alpha", f.toOverseer("c1", 3000), f.toOverseer("c1", 2000))
	f.layout2("l2", f.tag("alpha"), f.toOverseer("l2", 3000), f.toOverseer("l2", 2000), f.toOverseer("l2", 1000))
	f.layout2("l3", f.tag("alpha"), f.toOverseer("l3", 3000), f.toOverseer("l3", 2000))
	f.writeCursor(lane.OverseerCursor(f.env["XDG_STATE_HOME"], f.commons["alpha"]), 2500)
	f.writeCursor(lane.OverseerCrewCursor(f.stateRoot(), "l2"), 1500)
	f.write(lane.OverseerCrewCursor(f.stateRoot(), "l3"), "garbage\n")

	o := f.statusOpts()
	o.Host = func() (string, error) { return "box", nil }
	d := CollectStatus(t.Context(), StatusRequest{}, os.Stderr, o)
	for _, tc := range []struct {
		id   string
		want []int64
	}{
		{"c1", []int64{f.ms(2000)}},
		{"l2", []int64{f.ms(1000)}},
		{"l3", []int64{f.ms(2000)}},
	} {
		if got := inboxTS(d.crew(t, tc.id)); !slices.Equal(got, tc.want) {
			t.Errorf("%s inbox = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestLayout2ServedStatusListsCrewsInTheServedScope(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.repo("gamma", false)
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/b", "working", ""))
	f.layout2("l3", nil, wstatus(20, "l3", "feat/c", "working", ""))
	f.layout2("l4", f.tag("gamma"), wstatus(20, "l4", "feat/d", "working", ""))

	o := f.statusOpts()
	o.Served = &ServedStatus{Origin: "halo"}
	d := CollectStatus(t.Context(), StatusRequest{}, os.Stderr, o)
	if got := d.crewIDs(); !slices.Equal(got, []string{"l2", "l3"}) {
		t.Fatalf("crews = %v, want l2 and l3, not gamma's l4", got)
	}
	if r := d.repo(t, "acme/alpha"); r.Common != f.commons["alpha"] || len(r.Crews) != 1 || r.Crews[0].CrewID != "l2" {
		t.Errorf("alpha = %+v, want l2 under alpha's registry path", r)
	}
	if r := d.repo(t, noRepo); r.Common != "" || len(r.Crews) != 1 || r.Crews[0].CrewID != "l3" {
		t.Errorf("no-repo group = %+v, want l3", r)
	}
}

func TestLayout2ServedStatusInboxStartsAtServedSinceNotTheCrewCursor(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.layout2("l2", f.tag("alpha"), f.toOverseer("l2", 3000), f.toOverseer("l2", 2000), f.toOverseer("l2", 1000))
	f.writeCursor(lane.OverseerCrewCursor(f.stateRoot(), "l2"), 1500)

	o := f.statusOpts()
	o.Served = &ServedStatus{Origin: "box", Since: f.ms(2500)}
	d := CollectStatus(t.Context(), StatusRequest{}, os.Stderr, o)
	want := []int64{f.ms(2000), f.ms(1000)}
	if got := inboxTS(d.crew(t, "l2")); !slices.Equal(got, want) {
		t.Errorf("l2 inbox = %v, want %v", got, want)
	}
}

// parkedIDs is the hold ids a status document's queue lists as parked.
func parkedIDs(d StatusDoc) []string {
	ids := []string{}
	for _, p := range d.Queue.Parked {
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	return ids
}

// servedQueueFix has alpha registered, beta a repo under the root and gamma
// outside both, each with a parked hold (no live dispatcher), plus a layout-2
// crew homed in gamma and one with no home.
func servedQueueFix(t *testing.T) *queueFix {
	t.Helper()
	f := newQueueFix(t, "alpha")
	f.repo("beta", false)
	f.repo("gamma", false)
	f.layout2("la", f.tag("alpha"), fleetAddRow(f.ms(90), "la", "ha", "feat/ha", "normal", 1))
	f.layout2("lb", f.tag("beta"), fleetAddRow(f.ms(90), "lb", "hb", "feat/hb", "normal", 1))
	f.layout2("lg", f.tag("gamma"), fleetAddRow(f.ms(90), "lg", "hg", "feat/hg", "normal", 1))
	f.layout2("ln", nil, fleetAddRow(f.ms(90), "ln", "hn", "feat/hn", "normal", 1))
	return f
}

func TestServedQueueIsScopedToServedRepos(t *testing.T) {
	f := servedQueueFix(t)
	o := f.statusOpts()
	o.Served = &ServedStatus{Origin: "halo", Root: filepath.Join(f.root, "beta")}
	d := CollectStatus(t.Context(), StatusRequest{}, os.Stderr, o)
	if got := parkedIDs(d); !slices.Equal(got, []string{"ha", "hb"}) {
		t.Errorf("parked = %v, want ha and hb: not gamma's hg or the home-less hn", got)
	}
}

func TestServedQueueWithoutARootIsTheRegistry(t *testing.T) {
	f := servedQueueFix(t)
	o := f.statusOpts()
	o.Served = &ServedStatus{Origin: "halo"}
	d := CollectStatus(t.Context(), StatusRequest{}, os.Stderr, o)
	if got := parkedIDs(d); !slices.Equal(got, []string{"ha"}) {
		t.Errorf("parked = %v, want only the registered repo's ha", got)
	}
}

func TestLocalStatusAndQueueListEveryCrew(t *testing.T) {
	f := servedQueueFix(t)
	if got := parkedIDs(f.status()); !slices.Equal(got, []string{"ha", "hb", "hg", "hn"}) {
		t.Errorf("local status parked = %v, want ha, hb, hg and hn", got)
	}
	got := f.doc().ids()
	slices.Sort(got)
	if !slices.Equal(got, []string{"ha", "hb", "hg", "hn"}) {
		t.Errorf("local queue = %v, want ha, hb, hg and hn", got)
	}
}

func TestServedQueueIsEvaluatedOverTheWholeMachine(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.repo("gamma", false)
	for _, l := range []struct{ id, hold, repo string }{{"lg", "hg", "gamma"}, {"la", "ha", "alpha"}} {
		ago := int64(900)
		if l.repo == "alpha" {
			ago = 90
		}
		dir := f.layout2(l.id, f.tag(l.repo), fleetAddRow(f.ms(ago), l.id, l.hold, "feat/"+l.hold, "normal", 1))
		f.write(filepath.Join(dir, "pid"), livePID+"\n")
	}
	local := f.status()
	if local.Queue.Head == nil || local.Queue.Head.ID != "hg" {
		t.Fatalf("local head = %+v, want gamma's older hg", local.Queue.Head)
	}

	o := f.statusOpts()
	o.Served = &ServedStatus{Origin: "halo"}
	d := CollectStatus(t.Context(), StatusRequest{}, os.Stderr, o)
	if d.Queue.Head != nil {
		t.Errorf("served head = %+v, want none: the machine's head is not served", d.Queue.Head)
	}
	if !reflect.DeepEqual(d.Machine, local.Machine) {
		t.Errorf("served machine = %+v, want the local %+v", d.Machine, local.Machine)
	}
	for _, h := range d.crew(t, "la").Holds {
		if h.Due {
			t.Errorf("served %s is due while gamma's older hold is ahead", h.ID)
		}
	}
}
