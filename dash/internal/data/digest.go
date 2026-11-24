package data

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	activityWindowMS = 3600000
	recentCap        = 50
	maxCrewRings     = 64
	subagentCap      = 16
	labelMax         = 80
	fieldMax         = 64
)

var (
	phaseNames    = []string{"spec", "plan", "execute", "gate", "review", "ship"}
	terminalState = []string{"done", "failed", "exited", "reaped", "migrated"}
)

type WorkerKey struct{ Crew, Branch string }

type RoleKey struct{ Crew, Branch, Role string }

type StageMark struct {
	Name string
	TS   int64
}

type WorkerDigest struct {
	Stages   []StageMark
	Ledger   string
	LedgerTS int64
	Activity []int64
	// Session is the session suffix of the worker's latest non-watchdog status row.
	Session string
}

// SubagentRun is one execute-phase subagent run reported by its lead.
// State is running, done or failed.
type SubagentRun struct {
	ID, AgentID, Session, Engine, Label, Agent, Model, Effort, State string
	StartTS, EndTS                                                   int64
}

type RoleDigest struct {
	State, Detail string
	TS            int64
	Verdict       string
	VerdictTS     int64
}

type PRWatch struct {
	PR                            int
	URL, State, Checks, Mergeable string
	TS                            int64
}

type RecentEvent struct {
	TS                        int64
	From, Kind, State, Detail string
	// Crew is the row's crew: its crew_id, else the dispatcher sender's, else
	// the last crew seen on the same branch; "" when none can be inferred.
	Crew string
	// Ev is the decoded bus row, read-only; nil for rows built without one.
	Ev Event
}

// BusDigest is the bus log folded into what the Overview needs: per-worker
// stage history, ledger and activity, per-role status and verdict, per-PR
// pr-watch state and the last events overall.
type BusDigest struct {
	Workers map[WorkerKey]*WorkerDigest
	Roles   map[RoleKey]*RoleDigest
	PRs     map[int]*PRWatch
	// Subagents holds each worker's runs in spawn order.
	Subagents map[WorkerKey][]SubagentRun
	Recent    []RecentEvent
	// RecentByCrew holds each attributed crew's last recentCap rows, so a busy
	// crew cannot push a quiet one out of its ring.
	RecentByCrew map[string][]RecentEvent
	// Repo is the repo name on the first dispatch row seen, "" until one lands.
	Repo string
	Err  *string
}

// BusScanner folds events.jsonl incrementally: it keeps a byte offset and only
// consumes complete lines, so a torn last line is retried on the next Scan.
type BusScanner struct {
	path      string
	mu        sync.Mutex
	off       int64
	d         BusDigest
	lastState map[WorkerKey]string
	// branchCrew is each branch's last-known crew, for rows (reap) that carry
	// a branch but no crew_id.
	branchCrew map[string]string
}

func NewBusScanner(path string) *BusScanner {
	s := &BusScanner{path: path}
	s.reset()
	return s
}

func (s *BusScanner) reset() {
	s.off = 0
	s.d = BusDigest{
		Workers:      map[WorkerKey]*WorkerDigest{},
		Roles:        map[RoleKey]*RoleDigest{},
		PRs:          map[int]*PRWatch{},
		Subagents:    map[WorkerKey][]SubagentRun{},
		Recent:       []RecentEvent{},
		RecentByCrew: map[string][]RecentEvent{},
	}
	s.lastState = map[WorkerKey]string{}
	s.branchCrew = map[string]string{}
}

// Scan reads any lines appended since the last call and returns a deep copy of
// the digest, with activity older than an hour before nowMS dropped.
func (s *BusScanner) Scan(nowMS int64) (BusDigest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path)
	if err != nil {
		return BusDigest{}, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return BusDigest{}, err
	}
	if st.Size() < s.off {
		s.reset()
	}
	if _, err := f.Seek(s.off, io.SeekStart); err != nil {
		return BusDigest{}, err
	}

	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // no trailing newline: the line may still be growing
		}
		if err != nil {
			return BusDigest{}, err
		}
		s.off += int64(len(line))
		if ev, ok := decodeLine(line); ok {
			s.apply(ev)
		}
	}

	s.prune(nowMS - activityWindowMS)
	return s.d.clone(), nil
}

func (s *BusScanner) prune(cutoff int64) {
	for _, w := range s.d.Workers {
		w.Activity = slices.DeleteFunc(w.Activity, func(ts int64) bool { return ts < cutoff })
	}
}

func (s *BusScanner) apply(ev Event) {
	ts := int64Field(ev["ts"])
	crew, _ := ev["crew_id"].(string)
	from, _ := ev["from"].(string)
	kind, _ := ev["kind"].(string)
	body := BodyObject(ev["body"])

	if repo, _ := ev["repo"].(string); kind == "dispatch" && repo != "" && s.d.Repo == "" {
		s.d.Repo = RepoName(repo)
	}
	branch, _ := ev["branch"].(string)
	if b, ok := strings.CutPrefix(from, "worker:"); ok {
		branch, _, _ = strings.Cut(b, "#")
	}
	evCrew := crew
	if evCrew == "" {
		if id, ok := strings.CutPrefix(from, "dispatcher:"); ok {
			evCrew = id
		} else {
			evCrew = s.branchCrew[branch]
		}
	}
	if evCrew != "" && branch != "" {
		s.branchCrew[branch] = evCrew
	}

	state, _ := body["state"].(string)
	detail, _ := body["detail"].(string)
	to, _ := ev["to"].(string)
	sink := kind == "msg" && crew != "" && to == "subagent:"+crew && strings.HasPrefix(from, "worker:")
	if !sink {
		summary := detail
		if str, ok := ev["body"].(string); ok && kind != "status" {
			summary = str
		}
		re := RecentEvent{TS: ts, From: from, Kind: kind, State: state, Detail: summary, Crew: evCrew, Ev: ev}
		s.d.Recent = capRecent(append(s.d.Recent, re))
		if evCrew != "" {
			s.addCrewRecent(re)
		}
	}

	switch {
	case strings.HasPrefix(from, "worker:"):
		branch, session, _ := strings.Cut(strings.TrimPrefix(from, "worker:"), "#")
		key := WorkerKey{crew, branch}
		s.applyWorker(key, session, ts, kind, state, detail, body["source"] == "watchdog")
		if sink {
			s.applySubagent(key, session, ts, body)
		}
	case strings.HasPrefix(from, "role:"):
		rest := strings.TrimPrefix(from, "role:")
		i := strings.LastIndex(rest, ":")
		if i < 0 {
			return
		}
		key := RoleKey{crew, rest[:i], rest[i+1:]}
		role := s.d.Roles[key]
		if role == nil {
			role = &RoleDigest{}
			s.d.Roles[key] = role
		}
		switch kind {
		case "status":
			role.State, role.Detail, role.TS = state, detail, ts
		case "msg":
			if v, _ := body["verdict"].(string); v != "" {
				role.Verdict, role.VerdictTS = v, ts
			}
		}
	case strings.HasPrefix(from, "pr-watch:") && kind == "msg":
		pr := int(int64Field(body["pr"]))
		if pr == 0 {
			pr, _ = strconv.Atoi(strings.TrimPrefix(from, "pr-watch:"))
		}
		if pr == 0 {
			return
		}
		ps, _ := body["state"].(map[string]any)
		url, _ := body["url"].(string)
		st, _ := ps["state"].(string)
		checks, _ := ps["checks"].(string)
		mergeable, _ := ps["mergeable"].(string)
		s.d.PRs[pr] = &PRWatch{PR: pr, URL: url, State: st, Checks: checks, Mergeable: mergeable, TS: ts}
	}
}

func capRecent(rs []RecentEvent) []RecentEvent {
	if len(rs) > recentCap {
		return slices.Clone(rs[len(rs)-recentCap:])
	}
	return rs
}

func (s *BusScanner) addCrewRecent(re RecentEvent) {
	rings := s.d.RecentByCrew
	if _, ok := rings[re.Crew]; !ok && len(rings) >= maxCrewRings {
		stalest, oldest := "", int64(math.MaxInt64)
		for id, r := range rings {
			if ts := r[len(r)-1].TS; ts < oldest {
				stalest, oldest = id, ts
			}
		}
		delete(rings, stalest)
	}
	rings[re.Crew] = capRecent(append(rings[re.Crew], re))
}

func (s *BusScanner) applyWorker(key WorkerKey, session string, ts int64, kind, state, detail string, watchdog bool) {
	if watchdog {
		return
	}
	w := s.d.Workers[key]
	if w == nil {
		w = &WorkerDigest{}
		s.d.Workers[key] = w
	}
	w.Activity = append(w.Activity, ts)
	if kind != "status" {
		return
	}
	w.Session = session

	if state == "pr_open" {
		w.Ledger, w.LedgerTS = detail, ts
	}
	if state == "working" && slices.Contains(phaseNames, detail) {
		if (detail == "spec" || detail == "plan") && slices.Contains(terminalState, s.lastState[key]) {
			w.Stages, w.Ledger, w.LedgerTS = nil, "", 0
			for k := range s.d.Roles {
				if k.Crew == key.Crew && k.Branch == key.Branch {
					delete(s.d.Roles, k)
				}
			}
			delete(s.d.Subagents, key)
		}
		if n := len(w.Stages); n == 0 || w.Stages[n-1].Name != detail {
			w.Stages = append(w.Stages, StageMark{Name: detail, TS: ts})
		}
	}
	s.lastState[key] = state
}

func (s *BusScanner) applySubagent(key WorkerKey, session string, ts int64, body map[string]any) {
	phase, _ := body["subagent"].(string)
	if phase != "spawn" && phase != "finish" {
		return
	}
	str := func(name string, max int) string { return clipRunes(body[name], max) }
	id, agentID := str("id", fieldMax), str("agent_id", fieldMax)
	runs := s.d.Subagents[key]
	i := -1
	if id != "" {
		i = slices.IndexFunc(runs, func(r SubagentRun) bool { return r.ID == id })
	}
	if i < 0 && agentID != "" {
		i = slices.IndexFunc(runs, func(r SubagentRun) bool { return r.AgentID == agentID })
	}

	if phase == "spawn" {
		if i < 0 {
			if id == "" {
				return
			}
			runs = appendRun(runs, SubagentRun{ID: id, Session: session, State: "running", StartTS: ts})
			i = len(runs) - 1
		}
		r := &runs[i]
		// A finished run got there first: its fields are the ones actually used
		// (the spawn row carries the configured model), so the spawn only fills gaps.
		merge := mergeNonEmpty
		if r.State != "running" {
			merge = fillEmpty
		}
		merge(&r.AgentID, agentID)
		merge(&r.Engine, str("engine", fieldMax))
		merge(&r.Label, str("label", labelMax))
		merge(&r.Agent, str("agent", fieldMax))
		merge(&r.Model, str("model", fieldMax))
		merge(&r.Effort, str("effort", fieldMax))
		s.d.Subagents[key] = runs
		return
	}

	state := "done"
	if outcome, _ := body["outcome"].(string); outcome == "error" || outcome == "aborted" {
		state = "failed"
	}
	if i < 0 {
		// A finish can reach the bus before its spawn: keep it, finished, so the
		// late spawn only fills the fields it left empty. Without an id there is
		// nothing to match it on.
		if id == "" {
			return
		}
		runs = appendRun(runs, SubagentRun{ID: id, AgentID: agentID, Session: session, Model: str("model", fieldMax),
			State: state, StartTS: ts, EndTS: ts})
		s.d.Subagents[key] = runs
		return
	}
	if runs[i].State != "running" {
		return
	}
	r := &runs[i]
	r.State, r.EndTS = state, ts
	mergeNonEmpty(&r.Model, str("model", fieldMax))
	mergeNonEmpty(&r.AgentID, agentID)
}

// appendRun adds r, first evicting the oldest finished run (else the oldest
// run) once the worker is at subagentCap.
func appendRun(runs []SubagentRun, r SubagentRun) []SubagentRun {
	if len(runs) >= subagentCap {
		evict := slices.IndexFunc(runs, func(x SubagentRun) bool { return x.State != "running" })
		runs = slices.Delete(runs, max(evict, 0), max(evict, 0)+1)
	}
	return append(runs, r)
}

func mergeNonEmpty(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func fillEmpty(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func clipRunes(v any, n int) string {
	s, _ := v.(string)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (d BusDigest) clone() BusDigest {
	out := BusDigest{
		Workers:   make(map[WorkerKey]*WorkerDigest, len(d.Workers)),
		Roles:     make(map[RoleKey]*RoleDigest, len(d.Roles)),
		PRs:       make(map[int]*PRWatch, len(d.PRs)),
		Subagents: make(map[WorkerKey][]SubagentRun, len(d.Subagents)),
		Recent:    append([]RecentEvent{}, d.Recent...),
		Repo:      d.Repo,
	}
	if len(d.RecentByCrew) > 0 {
		out.RecentByCrew = make(map[string][]RecentEvent, len(d.RecentByCrew))
		for k, r := range d.RecentByCrew {
			out.RecentByCrew[k] = slices.Clone(r)
		}
	}
	for k, w := range d.Workers {
		c := *w
		c.Stages = append([]StageMark{}, w.Stages...)
		c.Activity = append([]int64{}, w.Activity...)
		out.Workers[k] = &c
	}
	for k, runs := range d.Subagents {
		out.Subagents[k] = slices.Clone(runs)
	}
	for k, r := range d.Roles {
		c := *r
		out.Roles[k] = &c
	}
	for k, p := range d.PRs {
		c := *p
		out.PRs[k] = &c
	}
	if d.Err != nil {
		e := *d.Err
		out.Err = &e
	}
	return out
}

// BodyObject returns the event body as an object, decoding it first when the
// bus carries it as a JSON-encoded string.
func BodyObject(v any) map[string]any {
	switch b := v.(type) {
	case map[string]any:
		return b
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(b), &m) == nil {
			return m
		}
	}
	return map[string]any{}
}

func int64Field(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}
