package view

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// peerSyncStaleSec matches crew's PeerStale: an older last good sync is stale.
const peerSyncStaleSec = 300

// FleetInputs is everything the machine-wide view reads. Digests and
// DigestErrs are keyed by repo common dir; Snap supplies budget and settings.
type FleetInputs struct {
	Fleet      data.FleetSection
	Digests    map[string]data.BusDigest
	DigestErrs map[string]string
	Snap       data.Snapshot
	Loaded     Loaded
	NowMS      int64
	Crew       []string
	Pending    FleetPending
	ReapKeeps  map[data.WorkerKey]string
}

// FleetPending marks the sources a streamed partial has not delivered yet.
type FleetPending struct {
	Status, Queue, Reach, Hosts bool
}

func (f FleetInputs) nowSec() int64 {
	if f.NowMS != 0 {
		return f.NowMS / 1000
	}
	return f.Snap.Now
}

type FleetView struct {
	SchemaVersion  int                  `json:"schema_version"`
	View           string               `json:"view"`
	GeneratedAt    int64                `json:"generated_at"`
	Summary        FleetSummary         `json:"summary"`
	Attention      []AttentionItem      `json:"attention"`
	Crews          []FleetCrewRow       `json:"crews"`
	Capacity       FleetCapacity        `json:"capacity"`
	Holds          []FleetHoldRow       `json:"holds"`
	Hosts          []FleetHostRow       `json:"hosts"`
	SuggestedHosts []data.SuggestedHost `json:"suggested_hosts"`
	Errors         []string             `json:"errors"`
	Pending        FleetPending         `json:"-"`
}

// FleetSummary counts the shown crews (one per head row); Dropped counts peer rows that claimed
// a local crew id (crew's per-repo drops plus the cross-repo ones).
type FleetSummary struct {
	Crews          int            `json:"crews"`
	HiddenCrews    int            `json:"hidden_crews"`
	Workers        int            `json:"workers"`
	ByState        map[string]int `json:"by_state"`
	Attention      int            `json:"attention"`
	Hosts          int            `json:"hosts"`
	HostsUnhealthy int            `json:"hosts_unhealthy"`
	Dropped        int            `json:"dropped"`
}

// FleetCrewRow is one crew on one repo bus (local) or one peer host's crew.
// Peer rows have no dispatcher, repo or dir and never any action. A crew's
// rows form a group: the head row, then Sub rows for other repos with work;
// the head's IdleRepos counts the repos folded away. Section is the group's
// live, orphaned, peer or dead bucket.
type FleetCrewRow struct {
	Key            string               `json:"key"`
	Origin         Origin               `json:"origin"`
	Host           string               `json:"host"`
	Repo           *string              `json:"repo"`
	Common         *string              `json:"common"`
	Crew           string               `json:"crew"`
	HomeRepo       *string              `json:"home_repo"`
	Orphaned       bool                 `json:"orphaned"`
	Actions        []Action             `json:"actions"`
	Dispatcher     *FleetCrewDispatcher `json:"dispatcher"`
	AttentionState *string              `json:"attention_state"`
	Workers        FleetWorkerCounts    `json:"workers"`
	RemoteWorkers  int                  `json:"remote_workers"`
	Attention      int                  `json:"attention"`
	Sub            bool                 `json:"sub"`
	Section        string               `json:"section"`
	IdleRepos      int                  `json:"idle_repos"`
	Dir            *string              `json:"dir" dash:"raw"`
}

// FleetCrewDispatcher is the crew's home dispatcher, inherited by its rows in
// other repos.
type FleetCrewDispatcher struct {
	State      string  `json:"state"`
	Lane       *string `json:"lane"`
	LastEventS *int64  `json:"last_event_s"`
	PID        *int    `json:"pid"`
}

type FleetWorkerCounts struct {
	Total   int            `json:"total"`
	ByState map[string]int `json:"by_state"`
}

type FleetCapacity struct {
	Budget      *int                   `json:"budget"`
	Weight      *int                   `json:"weight"`
	Pending     *int                   `json:"pending"`
	Free        *int                   `json:"free"`
	Leads       []data.FleetLead       `json:"leads"`
	Load1       *float64               `json:"load1"`
	Cores       *int                   `json:"cores"`
	LoadOver    *bool                  `json:"load_over"`
	LocalModels []data.FleetLocalModel `json:"local_models"`
	Engines     []FleetEngineRow       `json:"engines"`
	Budgets     []BudgetRow            `json:"budgets"`
}

type FleetEngineRow struct {
	Engine       string   `json:"engine"`
	InBudget     bool     `json:"in_budget"`
	HotWindows   []string `json:"hot_windows"`
	LimitReached bool     `json:"limit_reached"`
}

// FleetHoldRow is a fleet hold from `crew fleet queue` or a quota hold from a
// crew's fleet status; the queue-only fields are null on quota holds.
type FleetHoldRow struct {
	ID       string  `json:"id"`
	Repo     string  `json:"repo"`
	Crew     string  `json:"crew"`
	Branch   *string `json:"branch"`
	Kind     string  `json:"kind"`
	Prio     *string `json:"prio"`
	RepoPrio *int64  `json:"repo_prio"`
	Weight   *int    `json:"weight"`
	State    string  `json:"state"`
	Reason   *string `json:"reason"`
	WaitedS  int64   `json:"waited_s"`
	Matured  bool    `json:"matured"`
}

type FleetHostRow struct {
	Name  string         `json:"name"`
	SSH   string         `json:"ssh"`
	Peer  bool           `json:"peer"`
	Relay FleetHostRelay `json:"relay"`
	Sync  FleetHostSync  `json:"sync"`
	Reach FleetHostReach `json:"reach"`
}

// FleetHostRelay: up when a crew relays the host, down when a live-dispatcher
// crew has an unfinished worker there and no live relay for it, else idle.
type FleetHostRelay struct {
	State string   `json:"state"`
	Crews []string `json:"crews"`
}

// FleetHostSync folds a peer host's links across repos; n/a for non-peers.
type FleetHostSync struct {
	State    string  `json:"state"`
	LastOKTS *int64  `json:"last_ok_ts"`
	LagSec   *int64  `json:"lag_sec"`
	Error    *string `json:"error"`
}

// FleetHostReach is the collector's reach probe; Source is null when none ran.
type FleetHostReach struct {
	Source   *string `json:"source"`
	Online   *bool   `json:"online"`
	LastSeen *int64  `json:"last_seen"`
	Error    *string `json:"error"`
}

// repoRoot is the main worktree of a git common dir, where crew commands for
// that repo's crews must run.
func repoRoot(common string) string {
	if strings.HasSuffix(common, "/.git") {
		return filepath.Dir(common)
	}
	return common
}

// fleetCrew is one crew row before the active filter. Local rows point into
// the fleet status doc, home at the row whose dispatcher they inherit; peer
// rows hold their deduped peer worker rows.
type fleetCrew struct {
	key      string
	origin   Origin
	host     string
	crew     string
	repo     *data.FleetRepo
	local    *data.FleetCrew
	homeRepo *data.FleetRepo
	home     *data.FleetCrew
	peerRows []map[string]any
}

func dispatcherRank(state string) int {
	switch state {
	case "live":
		return 0
	case "dead":
		return 1
	case "closed":
		return 2
	}
	return 3
}

// keptRepos marks one repo per canonical common dir, so a repo reached
// through a symlink is not listed twice. The copy whose path is already
// canonical wins, else the first.
func keptRepos(repos []data.FleetRepo) []bool {
	keep := make([]bool, len(repos))
	chosen := map[string]int{}
	for i, r := range repos {
		canon := r.Common
		if p, err := filepath.EvalSymlinks(r.Common); err == nil {
			canon = p
		}
		j, ok := chosen[canon]
		if !ok || (r.Common == canon && repos[j].Common != canon) {
			chosen[canon] = i
		}
	}
	for _, i := range chosen {
		keep[i] = true
	}
	return keep
}

// fleetCrews lists local rows in repo order, then peer crews in first-seen
// order, and counts the peer rows dropped for claiming a local crew id.
func fleetCrews(f FleetInputs) ([]fleetCrew, int) {
	st := f.Fleet.Status
	if st == nil {
		return nil, 0
	}
	type ref struct{ rank, repo, crew int }
	homes := map[string]ref{}
	keep := keptRepos(st.Repos)
	for ri, repo := range st.Repos {
		if !keep[ri] {
			continue
		}
		for ci, c := range repo.Crews {
			r := dispatcherRank(c.Dispatcher.State)
			if h, ok := homes[c.CrewID]; !ok || r < h.rank {
				homes[c.CrewID] = ref{r, ri, ci}
			}
		}
	}
	var crews []fleetCrew
	for ri := range st.Repos {
		if !keep[ri] {
			continue
		}
		repo := &st.Repos[ri]
		for ci := range repo.Crews {
			c := &repo.Crews[ci]
			h := homes[c.CrewID]
			crews = append(crews, fleetCrew{
				key: repo.Host + "|" + repo.Common + "|" + c.CrewID, origin: OriginLocal, host: repo.Host, crew: c.CrewID,
				repo: repo, local: c, homeRepo: &st.Repos[h.repo], home: &st.Repos[h.repo].Crews[h.crew],
			})
		}
	}

	dropped := 0
	groups := map[[2]string]int{}
	seen := map[[3]string]bool{}
	for ri, repo := range st.Repos {
		if !keep[ri] {
			continue
		}
		dropped += repo.Peers.Dropped
		for _, row := range repo.Peers.Rows {
			host, id := str(row, "host"), str(row, "crew_id")
			dk := [3]string{host, id, str(row, "branch")}
			if seen[dk] {
				continue
			}
			seen[dk] = true
			if _, local := homes[id]; local {
				dropped++
				continue
			}
			i, ok := groups[[2]string{host, id}]
			if !ok {
				i = len(crews)
				groups[[2]string{host, id}] = i
				crews = append(crews, fleetCrew{key: host + "||" + id, origin: OriginPeer, host: host, crew: id})
			}
			crews[i].peerRows = append(crews[i].peerRows, row)
		}
	}
	return crews, dropped
}

// ActiveCommons lists the local repo commons holding a crew BuildFleet will
// not hide for lack of work: a live dispatcher or a working worker (see
// working).
func ActiveCommons(sec data.FleetSection) []string {
	crews, _ := fleetCrews(FleetInputs{Fleet: sec})
	var commons []string
	for _, c := range crews {
		if c.origin != OriginLocal || slices.Contains(commons, c.repo.Common) {
			continue
		}
		active := c.live()
		for _, w := range c.local.Workers {
			active = active || WorkerRowOf(nil, c.crew, w).working()
		}
		if active {
			commons = append(commons, c.repo.Common)
		}
	}
	return commons
}

func (c fleetCrew) live() bool {
	return c.origin == OriginLocal && c.home.Dispatcher.State == "live"
}

func (c fleetCrew) dir() *string {
	if c.origin == OriginPeer {
		return nil
	}
	root := repoRoot(c.repo.Common)
	return &root
}

func (c fleetCrew) relaunch() Action {
	return newAction("", "relaunch", "relaunch dispatcher", false, RelaunchArgv(c.crew, repoRoot(c.homeRepo.Common)))
}

// inputs is the crew row's Overview inputs: one repo, one digest.
func (c fleetCrew) inputs(f FleetInputs) Inputs {
	in := Inputs{
		Snap:   data.Snapshot{Now: f.nowSec(), Budget: f.Snap.Budget},
		Loaded: Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
		NowMS:  f.NowMS,
		Crew:   f.Crew,
	}
	if c.origin == OriginLocal {
		in.ReapKeeps = f.ReapKeeps
	}
	if c.origin == OriginPeer {
		in.Snap.Roster.Peers = c.peerRows
		return in
	}
	holds := make([]map[string]any, 0, len(c.local.Holds))
	for _, h := range c.local.Holds {
		holds = append(holds, map[string]any{"id": h.ID, "kind": h.Kind, "waited_s": h.WaitedS, "due": h.Due})
	}
	rc := data.RosterCrew{ID: c.crew, Workers: c.local.Workers, Holds: holds}
	if lane := c.home.Dispatcher.Lane; c.live() && lane != nil {
		rc.Lane = &data.LaneStatus{State: *lane}
	}
	in.Snap.Roster.Crews = []data.RosterCrew{rc}
	in.Bus = c.digest(f)
	in.Dir = c.dir()
	return in
}

// digest is the repo's digest with Recent cut to this crew's own events.
func (c fleetCrew) digest(f FleetInputs) data.BusDigest {
	d, ok := f.Digests[c.repo.Common]
	if !ok {
		if e, failed := f.DigestErrs[c.repo.Common]; failed {
			d.Err = &e
		}
		return d
	}
	branches := map[string]bool{}
	for _, w := range c.local.Workers {
		branches[str(w, "branch")] = true
	}
	recent := make([]data.RecentEvent, 0, len(d.Recent))
	for _, e := range d.Recent {
		if e.From == "dispatcher:"+c.crew {
			recent = append(recent, e)
			continue
		}
		if b, worker := strings.CutPrefix(e.From, "worker:"); worker {
			if b, _, _ = strings.Cut(b, "#"); branches[b] {
				recent = append(recent, e)
			}
		}
	}
	d.Recent = recent
	return d
}

// CrewInputs is the drill-down: the Overview inputs of one fleet crew row.
func CrewInputs(f FleetInputs, crewKey string) (Inputs, bool) {
	crews, _ := fleetCrews(f)
	for _, c := range crews {
		if c.key == crewKey {
			return c.inputs(f), true
		}
	}
	return Inputs{}, false
}

// working is the one rule for a worker that keeps its crew active and, under
// a dead dispatcher, orphaned: non-terminal and not stale. A stale row is
// crew's verdict of a local row with no sign of life; it still counts in the
// crew's worker totals and its own state items.
func (w WorkerRow) working() bool { return !isTerminal(w.State) && !w.stale }

func unfinished(rows []WorkerRow) int {
	n := 0
	for _, w := range rows {
		if w.working() {
			n++
		}
	}
	return n
}

// relaunchable is a dead dispatcher only: the protocol never relaunches a
// closed one, and a deregister deletes its launch record.
func (c fleetCrew) relaunchable() bool { return c.home.Dispatcher.State == "dead" }

func (c fleetCrew) orphaned(rows []WorkerRow) bool {
	if c.origin == OriginPeer {
		return false
	}
	s := c.home.Dispatcher.State
	return (s == "dead" || s == "closed") && unfinished(rows) > 0
}

// linkItems are the crew-level items the repo-scoped Overview cannot see: a
// remote worker the live dispatcher has lost sight of, and an orphaned crew.
func (c fleetCrew) linkItems(rows []WorkerRow) []AttentionItem {
	var items []AttentionItem
	if c.live() {
		for _, w := range rows {
			if w.Origin != OriginRemote || w.HostError == nil || isTerminal(w.State) {
				continue
			}
			msg := fmt.Sprintf("%s on %s: %s", w.display(), w.Host, *w.HostError)
			items = append(items, attentionOf(w, KindRemoteWatchDead, SeverityCritical, "p", msg, ""))
		}
	}
	if c.orphaned(rows) {
		item := AttentionItem{
			ID: fmt.Sprintf("%s:%s:", KindDispatcherGone, c.crew), Crew: c.crew, Origin: OriginLocal,
			Severity: SeverityWarning, Kind: KindDispatcherGone,
			Clears:  clearsText(KindDispatcherGone, clearsCtx{}),
			Message: fmt.Sprintf("dispatcher closed, %d workers", unfinished(rows)),
			AgeSec:  derefInt(c.home.Dispatcher.LastEventS),
		}
		if c.relaunchable() {
			act := c.relaunch()
			item.Severity, item.Action = SeverityCritical, &act
			item.Message = fmt.Sprintf("dispatcher gone, %d workers", unfinished(rows))
		}
		items = append(items, item)
	}
	return items
}

func stateKey(state string) string {
	if state == "" {
		return "unknown"
	}
	return state
}

func (c fleetCrew) row(rows []WorkerRow, attention int) FleetCrewRow {
	r := FleetCrewRow{
		Key: c.key, Origin: c.origin, Host: c.host, Crew: c.crew, Actions: []Action{},
		Workers:   FleetWorkerCounts{Total: len(rows), ByState: map[string]int{}},
		Attention: attention,
	}
	for _, w := range rows {
		r.Workers.ByState[stateKey(w.State)]++
		if w.Origin == OriginRemote {
			r.RemoteWorkers++
		}
	}
	if c.origin == OriginPeer {
		return r
	}
	d := c.home.Dispatcher
	r.Repo, r.Common, r.HomeRepo = &c.repo.Repo, &c.repo.Common, &c.homeRepo.Repo
	r.Dispatcher = &FleetCrewDispatcher{State: d.State, Lane: d.Lane, LastEventS: d.LastEventS, PID: d.PID}
	r.AttentionState = &c.home.Attention
	r.Dir = c.dir()
	if c.orphaned(rows) {
		r.Orphaned = true
		if c.relaunchable() {
			r.Actions = []Action{c.relaunch()}
		}
	}
	return r
}

type builtCrew struct {
	fleetCrew
	rows []WorkerRow
}

func BuildFleet(f FleetInputs) FleetView {
	now := f.nowSec()
	v := FleetView{
		SchemaVersion:  SchemaVersion,
		View:           "fleet",
		GeneratedAt:    now,
		Summary:        FleetSummary{ByState: map[string]int{}},
		Attention:      []AttentionItem{},
		Crews:          []FleetCrewRow{},
		Capacity:       fleetCapacity(f),
		Holds:          fleetHolds(f),
		SuggestedHosts: nonNil(f.Fleet.Suggested),
		Errors:         fleetErrors(f),
		Pending:        f.Pending,
	}
	crews, dropped := fleetCrews(f)
	v.Summary.Dropped = dropped
	var locals []builtCrew
	var built []builtRow
	for _, c := range crews {
		in := c.inputs(f)
		rows := overviewRows(in)
		if c.origin == OriginLocal {
			locals = append(locals, builtCrew{c, rows})
		}
		items := append(buildAttention(in, rows), c.linkItems(rows)...)
		if !c.live() && unfinished(rows) == 0 && len(items) == 0 {
			v.Summary.HiddenCrews++
			continue
		}
		key := c.key
		for i := range items {
			items[i].CrewKey = &key
			items[i].ID = key + "#" + items[i].ID
		}
		row := c.row(rows, len(items))
		built = append(built, builtRow{row: row, home: c.repo == c.homeRepo, live: c.live()})
		v.Attention = append(v.Attention, items...)
		v.Summary.Workers += row.Workers.Total
		for s, n := range row.Workers.ByState {
			v.Summary.ByState[s] += n
		}
	}
	var hostItems []AttentionItem
	v.Hosts, hostItems = fleetHosts(f, locals, now)
	v.Attention = append(v.Attention, hostItems...)
	sortAttention(v.Attention)

	v.Crews, v.Summary.Crews = groupCrews(built)
	v.Summary.Attention = len(v.Attention)
	v.Summary.Hosts = len(v.Hosts)
	for _, h := range v.Hosts {
		if h.Relay.State == "down" || h.Sync.State == "stale" || h.Sync.State == "failed" || (h.Reach.Online != nil && !*h.Reach.Online) {
			v.Summary.HostsUnhealthy++
		}
	}
	return v
}

type builtRow struct {
	row  FleetCrewRow
	home bool
	live bool
}

func homeRank(home bool) int {
	if home {
		return 0
	}
	return 1
}

type crewGroup struct {
	rows      []FleetCrewRow
	rank      int
	attention int
}

// groupCrews gathers each local crew's per-repo rows (peer rows stand alone)
// under a head and orders the groups live, orphaned, peer, dead. Non-head
// rows with no workers and no attention fold into the head's IdleRepos.
func groupCrews(built []builtRow) ([]FleetCrewRow, int) {
	byCrew := map[[2]string][]builtRow{}
	var order [][2]string
	for _, b := range built {
		k := [2]string{b.row.Host, b.row.Crew}
		if b.row.Origin == OriginPeer {
			k = [2]string{b.row.Host, b.row.Key}
		}
		if _, ok := byCrew[k]; !ok {
			order = append(order, k)
		}
		byCrew[k] = append(byCrew[k], b)
	}
	groups := make([]crewGroup, 0, len(order))
	for _, k := range order {
		rows := byCrew[k]
		slices.SortStableFunc(rows, func(a, b builtRow) int {
			return cmp.Or(cmp.Compare(homeRank(a.home), homeRank(b.home)), cmp.Compare(deref(a.row.Repo), deref(b.row.Repo)))
		})
		head := rows[0].row
		orphaned := head.Orphaned
		g := crewGroup{attention: head.Attention}
		idle := 0
		for _, b := range rows[1:] {
			orphaned = orphaned || b.row.Orphaned
			if b.row.Workers.Total == 0 && b.row.Attention == 0 {
				idle++
				continue
			}
			b.row.Sub = true
			g.attention += b.row.Attention
			g.rows = append(g.rows, b.row)
		}
		head.IdleRepos = idle
		g.rank, head.Section = 3, "dead"
		switch {
		case head.Origin == OriginPeer:
			g.rank, head.Section = 2, "peer"
		case rows[0].live:
			g.rank, head.Section = 0, "live"
		case orphaned:
			g.rank, head.Section = 1, "orphaned"
		}
		for i := range g.rows {
			g.rows[i].Section = head.Section
		}
		g.rows = slices.Insert(g.rows, 0, head)
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b crewGroup) int {
		return cmp.Or(
			cmp.Compare(a.rank, b.rank),
			cmp.Compare(b.attention, a.attention),
			cmp.Compare(a.rows[0].Host, b.rows[0].Host),
			cmp.Compare(a.rows[0].Crew, b.rows[0].Crew),
			cmp.Compare(a.rows[0].Key, b.rows[0].Key),
		)
	})
	out := []FleetCrewRow{}
	for _, g := range groups {
		out = append(out, g.rows...)
	}
	return out, len(groups)
}

func fleetCapacity(f FleetInputs) FleetCapacity {
	c := FleetCapacity{
		Leads: []data.FleetLead{}, LocalModels: []data.FleetLocalModel{}, Engines: []FleetEngineRow{},
		Budgets: budgetRows(f.Snap.Budget.Report),
	}
	if f.Fleet.Status == nil || f.Fleet.Status.Machine == nil {
		return c
	}
	m := f.Fleet.Status.Machine
	c.Budget, c.Weight, c.Pending, c.Free = &m.Budget, &m.Weight, &m.Pending, m.Free
	c.Load1, c.Cores, c.LoadOver = m.Load1, m.Cores, &m.LoadOver
	c.Leads, c.LocalModels = nonNil(m.Leads), nonNil(m.LocalModels)
	names := make([]string, 0, len(m.Engines))
	for name := range m.Engines {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		e := m.Engines[name]
		c.Engines = append(c.Engines, FleetEngineRow{Engine: name, InBudget: e.InBudget, HotWindows: nonNil(e.HotWindows), LimitReached: e.LimitReached})
	}
	return c
}

func prioRank(p *string) int {
	switch deref(p) {
	case "high":
		return 0
	case "normal":
		return 1
	case "low":
		return 2
	}
	return 3
}

func fleetHolds(f FleetInputs) []FleetHoldRow {
	holds := []FleetHoldRow{}
	for _, q := range f.Fleet.Queue {
		holds = append(holds, FleetHoldRow{
			ID: q.ID, Repo: q.Repo, Crew: q.CrewID, Branch: &q.Branch, Kind: "fleet",
			Prio: &q.Prio, RepoPrio: &q.RepoPrio, Weight: &q.Weight, State: q.State, Reason: q.Reason,
			WaitedS: q.WaitedS, Matured: q.State == "due",
		})
	}
	if st := f.Fleet.Status; st != nil {
		keep := keptRepos(st.Repos)
		for ri, repo := range st.Repos {
			if !keep[ri] {
				continue
			}
			for _, c := range repo.Crews {
				for _, h := range c.Holds {
					if h.Kind != "quota" {
						continue
					}
					state := "waiting"
					if h.Due {
						state = "due"
					}
					holds = append(holds, FleetHoldRow{
						ID: h.ID, Repo: repo.Repo, Crew: c.CrewID, Kind: "quota", State: state, WaitedS: h.WaitedS, Matured: h.Due,
					})
				}
			}
		}
	}
	slices.SortFunc(holds, func(a, b FleetHoldRow) int {
		matured := func(h FleetHoldRow) int {
			if h.Matured {
				return 0
			}
			return 1
		}
		return cmp.Or(
			cmp.Compare(matured(a), matured(b)),
			cmp.Compare(prioRank(a.Prio), prioRank(b.Prio)),
			cmp.Compare(b.WaitedS, a.WaitedS),
			cmp.Compare(a.ID, b.ID),
			cmp.Compare(a.Repo, b.Repo),
		)
	})
	return holds
}

// hostRelay reads every local crew row's relays for host; lost counts the
// unfinished remote workers of live-dispatcher crews with no live relay to
// it, lastCursor is the newest cursor among those crews' dead relays.
func hostRelay(host string, locals []builtCrew) (relay FleetHostRelay, lost int, lastCursor int64) {
	relay = FleetHostRelay{Crews: []string{}}
	for _, c := range locals {
		live, cursor := false, int64(0)
		for _, r := range c.local.Relays {
			if r.Host != host {
				continue
			}
			if r.Live {
				live = true
			} else if r.CursorTS != nil {
				cursor = max(cursor, *r.CursorTS)
			}
		}
		if live {
			if !slices.Contains(relay.Crews, c.crew) {
				relay.Crews = append(relay.Crews, c.crew)
			}
			continue
		}
		if !c.live() {
			continue
		}
		n := 0
		for _, w := range c.rows {
			if w.Origin == OriginRemote && w.Host == host && !isTerminal(w.State) {
				n++
			}
		}
		if n > 0 {
			lost += n
			lastCursor = max(lastCursor, cursor)
		}
	}
	slices.Sort(relay.Crews)
	switch {
	case lost > 0:
		relay.State = "down"
	case len(relay.Crews) > 0:
		relay.State = "up"
	default:
		relay.State = "idle"
	}
	return relay, lost, lastCursor
}

func hostSync(h data.HostConfig, st *data.FleetStatus, now int64) FleetHostSync {
	if !h.Peer {
		return FleetHostSync{State: "n/a"}
	}
	var ok, errTS *int64
	var detail *string
	if st != nil {
		for _, repo := range st.Repos {
			for _, l := range repo.Peers.Links {
				if l.Host != h.Name {
					continue
				}
				if l.OKTS != nil && (ok == nil || *l.OKTS > *ok) {
					ok = l.OKTS
				}
				if l.ErrTS != nil && (errTS == nil || *l.ErrTS > *errTS) {
					errTS, detail = l.ErrTS, l.Detail
				}
			}
		}
	}
	s := FleetHostSync{LastOKTS: ok}
	if ok != nil {
		lag := now - *ok/1000
		s.LagSec = &lag
	}
	switch {
	case errTS != nil && (ok == nil || *errTS > *ok):
		s.State, s.Error = "failed", detail
	case ok == nil:
		s.State = "never"
	case *s.LagSec > peerSyncStaleSec:
		s.State = "stale"
	default:
		s.State = "ok"
	}
	return s
}

func hostReach(name string, reach []data.HostReach) FleetHostReach {
	for _, r := range reach {
		if r.Host == name {
			src := r.Source
			return FleetHostReach{Source: &src, Online: r.Online, LastSeen: r.LastSeen, Error: r.Error}
		}
	}
	return FleetHostReach{}
}

// fleetHosts builds one row per configured host plus its host-level items.
func fleetHosts(f FleetInputs, locals []builtCrew, now int64) ([]FleetHostRow, []AttentionItem) {
	rows := make([]FleetHostRow, 0, len(f.Fleet.Hosts))
	var items []AttentionItem
	for _, h := range f.Fleet.Hosts {
		relay, lost, cursor := hostRelay(h.Name, locals)
		row := FleetHostRow{
			Name: h.Name, SSH: h.SSH, Peer: h.Peer, Relay: relay,
			Sync: hostSync(h, f.Fleet.Status, now), Reach: hostReach(h.Name, f.Fleet.Reach),
		}
		rows = append(rows, row)
		if lost > 0 {
			var age int64
			if cursor > 0 {
				age = now - cursor/1000
			}
			items = append(items, AttentionItem{
				ID: fmt.Sprintf("%s::%s", KindRelayLost, h.Name), Host: h.Name, Origin: OriginRemote,
				Severity: SeverityCritical, Kind: KindRelayLost, AgeSec: age, Clears: clearsText(KindRelayLost, clearsCtx{}),
				Message: fmt.Sprintf("relay to %s lost (%d remote workers)", h.Name, lost),
			})
		}
		if s := row.Sync; s.State == "stale" || s.State == "failed" {
			msg := fmt.Sprintf("peer %s sync stale %s", h.Name, fmtDur(*s.LagSec))
			if s.State == "failed" {
				msg = fmt.Sprintf("peer %s sync failed", h.Name)
				if s.Error != nil {
					msg += ": " + *s.Error
				}
			}
			items = append(items, AttentionItem{
				ID: fmt.Sprintf("%s::%s", KindPeerStale, h.Name), Host: h.Name, Origin: OriginPeer,
				Severity: SeverityWarning, Kind: KindPeerStale, AgeSec: derefInt(s.LagSec), Message: msg,
				Clears: clearsText(KindPeerStale, clearsCtx{}),
			})
		}
	}
	return rows, items
}

func fleetErrors(f FleetInputs) []string {
	errs := []string{}
	add := func(label string, e *string) {
		if e != nil {
			errs = append(errs, label+": "+*e)
		}
	}
	add("fleet status", f.Fleet.StatusError)
	add("fleet queue", f.Fleet.QueueError)
	if data.TailscaleEnabled(f.Snap.Settings.Rows) {
		add("tailscale", f.Fleet.TailscaleError)
	}
	if f.Fleet.Status == nil {
		return errs
	}
	keep := keptRepos(f.Fleet.Status.Repos)
	for ri, repo := range f.Fleet.Status.Repos {
		if !keep[ri] {
			continue
		}
		add("repo "+repo.Repo+": peers", repo.PeersError)
		if e, ok := f.DigestErrs[repo.Common]; ok {
			add("repo "+repo.Repo+": bus", &e)
		} else if d, ok := f.Digests[repo.Common]; ok {
			add("repo "+repo.Repo+": bus", d.Err)
		}
	}
	return errs
}
