package data

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	activityWindowMS = 3600000
	recentCap        = 50
)

var (
	phaseNames    = []string{"spec", "plan", "execute", "gate", "review", "ship"}
	terminalState = []string{"done", "failed", "exited", "reaped"}
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
	Recent  []RecentEvent
	Err     *string
}

// BusScanner folds events.jsonl incrementally: it keeps a byte offset and only
// consumes complete lines, so a torn last line is retried on the next Scan.
type BusScanner struct {
	path      string
	mu        sync.Mutex
	off       int64
	d         BusDigest
	lastState map[WorkerKey]string
}

func NewBusScanner(path string) *BusScanner {
	s := &BusScanner{path: path}
	s.reset()
	return s
}

func (s *BusScanner) reset() {
	s.off = 0
	s.d = BusDigest{
		Workers: map[WorkerKey]*WorkerDigest{},
		Roles:   map[RoleKey]*RoleDigest{},
		PRs:     map[int]*PRWatch{},
		Recent:  []RecentEvent{},
	}
	s.lastState = map[WorkerKey]string{}
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

	state, _ := body["state"].(string)
	detail, _ := body["detail"].(string)
	summary := detail
	if str, ok := ev["body"].(string); ok && kind != "status" {
		summary = str
	}
	s.d.Recent = append(s.d.Recent, RecentEvent{TS: ts, From: from, Kind: kind, State: state, Detail: summary, Ev: ev})
	if len(s.d.Recent) > recentCap {
		s.d.Recent = slices.Clone(s.d.Recent[len(s.d.Recent)-recentCap:])
	}

	switch {
	case strings.HasPrefix(from, "worker:"):
		branch, _, _ := strings.Cut(strings.TrimPrefix(from, "worker:"), "#")
		s.applyWorker(WorkerKey{crew, branch}, ts, kind, state, detail, body["source"] == "watchdog")
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

func (s *BusScanner) applyWorker(key WorkerKey, ts int64, kind, state, detail string, watchdog bool) {
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
		}
		if n := len(w.Stages); n == 0 || w.Stages[n-1].Name != detail {
			w.Stages = append(w.Stages, StageMark{Name: detail, TS: ts})
		}
	}
	s.lastState[key] = state
}

func (d BusDigest) clone() BusDigest {
	out := BusDigest{
		Workers: make(map[WorkerKey]*WorkerDigest, len(d.Workers)),
		Roles:   make(map[RoleKey]*RoleDigest, len(d.Roles)),
		PRs:     make(map[int]*PRWatch, len(d.PRs)),
		Recent:  append([]RecentEvent{}, d.Recent...),
	}
	for k, w := range d.Workers {
		c := *w
		c.Stages = append([]StageMark{}, w.Stages...)
		c.Activity = append([]int64{}, w.Activity...)
		out.Workers[k] = &c
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
