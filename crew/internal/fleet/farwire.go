package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

// A far answer is decoded into the wire types below, which hold no plain
// string: each far string is a farID, a farEnum, a farText or a farKey, and
// reaches a local type only through project, which validates it or drops its
// element. Nothing is rewritten: a `?` from repos.Sanitize is itself a glob
// character.

// farID is a far identifier, valid only as one of the kinds below.
type farID string

func (s farID) crew() bool  { return bus.ValidCrewID(string(s)) }
func (s farID) host() bool  { return remote.HostNameRe.MatchString(string(s)) }
func (s farID) hold() bool  { return holdIDRe.MatchString(string(s)) }
func (s farID) ident() bool { return repos.ValidIdent(string(s)) }

// optional is ident, or nothing at all.
func (s farID) optional() bool { return s == "" || s.ident() }

// holdIDRe is `crew hold add`'s id: its ms ts and pid.
var holdIDRe = regexp.MustCompile(`\A[0-9]+-[0-9]+\z`)

// farEnum is a far value valid only within a fixed set.
type farEnum string

func (e farEnum) in(set []string) bool { return slices.Contains(set, string(e)) }

// farText is far free text, kept only where nothing copies it into a command:
// a skipped repo's status and an inbox reply's body.
type farText string

func (t farText) text() string { return scrub(string(t)) }

// farKey is a far common dir: only ever hashed into a key, so any bytes do,
// and never shown.
type farKey string

// abandonedDetail is the one worker detail a lane compares.
const abandonedDetail = "blocked, no dispatcher reply"

var (
	dispatcherStates = []string{"live", "dead", "closed"}
	laneStates       = []string{"alive", "stale", "dead"}
	attentions       = []string{"attended", "unattended", "idle", "guest"}
	holdKinds        = []string{"quota", "fleet"}
	parkedReasons    = []string{"oversize", "demoted", "ineligible"}
	tiers            = []string{"trivial", "standard", "deep"}
	replyKinds       = []string{"ack", "decline", "answer"}
	workerStates     = slices.Concat(roster.LiveStates, []string{
		string(bus.StateDone), string(bus.StateFailed), string(bus.StateExited), string(bus.StateReaped)})
	rowSources     = []string{"watchdog", "reap"}
	blockedSources = []string{"worker", "watchdog"}
	details        = []string{abandonedDetail}
)

type farWire struct {
	Self      farID          `json:"self"`
	Status    farStatus      `json:"status"`
	Inbox     []farBatch     `json:"inbox"`
	Misrouted []farMisrouted `json:"misrouted"`
	Cursor    int64          `json:"cursor"`
}

type farStatus struct {
	TS      int64        `json:"ts"`
	Host    farID        `json:"host"`
	Machine *farLoad     `json:"machine"`
	Queue   farQueue     `json:"queue"`
	Repos   []farRepo    `json:"repos"`
	Skipped []farSkipped `json:"skipped"`
}

type farLoad struct {
	Budget     int                 `json:"budget"`
	Weight     int                 `json:"weight"`
	Pending    int                 `json:"pending"`
	Free       *int                `json:"free"`
	Leads      []farLead           `json:"leads"`
	Load1      *float64            `json:"load1"`
	Cores      *int                `json:"cores"`
	LoadOver   bool                `json:"load_over"`
	Engines    map[farID]farEngine `json:"engines"`
	BudgetAgeS *int                `json:"budget_age_s"`
}

type farLead struct {
	Repo   farID   `json:"repo"`
	Branch farID   `json:"branch"`
	Name   farID   `json:"name"`
	Tier   farEnum `json:"tier"`
	Weight int     `json:"weight"`
}

type farEngine struct {
	InBudget     bool    `json:"in_budget"`
	HotWindows   []farID `json:"hot_windows"`
	LimitReached bool    `json:"limit_reached"`
}

type farQueue struct {
	Head    *farQueueEntry  `json:"head"`
	Demoted []farQueueEntry `json:"demoted"`
	Parked  []farQueueEntry `json:"parked"`
}

// farQueueEntry is any of a queue's entries: head, demoted or parked.
type farQueueEntry struct {
	Repo      farID   `json:"repo"`
	CrewID    farID   `json:"crew_id"`
	ID        farID   `json:"id"`
	WaitedS   int64   `json:"waited_s"`
	FirstDue  int64   `json:"first_due"`
	DemotedAt int64   `json:"demoted_at"`
	Reason    farEnum `json:"reason"`
}

func (e farQueueEntry) ok() bool { return e.CrewID.crew() && e.ID.hold() }

// farSkipped is a far skipped repo; its path, a far common dir, is never read.
type farSkipped struct {
	Status farText `json:"status"`
}

type farRepo struct {
	Repo      farID     `json:"repo"`
	Common    farKey    `json:"common"`
	Misrouted int       `json:"misrouted"`
	Crews     []farCrew `json:"crews"`
}

type farCrew struct {
	CrewID        farID                        `json:"crew_id"`
	Dispatcher    farDispatcher                `json:"dispatcher"`
	Attention     farEnum                      `json:"attention"`
	Workers       []map[string]json.RawMessage `json:"workers"`
	Holds         []farHold                    `json:"holds"`
	OverseerInbox []farInboxMsg                `json:"overseer_inbox"`
}

type farDispatcher struct {
	State      farEnum  `json:"state"`
	Lane       *farEnum `json:"lane"`
	LastEventS *int64   `json:"last_event_s"`
	Home       *farID   `json:"home"`
}

type farHold struct {
	ID      farID   `json:"id"`
	Kind    farEnum `json:"kind"`
	WaitedS int64   `json:"waited_s"`
	Due     bool    `json:"due"`
}

type farInboxMsg struct {
	TS   int64    `json:"ts"`
	Kind *farEnum `json:"kind"`
	Re   *int64   `json:"re"`
}

type farBatch struct {
	Repo   farID                        `json:"repo"`
	Common farKey                       `json:"common"`
	Cursor int64                        `json:"cursor"`
	Events []map[string]json.RawMessage `json:"events"`
}

type farMisrouted struct {
	Repo   farID  `json:"repo"`
	Common farKey `json:"common"`
	CrewID farID  `json:"crew_id"`
	From   farID  `json:"from"`
	TS     int64  `json:"ts"`
}

// project is the answer as host's, for an overseer named self. A repo whose
// label is no identifier keeps its crews, replies and misrouted msgs
// unlabeled, and one skipped note counts such repos, never naming them.
func (w farWire) project(host, self string, ts int64) FarAnswer {
	unlabeled := map[farKey]farID{}
	label := func(repo farID, common farKey) string {
		if repo.ident() {
			return string(repo)
		}
		unlabeled[common] = repo
		return ""
	}
	// A queue entry carries no common dir. Reuse a common dir already counted
	// whose raw far label equals the entry's, else the raw label text is its
	// own key: either way the label is counted once and never shown.
	labelQueue := func(repo farID) string {
		if repo.ident() {
			return string(repo)
		}
		for _, raw := range unlabeled {
			if raw == repo {
				return ""
			}
		}
		unlabeled[farKey(repo)] = repo
		return ""
	}
	ans := FarAnswer{V: 1, TS: ts, Status: w.Status.project(host, label, labelQueue), Inbox: []FarBatch{}, Misrouted: []FarMisrouted{}, Cursor: w.Cursor}
	if w.Self.host() {
		ans.Self = string(w.Self)
	}
	for _, b := range w.Inbox {
		var events []map[string]any
		for _, raw := range b.Events {
			if ev := projectEvent(raw, self); ev != nil {
				events = append(events, ev)
			}
		}
		if len(events) > 0 {
			ans.Inbox = append(ans.Inbox, FarBatch{Repo: label(b.Repo, b.Common), Common: string(b.Common), Cursor: b.Cursor, Events: events})
		}
	}
	// A misrouted msg may name no crew at all, or no sender.
	for _, m := range w.Misrouted {
		if m.CrewID != "" && !m.CrewID.crew() || !m.From.optional() {
			continue
		}
		ans.Misrouted = append(ans.Misrouted, FarMisrouted{Repo: label(m.Repo, m.Common), Common: string(m.Common), CrewID: string(m.CrewID), From: string(m.From), TS: m.TS})
	}
	if n := len(unlabeled); n > 0 {
		ans.Status.Skipped = append(ans.Status.Skipped, Skipped{Status: fmt.Sprintf("%d repo(s) with an invalid label shown unlabeled", n)})
	}
	return ans
}

// project is the far status as host's, each repo labelled by label. A crew
// that does not hold, or a worker row, is moved to Skipped by a reason this
// host words. A far common dir is only the repo's Key: no far path is shown.
func (s farStatus) project(host string, label func(farID, farKey) string, labelQueue func(farID) string) StatusDoc {
	doc := StatusDoc{TS: s.TS, Repos: []RepoStatus{}, Skipped: []Skipped{}, Hosts: []HostStatus{}}
	if s.Host.host() {
		doc.Host = string(s.Host)
	}
	if s.Machine != nil {
		doc.Machine = s.Machine.project()
	}
	for _, k := range s.Skipped {
		if status := k.Status.text(); status != "" {
			doc.Skipped = append(doc.Skipped, Skipped{Status: status})
		}
	}
	for _, r := range s.Repos {
		repo := RepoStatus{Host: host, Repo: label(r.Repo, r.Common), Key: string(r.Common), Misrouted: r.Misrouted, Crews: []CrewStatus{}}
		for _, c := range r.Crews {
			crew, dropped, why := c.project()
			if why != "" {
				doc.Skipped = append(doc.Skipped, Skipped{Status: why})
				continue
			}
			if dropped > 0 {
				doc.Skipped = append(doc.Skipped, Skipped{Status: fmt.Sprintf("crew %s: %d worker rows dropped", crew.CrewID, dropped)})
			}
			repo.Crews = append(repo.Crews, crew)
		}
		doc.Repos = append(doc.Repos, repo)
	}
	// Project the queue after the repos: a queue entry, which has no common
	// dir, can then reuse a common dir a repo row already contributed.
	doc.Queue = s.Queue.project(labelQueue)
	return doc
}

func (l farLoad) project() *LoadDoc {
	d := &LoadDoc{Budget: l.Budget, Weight: l.Weight, Pending: l.Pending, Free: l.Free, Leads: []Lead{}, Load1: l.Load1,
		Cores: l.Cores, LoadOver: l.LoadOver, Engines: map[string]Engine{}, BudgetAgeS: l.BudgetAgeS}
	for _, ld := range l.Leads {
		if !ld.Repo.ident() || !ld.Branch.ident() || !ld.Tier.in(tiers) {
			continue
		}
		lead := Lead{Repo: string(ld.Repo), Branch: string(ld.Branch), Tier: string(ld.Tier), Weight: ld.Weight}
		if ld.Name.ident() {
			lead.Name = string(ld.Name)
		}
		d.Leads = append(d.Leads, lead)
	}
	for name, e := range l.Engines {
		if !name.ident() {
			continue
		}
		eng := Engine{InBudget: e.InBudget, HotWindows: []string{}, LimitReached: e.LimitReached}
		for _, w := range e.HotWindows {
			if w.ident() {
				eng.HotWindows = append(eng.HotWindows, string(w))
			}
		}
		d.Engines[string(name)] = eng
	}
	return d
}

func (q farQueue) project(label func(farID) string) QueueStatus {
	out := QueueStatus{Demoted: []QueueDemote{}, Parked: []QueueParked{}}
	if h := q.Head; h != nil && h.ok() {
		out.Head = &QueueHead{Repo: label(h.Repo), CrewID: string(h.CrewID), ID: string(h.ID), WaitedS: h.WaitedS}
	}
	for _, d := range q.Demoted {
		if d.ok() {
			out.Demoted = append(out.Demoted, QueueDemote{Repo: label(d.Repo), CrewID: string(d.CrewID), ID: string(d.ID), FirstDue: d.FirstDue, DemotedAt: d.DemotedAt})
		}
	}
	for _, p := range q.Parked {
		if p.ok() && p.Reason.in(parkedReasons) {
			out.Parked = append(out.Parked, QueueParked{Repo: label(p.Repo), CrewID: string(p.CrewID), ID: string(p.ID), Reason: string(p.Reason)})
		}
	}
	return out
}

// project is the crew and how many of its worker rows did not hold, or why no
// fleet status could have built it. Only a valid crew id is ever named.
func (c farCrew) project() (CrewStatus, int, string) {
	if !c.CrewID.crew() {
		return CrewStatus{}, 0, "invalid crew id"
	}
	id := string(c.CrewID)
	d := c.Dispatcher
	switch {
	case !d.State.in(dispatcherStates):
		return CrewStatus{}, 0, "crew " + id + ": invalid dispatcher state"
	case d.Lane != nil && !d.Lane.in(laneStates):
		return CrewStatus{}, 0, "crew " + id + ": invalid lane state"
	case !c.Attention.in(attentions):
		return CrewStatus{}, 0, "crew " + id + ": invalid attention"
	case d.Home != nil && !d.Home.host():
		return CrewStatus{}, 0, "crew " + id + ": invalid home"
	}
	out := CrewStatus{CrewID: id, Attention: string(c.Attention), Workers: []map[string]any{}, Holds: []HoldStatus{}, OverseerInbox: []InboxMsg{},
		Dispatcher: Dispatcher{State: string(d.State), Lane: (*string)(d.Lane), LastEventS: d.LastEventS, Home: (*string)(d.Home)}}
	dropped := 0
	for _, raw := range c.Workers {
		if w := projectWorker(raw); w != nil {
			out.Workers = append(out.Workers, w)
		} else {
			dropped++
		}
	}
	for _, h := range c.Holds {
		if h.ID.hold() && h.Kind.in(holdKinds) {
			out.Holds = append(out.Holds, HoldStatus{ID: string(h.ID), Kind: string(h.Kind), WaitedS: h.WaitedS, Due: h.Due})
		}
	}
	for _, m := range c.OverseerInbox {
		msg := InboxMsg{TS: m.TS, Re: m.Re}
		if m.Kind != nil && m.Kind.in(replyKinds) {
			msg.Kind = (*string)(m.Kind)
		}
		out.OverseerInbox = append(out.OverseerInbox, msg)
	}
	return out, dropped, ""
}

// rowField decodes row[key] into v: present is false for a missing or null
// key, ok false for a value v cannot hold.
func rowField(row map[string]json.RawMessage, key string, v any) (present, ok bool) {
	raw, has := row[key]
	if !has || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, true
	}
	return true, json.Unmarshal(raw, v) == nil
}

// farBlocked is a far row's blocked wait, as status's derive builds it.
type farBlocked struct {
	Since       json.Number  `json:"since"`
	Cycle       *json.Number `json:"cycle"`
	BudgetLeftS *json.Number `json:"budget_left_s"`
	Source      farEnum      `json:"source"`
	Answered    bool         `json:"answered"`
}

// projectWorker is a far roster row cut to the keys a lane line or the
// overseer protocol reads, nil when its branch, sender, host, state, source,
// a number, a flag or its blocked wait does not hold. An invalid name or PR
// URL drops only that key, and the only detail kept is the one a lane
// compares.
func projectWorker(raw map[string]json.RawMessage) map[string]any {
	var branch farID
	if present, ok := rowField(raw, "branch", &branch); !present || !ok || !branch.ident() {
		return nil
	}
	row := map[string]any{"branch": string(branch)}
	for _, f := range []struct {
		key   string
		valid func(farID) bool
	}{{"from", farID.ident}, {"host", farID.host}} {
		var v farID
		present, ok := rowField(raw, f.key, &v)
		if !ok || present && !f.valid(v) {
			return nil
		}
		if present {
			row[f.key] = string(v)
		}
	}
	for _, f := range []struct {
		key string
		set []string
	}{{"state", workerStates}, {"source", rowSources}} {
		var v farEnum
		present, ok := rowField(raw, f.key, &v)
		if !ok || present && !v.in(f.set) {
			return nil
		}
		if present {
			row[f.key] = string(v)
		}
	}
	for _, key := range []string{"ts", "age_s"} {
		var n json.Number
		present, ok := rowField(raw, key, &n)
		if !ok {
			return nil
		}
		if present {
			row[key] = n
		}
	}
	for _, key := range []string{"live", "stale", "finished_window", "remote_unprobed", "pr_candidate"} {
		var b bool
		present, ok := rowField(raw, key, &b)
		if !ok {
			return nil
		}
		if present {
			row[key] = b
		}
	}
	var b farBlocked
	present, ok := rowField(raw, "blocked", &b)
	if !ok || present && (b.Since == "" || !b.Source.in(blockedSources)) {
		return nil
	}
	if present {
		row["blocked"] = map[string]any{"since": b.Since, "cycle": numberOrNil(b.Cycle), "budget_left_s": numberOrNil(b.BudgetLeftS),
			"source": string(b.Source), "answered": b.Answered}
	}
	for _, key := range []string{"name", "pr_url"} {
		var v farID
		if present, ok := rowField(raw, key, &v); present && ok && v.ident() {
			row[key] = string(v)
		}
	}
	var detail farEnum
	if present, ok := rowField(raw, "detail", &detail); present && ok && detail.in(details) {
		row["detail"] = string(detail)
	}
	return row
}

func numberOrNil(n *json.Number) any {
	if n == nil {
		return nil
	}
	return *n
}

// projectEvent is a reply the recheck kept, cut to the msg's own fields; nil
// when it is not a msg to overseer:<self> from the dispatcher of the crew it
// names with a string body.
func projectEvent(raw map[string]json.RawMessage, self string) map[string]any {
	var ts json.Number
	var crew, from, to farID
	var kind farEnum
	var body farText
	for key, v := range map[string]any{"ts": &ts, "crew_id": &crew, "kind": &kind, "from": &from, "to": &to, "body": &body} {
		if present, ok := rowField(raw, key, v); !present || !ok {
			return nil
		}
	}
	if _, whole := watch.WholeMS(ts); !whole || !crew.crew() || kind != "msg" || from != "dispatcher:"+crew || to != farID("overseer:"+self) {
		return nil
	}
	return map[string]any{"ts": ts, "crew_id": string(crew), "kind": string(kind), "from": string(from), "to": string(to), "body": body.text()}
}
