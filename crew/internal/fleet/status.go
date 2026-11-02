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
	// inboxWindow is how far back overseer_inbox reaches with no cursor.
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
}

// StatusRequest is `crew fleet status`'s flags.
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
	Queue   Queue        `json:"queue"`
	Repos   []RepoStatus `json:"repos"`
	Skipped []Skipped    `json:"skipped"`
}

// Queue is the fleet hold queue; empty until fleet holds exist.
type Queue struct {
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
	// Misrouted counts msgs to the overseer from anyone but a dispatcher of the
	// crew they name; they are never shown.
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

// Dispatcher is the crew's session: State is live, dead or closed; Lane is
// null when the dispatcher never wrote a stream tick.
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
	stateDir := o.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		stateDir = o.Getenv("HOME") + "/.local/state"
	}

	doc := StatusDoc{
		TS:      nowMS,
		Host:    host,
		Queue:   Queue{Demoted: []QueueDemote{}, Parked: []QueueParked{}},
		Repos:   []RepoStatus{},
		Skipped: []Skipped{},
	}
	lo := LoadOptions{Getenv: o.Getenv, Tmux: once, Git: o.Git, Clock: o.Clock, BudgetFile: o.BudgetFile, Budget: o.Budget, Cmd: o.Cmd}
	if budget, err := resolveBudget(ctx, nil, lo); err != nil {
		say(stderr, "crew: fleet status: machine: %s\n", oneLine(err))
	} else {
		machine, _ := collectLoad(ctx, budget, lo)
		doc.Machine = &machine
	}

	var candidates []map[string]any
	for _, r := range repos.Discover(repos.Options{Registry: o.Registry, Getenv: o.Getenv, Stderr: stderr, Prefix: "crew: fleet status"}) {
		if r.Status != repos.StatusOK {
			doc.Skipped = append(doc.Skipped, Skipped{Path: r.Common, Status: string(r.Status)})
			continue
		}
		events, torn, skip := readBus(r.Common)
		if skip != "" {
			doc.Skipped = append(doc.Skipped, Skipped{Path: r.Common, Status: skip})
			continue
		}
		if torn != "" {
			say(stderr, "crew: fleet status: %s: %s (rows from the readable prefix)\n", r.Common, torn)
		}
		b := repoBuild{ctx: ctx, o: o, once: once, common: r.Common, events: events, docs: eventDocs(events),
			idx: indexBus(events, host), now: now, nowMS: nowMS, nowSec: nowSec, overseer: overseer, stateDir: stateDir}
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

	candidates []map[string]any
}

func (b *repoBuild) build() (RepoStatus, error) {
	ctx, o := b.ctx, b.o
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
	origin, _ := git.Origin(ctx, o.Git, b.common)
	repo := RepoStatus{Repo: repos.Name(b.common, origin), Common: b.common, Crews: []CrewStatus{}}

	cutoff := b.inboxCutoff()
	repo.Misrouted = b.idx.misrouted(cutoff)
	for _, id := range b.crewIDs(byCrew) {
		repo.Crews = append(repo.Crews, b.crew(id, byCrew[id], live, cutoff))
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
		if e.IsDir() && bus.ValidCrewID(e.Name()) {
			ids[e.Name()] = true
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (b *repoBuild) crew(id string, rows []map[string]any, live roster.Liveness, cutoff int64) CrewStatus {
	c := CrewStatus{CrewID: id, Workers: []map[string]any{}, Holds: b.holds(id), OverseerInbox: b.idx.inbox(id, cutoff)}
	c.Dispatcher = b.dispatcher(id)
	for _, row := range rows {
		row = jsonval.Clamp(row).(map[string]any)
		stale, _ := staleOf(row, live)
		row["stale"] = stale
		b.derive(id, row)
		c.Workers = append(c.Workers, row)
	}
	c.Attention = attention(c)
	return c
}

// derive adds the status-only fields to a worker row.
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
		b.candidates = append(b.candidates, row)
	}
}

// dispatcher reads the crew's registration: pid file, pane file and the lane
// tick, with the state each says about the session.
func (b *repoBuild) dispatcher(id string) Dispatcher {
	dir := filepath.Join(b.common, "crew", "crews", id)
	pidfile := filepath.Join(dir, "pid")
	text, _ := crews.PidFileText(pidfile)
	d := Dispatcher{State: "closed"}
	switch alive := crews.PidAlive(b.o.Proc, b.now, pidfile, text); {
	case alive == nil:
	case *alive:
		d.State = "live"
	default:
		d.State = "dead"
	}
	if n, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && n > 0 && d.State != "closed" {
		d.PID = &n
	}
	if pane, _ := crews.PidFileText(filepath.Join(dir, "pane")); pane != "" {
		d.Pane = &pane
	}
	if _, err := os.Stat(filepath.Join(dir, "stream.tick")); err == nil {
		state, _, _, _ := lane.State(dir, b.o.Proc, b.nowMS)
		d.Lane = &state
	}
	if last, ok := b.idx.last[id]; ok {
		s := max((b.nowMS-last)/1000, 0)
		d.LastEventS = &s
	}
	return d
}

// holds are the crew's outstanding holds. A quota hold is due once the window
// it waits on has reset.
func (b *repoBuild) holds(crew string) []HoldStatus {
	out := []HoldStatus{}
	if !b.idx.holders[crew] {
		return out
	}
	held, err := hold.Outstanding(b.docs, crew)
	if err != nil {
		return out
	}
	list, _ := held.([]any)
	for _, h := range list {
		body, _ := h.(map[string]any)
		id, _ := body["id"].(string)
		kind := "quota"
		if body["kind"] == "fleet" {
			kind = "fleet"
		}
		hs := HoldStatus{ID: id, Kind: kind}
		if at, ok := b.idx.holdAt[crewKey{crew, id}]; ok {
			hs.WaitedS = max((b.nowMS-at)/1000, 0)
		}
		if wait, ok := body["wait"].(map[string]any); ok && kind == "quota" {
			if resets, ok := number(wait["resets_at"]); ok {
				hs.Due = resets <= b.nowSec
			}
		}
		out = append(out, hs)
	}
	return out
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

// number is a decoded JSON number as a float, whatever type the decoder left.
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
		if crew, ok := strings.CutPrefix(e.To, "hold:"); ok && crew == e.CrewID {
			idx.holders[crew] = true
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

// inbox is the crew's dispatcher msgs to the overseer after cutoff.
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

// statusTable is one line per crew.
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
