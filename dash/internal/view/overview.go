package view

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

var (
	phaseNames     = []string{"spec", "plan", "execute", "gate", "review", "ship"}
	terminalStates = []string{"done", "failed", "exited", "reaped", "migrated"}
)

const activityBuckets = 30

// eventWindowSec bounds the unscoped and fleet event lists.
const eventWindowSec = 3600

type OverviewView struct {
	SchemaVersion int              `json:"schema_version"`
	View          string           `json:"view"`
	GeneratedAt   int64            `json:"generated_at"`
	Summary       Summary          `json:"summary"`
	Attention     []AttentionItem  `json:"attention"`
	Workers       []OverviewWorker `json:"workers"`
	Agents        []AgentRow       `json:"agents"`
	Monitors      Monitors         `json:"monitors"`
	Events        []EventRow       `json:"events"`
	Scope         OverviewScope    `json:"scope"`
	Errors        []string         `json:"errors"`
}

// OverviewScope reports which crew the view is narrowed to; Crew is null when
// unscoped.
type OverviewScope struct {
	Crew   *string `json:"crew"`
	Source string  `json:"source"`
	Known  bool    `json:"known"`
	Crews  int     `json:"crews"`
	Error  string  `json:"error,omitempty"`
}

type Summary struct {
	Workers    int            `json:"workers"`
	ByState    map[string]int `json:"by_state"`
	Attention  int            `json:"attention"`
	Agents     int            `json:"agents"`
	PRsWatched int            `json:"prs_watched"`
}

type PhaseStep struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	SinceSec *int64 `json:"since_sec"`
}

type Phase struct {
	Current *string     `json:"current"`
	Steps   []PhaseStep `json:"steps"`
}

type Acceptance struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// OverviewWorker is one non-reaped worker. Phase and Acceptance are digest
// fields: null for remote and peer rows, whose events never reach the local
// bus, and Acceptance is also null until a ledger exists.
type OverviewWorker struct {
	Crew         string            `json:"crew"`
	Branch       string            `json:"branch"`
	Name         *string           `json:"name"`
	Tmux         *string           `json:"tmux,omitempty"`
	Host         string            `json:"host"`
	Origin       Origin            `json:"origin"`
	State        string            `json:"state"`
	Detail       *string           `json:"detail"`
	Tier         *string           `json:"tier"`
	Engine       *string           `json:"engine"`
	Model        *string           `json:"model"`
	Title        *string           `json:"title"`
	PRURL        *string           `json:"pr_url"`
	PR           *int              `json:"pr"`
	AgeSec       *int64            `json:"age_sec"`
	BlockedSince *int64            `json:"blocked_since"`
	AnsweredAt   *int64            `json:"answered_at"`
	Phase        *Phase            `json:"phase"`
	Acceptance   *Acceptance       `json:"acceptance"`
	Actions      []Action          `json:"actions"`
	NeedsCleanup bool              `json:"needs_cleanup,omitempty"`
	Dispatcher   *WorkerDispatcher `json:"dispatcher"`
}

type AgentNode struct {
	Kind   string  `json:"kind"`
	Label  *string `json:"label,omitempty"`
	Role   *string `json:"role"`
	Engine *string `json:"engine"`
	Model  *string `json:"model"`
	Effort *string `json:"effort,omitempty"`
	Tier   *string `json:"tier"`
	State  string  `json:"state"`
	Detail *string `json:"detail"`
	AgeSec *int64  `json:"age_sec"`
}

// AgentRow is one lead with its role panes. Roles is null when unknown (remote
// and peer leads); Execute is the lead's recent subagents from the bus and is
// empty for remote and peer leads.
type AgentRow struct {
	Crew    string      `json:"crew"`
	Branch  string      `json:"branch"`
	Host    string      `json:"host"`
	Origin  Origin      `json:"origin"`
	Lead    AgentNode   `json:"lead"`
	Roles   []AgentNode `json:"roles"`
	Execute []AgentNode `json:"execute"`
}

type WatchdogRow struct {
	Crew     string `json:"crew"`
	Branch   string `json:"branch"`
	Host     string `json:"host"`
	Origin   Origin `json:"origin"`
	State    string `json:"state"`
	Label    string `json:"label"`
	Activity []int  `json:"activity"`
}

type PRCheckRow struct {
	Crew      string `json:"crew"`
	Branch    string `json:"branch"`
	Host      string `json:"host"`
	Origin    Origin `json:"origin"`
	PR        int    `json:"pr"`
	Checks    string `json:"checks"`
	Mergeable string `json:"mergeable"`
	AgeSec    int64  `json:"age_sec"`
}

type BudgetRow struct {
	Engine   string  `json:"engine"`
	Label    string  `json:"label"`
	UsedPct  float64 `json:"used_pct"`
	AheadPts *int64  `json:"ahead_pts"`
	Pace     string  `json:"pace"`
	Verdict  *string `json:"verdict"`
}

type Monitors struct {
	Watchdogs []WatchdogRow `json:"watchdogs"`
	PRChecks  []PRCheckRow  `json:"pr_checks"`
	Budgets   []BudgetRow   `json:"budgets"`
}

type EventRow struct {
	TS     int64  `json:"ts"`
	Worker string `json:"worker"`
	Text   string `json:"text"`
	Crew   string `json:"crew,omitempty"`
	Repo   string `json:"repo,omitempty"`
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isTerminal(state string) bool { return slices.Contains(terminalStates, state) }

// overviewRows are the typed local, remote and peer rows minus reaped ones.
func overviewRows(in Inputs) []WorkerRow {
	var rows []WorkerRow
	keep := func(w WorkerRow) {
		for i := range w.Actions {
			w.Actions[i] = withDir(w.Actions[i], in.Dir)
		}
		if w.State != "reaped" {
			rows = append(rows, w)
		}
	}
	for _, c := range in.Snap.Roster.Crews {
		for _, w := range c.Workers {
			keep(WorkerRowOf(in.Crew, c.ID, w))
		}
	}
	seen := map[data.WorkerKey]bool{}
	for _, w := range rows {
		seen[data.WorkerKey{Crew: w.Crew, Branch: w.Branch}] = true
	}
	for _, p := range in.Snap.Roster.Peers {
		w := WorkerRowOf(in.Crew, str(p, "crew_id"), p)
		if w.Branch != "" && seen[data.WorkerKey{Crew: w.Crew, Branch: w.Branch}] {
			continue
		}
		keep(w)
	}
	return rows
}

func sinceSec(now, tsMS int64) *int64 {
	n := now - tsMS/1000
	return &n
}

func phaseOf(w WorkerRow, d *data.WorkerDigest, now int64) *Phase {
	if w.Origin != OriginLocal {
		return nil
	}
	marks := map[string]int64{}
	var last string
	if d != nil {
		for _, m := range d.Stages {
			if _, seen := marks[m.Name]; !seen {
				marks[m.Name] = m.TS
			}
			last = m.Name
		}
	}
	allDone := w.State == "pr_open" || w.State == "done"
	cur := slices.Index(phaseNames, last)
	p := &Phase{Steps: make([]PhaseStep, len(phaseNames))}
	if !allDone && cur >= 0 {
		p.Current = &last
	}
	for i, name := range phaseNames {
		step := PhaseStep{Name: name, Status: "pending"}
		ts, entered := marks[name]
		if entered {
			step.SinceSec = sinceSec(now, ts)
		}
		switch {
		case allDone:
			step.Status = "done"
		case cur < 0 || i > cur:
		case i == cur:
			step.Status = "current"
		case entered:
			step.Status = "done"
		default:
			step.Status = "skipped"
		}
		p.Steps[i] = step
	}
	return p
}

func acceptanceOf(w WorkerRow, d *data.WorkerDigest) *Acceptance {
	if w.Origin != OriginLocal || d == nil {
		return nil
	}
	done, total, ok := ledgerCount(d.Ledger)
	if !ok {
		return nil
	}
	return &Acceptance{Done: done, Total: total}
}

func agentRow(w WorkerRow, in Inputs, now int64) AgentRow {
	row := AgentRow{
		Crew: w.Crew, Branch: w.Branch, Host: w.Host, Origin: w.Origin,
		Lead: AgentNode{
			Kind: "lead", Engine: w.Engine, Model: w.Model, Tier: w.Tier,
			State: w.State, Detail: w.Detail, AgeSec: w.AgeS,
		},
		Execute: []AgentNode{},
	}
	if w.Origin != OriginLocal {
		return row
	}
	row.Roles = []AgentNode{}
	var keys []data.RoleKey
	for k := range in.Bus.Roles {
		if k.Crew == w.Crew && k.Branch == w.Branch {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b data.RoleKey) int { return strings.Compare(a.Role, b.Role) })
	for _, k := range keys {
		r := in.Bus.Roles[k]
		role := k.Role
		node := AgentNode{Kind: "role", Role: &role, State: r.State, AgeSec: sinceSec(now, r.TS)}
		if node.State == "" {
			node.State = "unknown"
			node.AgeSec = nil
		}
		if r.Detail != "" {
			detail := r.Detail
			node.Detail = &detail
		}
		row.Roles = append(row.Roles, node)
	}
	key := data.WorkerKey{Crew: w.Crew, Branch: w.Branch}
	var session string
	if d := in.Bus.Workers[key]; d != nil {
		session = d.Session
	}
	for _, r := range in.Bus.Subagents[key] {
		row.Execute = append(row.Execute, subagentNode(r, w, session, now))
	}
	return row
}

// subagentNode shows a still-running run as unknown when its lead can no longer
// be finishing it: the lead is terminal, or a resumed session replaced the one
// that spawned it.
func subagentNode(r data.SubagentRun, w WorkerRow, session string, now int64) AgentNode {
	node := AgentNode{Kind: "subagent", State: r.State, Label: nonEmpty(r.Label), Role: nonEmpty(r.Agent),
		Engine: nonEmpty(r.Engine), Model: nonEmpty(r.Model), Effort: nonEmpty(r.Effort)}
	if r.State == "running" && (isTerminal(w.State) || (r.Session != "" && session != "" && r.Session != session)) {
		node.State = "unknown"
	}
	since := r.StartTS
	if node.State == "done" || node.State == "failed" {
		since = r.EndTS
	}
	node.AgeSec = sinceSec(now, since)
	return node
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func activityOf(d *data.WorkerDigest, now int64) []int {
	buckets := make([]int, activityBuckets)
	if d == nil {
		return buckets
	}
	for _, ts := range d.Activity {
		i := activityBuckets - 1 - int(max(now-ts/1000, 0)/60)
		if i >= 0 {
			buckets[i]++
		}
	}
	return buckets
}

// Age is coarse on purpose: a row's text only changes at minute and hour
// boundaries, so a live refresh does not reflow it every second.
func Age(sec int64) string {
	switch {
	case sec < 60:
		return "now"
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%dh", sec/3600)
}

func watchdogLabel(w WorkerRow) string {
	word := "ok"
	switch {
	case w.State == "" || w.PeerError != nil || w.HostError != nil:
		word = "?"
	case deref(w.Source) == "watchdog":
		detail := deref(w.Detail)
		word, _, _ = strings.Cut(detail, ":")
		if watchdogKind(detail) == KindStall {
			word = "stall"
		}
	}
	if w.AgeS == nil {
		return word
	}
	return word + " " + Age(*w.AgeS)
}

func paceOf(ahead *int64) string {
	switch {
	case ahead == nil:
		return "n/a"
	case *ahead > 0:
		return "ahead"
	case *ahead < 0:
		return "behind"
	}
	return "on_pace"
}

func budgetRows(r *data.BudgetReport) []BudgetRow {
	rows := []BudgetRow{}
	if r == nil {
		return rows
	}
	engines := make([]string, 0, len(r.Engines))
	for name := range r.Engines {
		engines = append(engines, name)
	}
	slices.Sort(engines)
	for _, name := range engines {
		if r.Engines[name] == nil {
			continue
		}
		for _, win := range r.Engines[name].Windows {
			rows = append(rows, BudgetRow{
				Engine: name, Label: name + " " + win.Key, UsedPct: win.UsedPct,
				AheadPts: win.AheadPts, Pace: paceOf(win.AheadPts), Verdict: win.Verdict,
			})
		}
	}
	return rows
}

// eventRows lists a ring newest first; repo labels fleet rows.
func eventRows(ring []data.RecentEvent, repo string) []EventRow {
	rows := make([]EventRow, 0, len(ring))
	for i := len(ring) - 1; i >= 0; i-- {
		e := ring[i]
		from, text := summarizeEvent(e)
		rows = append(rows, EventRow{TS: e.TS / 1000, Worker: from, Text: text, Crew: e.Crew, Repo: repo})
	}
	return rows
}

// overviewEvents is the scoped crew's whole ring, or the last hour of the
// repo's rows when unscoped.
func overviewEvents(in Inputs, now int64) []EventRow {
	if crew := in.Snap.Roster.Scope.Crew; crew != "" {
		return eventRows(in.Bus.RecentByCrew[crew], "")
	}
	return eventRows(recentSince(in.Bus.Recent, now), "")
}

func recentSince(ring []data.RecentEvent, now int64) []data.RecentEvent {
	cut := (now - eventWindowSec) * 1000
	return slices.DeleteFunc(slices.Clone(ring), func(e data.RecentEvent) bool { return e.TS < cut })
}

func overviewScope(sc data.Scope) OverviewScope {
	out := OverviewScope{Crew: nil, Source: sc.Source, Known: sc.Known, Crews: sc.Crews, Error: sc.Err}
	if sc.Crew != "" {
		out.Crew = &sc.Crew
	}
	if out.Source == "" {
		out.Source = "none"
	}
	return out
}

func overviewErrors(in Inputs, rows []WorkerRow) []string {
	errs := []string{}
	if e := in.Snap.Roster.Error; e != nil {
		errs = append(errs, "roster: "+*e)
	}
	if e := in.Snap.Roster.ForeignError; e != nil {
		errs = append(errs, *e)
	}
	for _, c := range in.Snap.Roster.Crews {
		if c.WorkersError != nil {
			errs = append(errs, fmt.Sprintf("crew %s: workers: %s", c.ID, *c.WorkersError))
		}
		if c.HoldsError != nil {
			errs = append(errs, fmt.Sprintf("crew %s: holds: %s", c.ID, *c.HoldsError))
		}
		if c.Lane != nil && c.Lane.State == "unknown" {
			errs = append(errs, fmt.Sprintf("crew %s: lane unknown: %s", c.ID, deref(c.Lane.Error)))
		}
	}
	add := func(label, host string, why *string) {
		if why == nil {
			return
		}
		if msg := fmt.Sprintf("%s %s: %s", label, host, *why); !slices.Contains(errs, msg) {
			errs = append(errs, msg)
		}
	}
	for _, w := range rows {
		add("peer", w.Host, w.PeerError)
		add("host", w.Host, w.HostError)
	}
	if in.Bus.Err != nil {
		errs = append(errs, "bus: "+*in.Bus.Err)
	}
	return errs
}

func BuildOverview(in Inputs) OverviewView {
	now := in.nowSec()
	rows := overviewRows(in)
	// Stale workers wait on a human, not an agent: list them after the active ones.
	slices.SortStableFunc(rows, func(a, b WorkerRow) int {
		return cmp.Compare(boolRank(a.NeedsCleanup), boolRank(b.NeedsCleanup))
	})
	v := OverviewView{
		SchemaVersion: SchemaVersion,
		View:          "overview",
		GeneratedAt:   now,
		Summary:       Summary{ByState: map[string]int{}},
		Attention:     buildAttention(in, rows),
		Workers:       make([]OverviewWorker, 0, len(rows)),
		Agents:        make([]AgentRow, 0, len(rows)),
		Monitors:      Monitors{Watchdogs: []WatchdogRow{}, PRChecks: []PRCheckRow{}, Budgets: budgetRows(in.Snap.Budget.Report)},
		Events:        overviewEvents(in, now),
		Scope:         overviewScope(in.Snap.Roster.Scope),
		Errors:        overviewErrors(in, rows),
	}
	dispatchers := map[string]*WorkerDispatcher{}
	for _, c := range in.Snap.Roster.Crews {
		dispatchers[c.ID] = dispatcherOf(c.Dispatcher)
	}
	for _, w := range rows {
		d := in.Bus.Workers[data.WorkerKey{Crew: w.Crew, Branch: w.Branch}]
		var dispatcher *WorkerDispatcher
		if w.Origin != OriginPeer {
			dispatcher = dispatchers[w.Crew]
		}
		v.Workers = append(v.Workers, OverviewWorker{
			Crew: w.Crew, Branch: w.Branch, Name: w.Name, Tmux: w.Tmux, Host: w.Host, Origin: w.Origin,
			State: w.State, Detail: w.Detail, Tier: w.Tier, Engine: w.Engine, Model: w.Model,
			Title: w.Title, PRURL: w.PRURL, PR: w.PR, AgeSec: w.AgeS,
			BlockedSince: w.BlockedSince, AnsweredAt: w.AnsweredAt,
			Phase: phaseOf(w, d, now), Acceptance: acceptanceOf(w, d), Actions: w.Actions,
			NeedsCleanup: w.NeedsCleanup, Dispatcher: dispatcher,
		})
		if !w.NeedsCleanup {
			agent := agentRow(w, in, now)
			v.Agents = append(v.Agents, agent)
			v.Summary.Agents += 1 + len(agent.Roles)
			for _, n := range agent.Execute {
				if n.State == "running" {
					v.Summary.Agents++
				}
			}
		}
		if w.State == "" {
			v.Summary.ByState["unknown"]++
		} else {
			v.Summary.ByState[w.State]++
		}

		if isTerminal(w.State) {
			continue
		}
		wd := WatchdogRow{Crew: w.Crew, Branch: w.Branch, Host: w.Host, Origin: w.Origin, State: w.State, Label: watchdogLabel(w)}
		if w.Origin == OriginLocal {
			wd.Activity = activityOf(d, now)
		}
		v.Monitors.Watchdogs = append(v.Monitors.Watchdogs, wd)
		if w.PR != nil && w.Origin != OriginPeer {
			if pr := in.Bus.PRs[*w.PR]; pr != nil {
				v.Monitors.PRChecks = append(v.Monitors.PRChecks, PRCheckRow{
					Crew: w.Crew, Branch: w.Branch, Host: w.Host, Origin: w.Origin, PR: pr.PR,
					Checks: pr.Checks, Mergeable: pr.Mergeable, AgeSec: now - pr.TS/1000,
				})
			}
		}
	}
	v.Summary.Workers = len(v.Workers)
	v.Summary.Attention = len(v.Attention)
	v.Summary.PRsWatched = len(v.Monitors.PRChecks)
	return v
}
