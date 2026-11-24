package fleet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
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

func TestLayout2StatusUnreadableLayout1RepoKeepsItsSkip(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.layout2("l2", f.tag("alpha"), wstatus(20, "l2", "feat/b", "working", ""))
	if err := os.Chmod(f.busPath("alpha"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(f.busPath("alpha")); err == nil {
		t.Skip("permissions are not enforced")
	}

	d := f.status()
	if got := d.crewIDs(); !slices.Equal(got, []string{"l2"}) {
		t.Errorf("crews = %v, want l2 built anyway", got)
	}
	if len(d.Skipped) != 1 || d.Skipped[0].Path != f.commons["alpha"] {
		t.Errorf("skipped = %+v, want alpha's read error", d.Skipped)
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
