package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// StatusUsage is the line every `crew fleet status` grammar refusal prints.
const StatusUsage = "crew: fleet status [--json] [--gh]"

const (
	// blockedCycles and blockedCycleS are the worker's await budget: 24 cycles of
	// 300 s, each re-stamped `(cycle K of 24)`.
	blockedCycles = 24
	blockedCycleS = 300
	// inboxWindow is how far back overseer_inbox reaches with no cursor, and
	// misrouted always.
	inboxWindow = 24 * time.Hour
	// unprobed is the reason an overseer's remote capture fails with: it asks no
	// host, and the placeholder row it leaves is marked remote_unprobed.
	unprobed = "remote_unprobed"
)

var cycleRE = regexp.MustCompile(`\(cycle (\d+) of 24\)`)

// StatusOptions carry every outside read, so tests use temp dirs and stubs.
type StatusOptions struct {
	Options
	Proc proc.Table
	// Gh is the client --gh asks; nil leaves every candidate with a pr_error.
	Gh gh.Client
	// GhTimeout bounds each gh call; zero is 20 s.
	GhTimeout time.Duration
	// Host is this machine's short name, the `overseer:<host>` address.
	Host func() (string, error)
	// Budget, BudgetFile and Cmd are the machine document's reads (LoadOptions).
	Budget     func(ctx context.Context) (int, error)
	BudgetFile string
	Cmd        func(ctx context.Context, name string, args ...string) (string, error)
	// Settings resolves the fleet queue's settings, its budget the machine
	// document's too. nil: the defaults, with Budget's budget when it is set.
	Settings func(ctx context.Context) (QueueSettings, error)
}

func (o StatusOptions) queueSettings(ctx context.Context) (QueueSettings, error) {
	if o.Settings != nil {
		return o.Settings(ctx)
	}
	s := defaultQueueSettings
	if o.Budget != nil {
		b, err := o.Budget(ctx)
		if err != nil {
			return s, err
		}
		s.Budget = b
	}
	return s, nil
}

type StatusRequest struct {
	JSON bool
	GH   bool
}

// StatusDoc is the cold-start fleet view: the field names are the contract the
// overseer reads.
type StatusDoc struct {
	TS      int64        `json:"ts"`
	Host    string       `json:"host"`
	Machine *LoadDoc     `json:"machine"`
	Queue   QueueStatus  `json:"queue"`
	Repos   []RepoStatus `json:"repos"`
	Skipped []Skipped    `json:"skipped"`
}

// QueueStatus is the fleet hold queue; empty until fleet holds exist.
type QueueStatus struct {
	Head    *QueueHead    `json:"head"`
	Demoted []QueueDemote `json:"demoted"`
	Parked  []QueueParked `json:"parked"`
}

type QueueHead struct {
	Repo    string `json:"repo"`
	CrewID  string `json:"crew_id"`
	ID      string `json:"id"`
	WaitedS int64  `json:"waited_s"`
}

type QueueDemote struct {
	Repo      string `json:"repo"`
	CrewID    string `json:"crew_id"`
	ID        string `json:"id"`
	FirstDue  int64  `json:"first_due"`
	DemotedAt int64  `json:"demoted_at"`
}

type QueueParked struct {
	Repo   string `json:"repo"`
	CrewID string `json:"crew_id"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type Skipped struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type RepoStatus struct {
	Repo   string `json:"repo"`
	Common string `json:"common"`
	// Misrouted counts the last 24 h of msgs to the overseer from anyone but a
	// dispatcher of the crew they name; they are never shown.
	Misrouted int          `json:"misrouted"`
	Crews     []CrewStatus `json:"crews"`
}

type CrewStatus struct {
	CrewID     string     `json:"crew_id"`
	Dispatcher Dispatcher `json:"dispatcher"`
	Attention  string     `json:"attention"`
	// Workers are the roster rows of the crew, with blocked, finished_window,
	// remote_unprobed and pr_candidate added (pr or pr_error under --gh).
	Workers       []map[string]any `json:"workers"`
	Holds         []HoldStatus     `json:"holds"`
	OverseerInbox []InboxMsg       `json:"overseer_inbox"`
}

// Dispatcher is the crew's session: State is live, dead or closed; Lane is the
// freshest of the per-repo and fleet-stream lane ticks written since the
// dispatcher started, null when there is none (a park lane, or only an earlier
// session's tick).
type Dispatcher struct {
	State      string  `json:"state"`
	PID        *int    `json:"pid"`
	Pane       *string `json:"pane"`
	Lane       *string `json:"lane"`
	LastEventS *int64  `json:"last_event_s"`
}

type HoldStatus struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	WaitedS int64  `json:"waited_s"`
	Due     bool   `json:"due"`
}

type InboxMsg struct {
	TS   int64   `json:"ts"`
	Kind *string `json:"kind"`
	Re   *int64  `json:"re"`
}

// Status prints the cold-start fleet view: the JSON document, or one table
// line per crew. It exits 0 with a document even when repos were skipped.
func Status(ctx context.Context, req StatusRequest, stdout, stderr io.Writer, o StatusOptions) int {
	doc := CollectStatus(ctx, req, stderr, o)
	if !req.JSON {
		return write(stdout, stderr, statusTable(doc))
	}
	var buf bytes.Buffer
	if err := jsonval.Encode(&buf, doc, false); err != nil {
		say(stderr, "crew: fleet status: %v\n", err)
		return 1
	}
	return write(stdout, stderr, buf.Bytes())
}

// CollectStatus rebuilds the fleet's state from durable state on disk, from
// any directory, with no memory of earlier runs. A repo it cannot read lands
// in Skipped; with GitHub enrichment a failure only becomes a pr_error.
func CollectStatus(ctx context.Context, req StatusRequest, stderr io.Writer, o StatusOptions) StatusDoc {
	once := onceTmux{o.Tmux, map[string]string{}}
	o.Tmux = once
	if o.Proc == nil {
		o.Proc = proc.OS{}
	}
	now := o.Clock.Now()
	nowMS := now.UnixMilli()
	nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6

	host := ""
	if o.Host != nil {
		h, err := o.Host()
		if err != nil {
			say(stderr, "crew: fleet status: host name: %s\n", oneLine(err))
		}
		host = h
	}
	overseer := o.Getenv("CREW_OVERSEER") != ""
	stateDir := stateHome(o.Getenv)

	doc := StatusDoc{
		TS:      nowMS,
		Host:    host,
		Queue:   QueueStatus{Demoted: []QueueDemote{}, Parked: []QueueParked{}},
		Repos:   []RepoStatus{},
		Skipped: []Skipped{},
	}
	// One read-only queue evaluation, over a bus cache the repo pass reuses.
	buses := busCache{}
	var due *queued
	if s, err := o.queueSettings(ctx); err != nil {
		say(stderr, "crew: fleet status: machine: %s\n", oneLine(err))
	} else {
		lo := LoadOptions{Getenv: o.Getenv, Registry: o.Registry, Tmux: once, Git: o.Git, Clock: o.Clock, BudgetFile: o.BudgetFile, Cmd: o.Cmd}
		q := newQueue(ctx, QueueOptions{LoadOptions: lo, Proc: o.Proc}, scanFleet(o.Registry, o.Getenv, buses.read, nowMS), s, now)
		var rec map[holdKey]dueEntry
		rec, due = q.advance(readRecord(duePath(o.Getenv)))
		doc.Machine = &q.load
		doc.Queue = q.report(rec, repoNamer(ctx, o.Git))
	}

	var candidates []prCandidate
	for _, r := range repos.Discover(repos.Options{Registry: o.Registry, Getenv: o.Getenv, Stderr: stderr, Prefix: "crew: fleet status"}) {
		if r.Status != repos.StatusOK {
			doc.Skipped = append(doc.Skipped, Skipped{Path: r.Common, Status: string(r.Status)})
			continue
		}
		events, torn, skip := buses.read(r.Common)
		if skip != "" {
			doc.Skipped = append(doc.Skipped, Skipped{Path: r.Common, Status: skip})
			continue
		}
		if torn != "" {
			say(stderr, "crew: fleet status: %s: %s (rows from the readable prefix)\n", r.Common, torn)
		}
		b := repoBuild{ctx: ctx, o: o, once: once, common: r.Common, events: events, docs: eventDocs(events),
			idx: indexBus(events, host), now: now, nowMS: nowMS, nowSec: nowSec, overseer: overseer, stateDir: stateDir, due: due}
		repo, err := b.build()
		if err != nil {
			doc.Skipped = append(doc.Skipped, Skipped{Path: r.Common, Status: oneLine(err)})
			continue
		}
		candidates = append(candidates, b.candidates...)
		doc.Repos = append(doc.Repos, repo)
	}
	if req.GH {
		timeout := o.GhTimeout
		if timeout == 0 {
			timeout = ghTimeout
		}
		enrichPRs(ctx, o.Gh, candidates, timeout)
	}
	return doc
}

// repoBuild is one repo's pass: the reads it shares across its crews.
type repoBuild struct {
	ctx      context.Context
	o        StatusOptions
	once     onceTmux
	common   string
	events   []bus.Event
	docs     []any
	idx      busIndex
	now      time.Time
	nowMS    int64
	nowSec   float64
	overseer bool
	stateDir string
	due      *queued // the fleet queue's due hold, nil for none

	repoName   string
	ghRepo     string // the raw owner/name, the PR-ownership check's identity
	candidates []prCandidate
}

func (b *repoBuild) build() (RepoStatus, error) {
	ctx, o := b.ctx, b.o
	origin, _ := git.Origin(ctx, o.Git, b.common)
	b.ghRepo = repos.Name(b.common, origin)
	b.repoName = repos.Label(b.common, origin)
	worktrees := sync.OnceValues(func() (string, error) { return git.Worktrees(ctx, o.Git, "", b.common) })
	live := roster.Liveness{Windows: func() map[string]bool { return roster.Windows(ctx, b.once) }, Worktrees: worktrees}
	rows, err := foldRepo(ctx, b.common, b.events, b.docs, "", b.nowSec,
		roster.Probes{Tmux: b.once, Worktrees: worktrees, Live: true}, b.remote())
	if err != nil {
		return RepoStatus{}, err
	}
	byCrew := map[string][]map[string]any{}
	for _, r := range rows {
		byCrew[r.crew] = append(byCrew[r.crew], r.Row)
	}
	repo := RepoStatus{Repo: b.repoName, Common: b.common, Crews: []CrewStatus{}}

	// Not from the overseer's cursor: its lane moves that past every misrouted
	// msg within a poll, and counts them only in memory.
	repo.Misrouted = b.idx.misrouted(b.nowMS - inboxWindow.Milliseconds())
	cutoff := b.inboxCutoff()
	for _, id := range b.crewIDs(byCrew) {
		c, err := b.crew(id, byCrew[id], live, cutoff)
		if err != nil {
			return RepoStatus{}, err
		}
		repo.Crews = append(repo.Crews, c)
	}
	return repo, nil
}

// remote is the capture the fold merges remote workers through. An overseer
// asks no host: remoteRun sender-checks every argv as `dispatcher:<crew>`, an
// identity the overseer never takes, so its remote rows are the bus's
// placeholders.
func (b *repoBuild) remote() func(crew string) roster.Capture {
	switch {
	case b.overseer:
		return func(string) roster.Capture {
			return func(_ context.Context, _ string, _ []string, _, stderr io.Writer) int {
				say(stderr, "%s\n", unprobed)
				return 1
			}
		}
	case b.o.Remote != nil:
		dir := filepath.Dir(b.common)
		return func(crew string) roster.Capture { return b.o.Remote(dir, crew) }
	}
	return nil
}

// inboxCutoff is where overseer_inbox starts: after the overseer's cursor for
// this bus when it left one, else a day back. Reading it is the only place
// status touches overseer-written state.
func (b *repoBuild) inboxCutoff() int64 {
	text, _ := crews.PidFileText(lane.OverseerCursor(b.stateDir, b.common))
	if ts, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil && ts >= 0 {
		return ts
	}
	return b.nowMS - inboxWindow.Milliseconds()
}

// crewIDs are the crews with a worker row, a crew dir or a hold, in id order.
// A crew id becomes a path under crews/, so only a valid one counts, whatever
// wrote it.
func (b *repoBuild) crewIDs(byCrew map[string][]map[string]any) []string {
	ids := map[string]bool{}
	for id := range byCrew {
		ids[id] = true
	}
	for id := range b.idx.holders {
		ids[id] = true
	}
	entries, _ := os.ReadDir(filepath.Join(b.common, "crew", "crews"))
	for _, e := range entries {
		if e.IsDir() {
			ids[e.Name()] = true
		}
	}
	for id := range ids {
		if !bus.ValidCrewID(id) {
			delete(ids, id)
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (b *repoBuild) crew(id string, rows []map[string]any, live roster.Liveness, cutoff int64) (CrewStatus, error) {
	holds, err := b.holds(id)
	if err != nil {
		return CrewStatus{}, err
	}
	c := CrewStatus{CrewID: id, Workers: []map[string]any{}, Holds: holds, OverseerInbox: b.idx.inbox(id, cutoff)}
	c.Dispatcher = b.dispatcher(id)
	for _, row := range rows {
		row = jsonval.Clamp(row).(map[string]any)
		stale, _ := staleOf(row, live)
		row["stale"] = stale
		b.derive(id, row)
		c.Workers = append(c.Workers, row)
	}
	c.Attention = attention(c)
	return c, nil
}

func (b *repoBuild) derive(crew string, row map[string]any) {
	state := str(row, "state")
	row["blocked"] = nil
	if state == "blocked" {
		row["blocked"] = b.idx.blocked(crew, row)
	}
	// A finished worker still holding a window past the grace is what the reap
	// timer should already have released.
	age, _ := number(row["age_s"])
	row["finished_window"] = slices.Contains([]string{"done", "failed", "exited"}, state) &&
		row["live"] == true && age > reap.ReleaseGrace
	if _, unanswered := row["host_error"]; unanswered && row["host_error"] == unprobed {
		delete(row, "host_error")
		row["remote_unprobed"] = true
	} else {
		row["remote_unprobed"] = false
	}
	url, _ := row["pr_url"].(string)
	candidate := (state == "pr_open" || state == "done") && url != ""
	row["pr_candidate"] = candidate
	if candidate {
		b.candidates = append(b.candidates, prCandidate{row: row, repo: b.ghRepo})
	}
}

// session is a crew's dispatcher as its registration says: the pid file's
// state (live, dead or closed) and pid, and the lane tick's state, nil when
// no tick from this dispatcher's session exists.
type session struct {
	state string
	pid   *int
	lane  *string
}

// fleetLane is the lane dir `crew fleet stream --crew` keeps for crew.
func fleetLane(getenv func(string) string, crew string) string {
	return filepath.Join(stateHome(getenv), "crew", "fleet-stream", crew)
}

// readSession reads the crew's dispatcher at dir. Its lane is the freshest tick
// of the per-repo lane in dir and the fleet-stream lane in fleet; a tick
// written before the dispatcher process started is an earlier session's and
// counts as none, so a leftover never reads as a dead lane.
func readSession(dir, fleet string, t proc.Table, now time.Time) session {
	pidfile := filepath.Join(dir, "pid")
	text, _ := crews.PidFileText(pidfile)
	s := session{state: "closed"}
	switch alive := crews.PidAlive(t, now, pidfile, text); {
	case alive == nil:
	case *alive:
		s.state = "live"
	default:
		s.state = "dead"
	}
	if n, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && n > 0 && s.state != "closed" {
		s.pid = &n
	}
	nowMS := now.UnixMilli()
	// ps counts whole seconds, so a tick may land that far before the start it
	// reports; RecordedLive allows the same slack.
	var startMS int64
	if s.pid != nil && s.state == "live" {
		if elapsed, ok := t.Elapsed(*s.pid); ok {
			startMS = nowMS - elapsed*1000 - 2000
		}
	}
	var newest int64
	for _, cdir := range []string{dir, fleet} {
		if _, err := os.Stat(filepath.Join(cdir, "stream.tick")); err != nil {
			continue
		}
		tick := lane.ReadTick(filepath.Join(cdir, "stream.tick"))
		if tick.OK && tick.TS < startMS || s.lane != nil && tick.TS <= newest {
			continue
		}
		state, _, _, _ := lane.State(cdir, t, nowMS)
		s.lane, newest = &state, tick.TS
	}
	return s
}

// eligible is whether the crew's fleet holds can bind the queue: a live
// dispatcher whose lane, if it ever ticked, is neither dead nor stale. A park
// lane writes no tick, so it counts.
func (s session) eligible() bool {
	return s.state == "live" && (s.lane == nil || *s.lane != "dead" && *s.lane != "stale")
}

// dispatcher reads the crew's registration: pid file, pane file and the lane
// tick, with the state each says about the session.
func (b *repoBuild) dispatcher(id string) Dispatcher {
	dir := filepath.Join(b.common, "crew", "crews", id)
	s := readSession(dir, fleetLane(b.o.Getenv, id), b.o.Proc, b.now)
	d := Dispatcher{State: s.state, PID: s.pid, Lane: s.lane}
	if pane, _ := crews.PidFileText(filepath.Join(dir, "pane")); pane != "" {
		d.Pane = &pane
	}
	if last, ok := b.idx.last[id]; ok {
		s := max((b.nowMS-last)/1000, 0)
		d.LastEventS = &s
	}
	return d
}

// holds are the crew's outstanding holds. A quota hold is due once the window
// it waits on has reset, a fleet hold when it is the fleet queue's due hold. A
// bus the fold cannot read fails the repo rather than reading as no holds: a
// crew parked on quota would look idle.
func (b *repoBuild) holds(crew string) ([]HoldStatus, error) {
	out := []HoldStatus{}
	if !b.idx.holders[crew] {
		return out, nil
	}
	held, err := hold.Outstanding(b.idx.holdDocs, crew)
	if err != nil {
		return nil, err
	}
	list, _ := held.([]any)
	for _, h := range list {
		body, _ := h.(map[string]any)
		id, _ := body["id"].(string)
		wait, _ := body["wait"].(map[string]any)
		hs := HoldStatus{ID: id, Kind: "quota"}
		if at, ok := b.idx.holdAt[crewKey{crew, id}]; ok {
			hs.WaitedS = max((b.nowMS-at)/1000, 0)
		}
		if wait["kind"] == "fleet" {
			hs.Kind = "fleet"
			hs.Due = b.due != nil && b.due.id == id && b.due.crew == crew && filepath.Clean(b.due.common) == filepath.Clean(b.common)
		} else if resets, ok := number(wait["resets_at"]); ok {
			hs.Due = resets <= b.nowSec
		}
		out = append(out, hs)
	}
	return out, nil
}

// attention says whether a crew with no live dispatcher needs one: yes while it
// has a worker that is live by roster's rules, or a hold. Stale rows are cleanup,
// never a need.
func attention(c CrewStatus) string {
	if c.Dispatcher.State == "live" {
		return "attended"
	}
	if len(c.Holds) > 0 {
		return "unattended"
	}
	for _, w := range c.Workers {
		if slices.Contains(roster.LiveStates, str(w, "state")) && w["stale"] == false {
			return "unattended"
		}
	}
	return "idle"
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// crewKey keys a per-crew index by a second id: a worker, a recipient, a hold.
type crewKey struct{ crew, id string }

type statusEv struct {
	ts     int64
	state  string
	detail string
}

type overseerMsg struct {
	ts   int64
	crew string
	from string
	body map[string]any
}

// busIndex is one pass over a bus, for the derivations that would otherwise
// each rescan it per worker or crew.
type busIndex struct {
	status   map[crewKey][]statusEv // {crew, worker from}
	replies  map[crewKey][]int64    // {crew, to}: msgs from dispatcher:<crew>
	last     map[string]int64       // crew → newest event
	holdAt   map[crewKey]int64      // {crew, hold id} → ts of its add
	holders  map[string]bool        // crews with a hold row
	holdDocs []any                  // the hold rows, the fold's input
	overseer []overseerMsg
}

func indexBus(events []bus.Event, host string) busIndex {
	idx := busIndex{
		status:  map[crewKey][]statusEv{},
		replies: map[crewKey][]int64{},
		last:    map[string]int64{},
		holdAt:  map[crewKey]int64{},
		holders: map[string]bool{},
	}
	for _, e := range events {
		doc, _ := e.Doc.(map[string]any)
		ts := tsOf(doc)
		if e.CrewID != "" && ts > idx.last[e.CrewID] {
			idx.last[e.CrewID] = ts
		}
		if e.Kind == bus.KindStatus && e.CrewID != "" && strings.HasPrefix(e.From, "worker:") {
			body, _ := doc["body"].(map[string]any)
			state, _ := body["state"].(string)
			detail, _ := body["detail"].(string)
			k := crewKey{e.CrewID, e.From}
			idx.status[k] = append(idx.status[k], statusEv{ts, state, detail})
		}
		if e.Kind != bus.KindMsg {
			continue
		}
		if e.From == "dispatcher:"+e.CrewID && e.CrewID != "" {
			k := crewKey{e.CrewID, e.To}
			idx.replies[k] = append(idx.replies[k], ts)
		}
		// Any sender may post to hold:<crew>; only the dispatcher's rows are holds.
		if crew, ok := strings.CutPrefix(e.To, "hold:"); ok && crew == e.CrewID && e.From == "dispatcher:"+crew {
			idx.holders[crew] = true
			idx.holdDocs = append(idx.holdDocs, e.Doc)
			if id, _ := bodyObject(doc["body"])["id"].(string); id != "" {
				if _, seen := idx.holdAt[crewKey{crew, id}]; !seen {
					idx.holdAt[crewKey{crew, id}] = ts
				}
			}
		}
		if host != "" && e.To == "overseer:"+host {
			idx.overseer = append(idx.overseer, overseerMsg{ts: ts, crew: e.CrewID, from: e.From, body: bodyObject(doc["body"])})
		}
	}
	for _, evs := range idx.status {
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].ts < evs[j].ts })
	}
	return idx
}

// blocked is the wait of a blocked worker. since is the first row of its
// current unbroken run of blocked rows: restamps refresh the roster's age_s,
// which therefore understates it.
func (idx busIndex) blocked(crew string, row map[string]any) map[string]any {
	from, _ := row["from"].(string)
	evs := idx.status[crewKey{crew, from}]
	start := len(evs)
	for start > 0 && evs[start-1].state == "blocked" {
		start--
	}
	since, _ := number(row["ts"])
	var detail string
	if start < len(evs) {
		since = float64(evs[start].ts)
		detail = evs[len(evs)-1].detail
	}
	b := map[string]any{"since": int64(since), "cycle": nil, "budget_left_s": nil, "source": "worker", "answered": false}
	if row["source"] == "watchdog" {
		b["source"] = "watchdog"
	}
	if m := cycleRE.FindStringSubmatch(detail); m != nil {
		if k, err := strconv.Atoi(m[1]); err == nil {
			b["cycle"] = k
			b["budget_left_s"] = max(blockedCycles-k, 0) * blockedCycleS
		}
	}
	for _, ts := range idx.replies[crewKey{crew, from}] {
		if ts > int64(since) {
			b["answered"] = true
		}
	}
	return b
}

func (idx busIndex) inbox(crew string, cutoff int64) []InboxMsg {
	out := []InboxMsg{}
	for _, m := range idx.overseer {
		if m.ts <= cutoff || m.crew != crew || m.from != "dispatcher:"+crew {
			continue
		}
		msg := InboxMsg{TS: m.ts}
		if kind, ok := m.body["kind"].(string); ok {
			msg.Kind = &kind
		}
		if re, ok := number(m.body["re"]); ok {
			n := int64(re)
			msg.Re = &n
		}
		out = append(out, msg)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

// misrouted counts the msgs to the overseer after cutoff from any sender but
// a dispatcher of the crew they name.
func (idx busIndex) misrouted(cutoff int64) int {
	n := 0
	for _, m := range idx.overseer {
		if m.ts > cutoff && (m.crew == "" || m.from != "dispatcher:"+m.crew) {
			n++
		}
	}
	return n
}

func tsOf(doc map[string]any) int64 {
	f, _ := number(doc["ts"])
	return int64(f)
}

// bodyObject is a msg body as an object: the object itself, or the one a
// string body holds.
func bodyObject(v any) map[string]any {
	switch b := v.(type) {
	case map[string]any:
		return b
	case string:
		var obj map[string]any
		if json.Unmarshal([]byte(b), &obj) == nil {
			return obj
		}
	}
	return nil
}

func statusTable(doc StatusDoc) []byte {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	say(tw, "REPO\tCREW\tDISPATCHER\tLANE\tLIVE\tBLOCKED(oldest)\tPR_OPEN\tHOLDS\n")
	for _, r := range doc.Repos {
		for _, c := range r.Crews {
			lane := "-"
			if c.Dispatcher.Lane != nil {
				lane = *c.Dispatcher.Lane
			}
			liveN, prN := 0, 0
			var oldest int64
			for _, w := range c.Workers {
				if slices.Contains(roster.LiveStates, str(w, "state")) && w["stale"] == false {
					liveN++
				}
				if prOpen(w) {
					prN++
				}
				if b, ok := w["blocked"].(map[string]any); ok {
					if since, _ := b["since"].(int64); oldest == 0 || since < oldest {
						oldest = since
					}
				}
			}
			blocked := "-"
			if oldest > 0 {
				blocked = fmtAge(max((doc.TS-oldest)/1000, 0))
			}
			say(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%d\t%d\n",
				clean(r.Repo), clean(c.CrewID), c.Dispatcher.State, lane, liveN, blocked, prN, len(c.Holds))
		}
	}
	_ = tw.Flush()
	return buf.Bytes()
}
