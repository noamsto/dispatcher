package fleet

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

// QueueUsage is the line every `crew fleet queue` grammar refusal prints.
const QueueUsage = "crew: fleet queue [--json] [--admit WEIGHT [--hold ID] [--own BRANCH]...] [--crew ID]"

const (
	defaultBackfillAfter = 1200
	defaultHeadTimeout   = 900
	// maxSeconds caps the timing settings, so a huge literal still converts.
	maxSeconds = math.MaxInt32
	// pendingWindowMS is how long a released fleet hold weighs as pending: the
	// time its dispatch has to put up a lead pane.
	pendingWindowMS = 120_000
)

// lockWait bounds how long a writer polls a busy lock; a test seam.
var lockWait = time.Second

// QueueSettings are the fleet queue's settings: fleetBudget, and
// fleetBackfillAfter and fleetHeadTimeout in seconds.
type QueueSettings struct {
	Budget, BackfillAfter, HeadTimeout int
}

var defaultQueueSettings = QueueSettings{Budget: defaultBudget, BackfillAfter: defaultBackfillAfter, HeadTimeout: defaultHeadTimeout}

// ResolveQueueSettings runs dispatch-config and reads the queue's settings the
// way ResolveBudget reads fleetBudget: each a non-negative whole number, its
// default when absent.
func ResolveQueueSettings(ctx context.Context, bin string, stderr io.Writer) (QueueSettings, error) {
	out, err := configJSON(ctx, bin, stderr)
	if err != nil {
		return QueueSettings{}, err
	}
	var s struct {
		FleetBudget        *float64 `json:"fleetBudget"`
		FleetBackfillAfter *float64 `json:"fleetBackfillAfter"`
		FleetHeadTimeout   *float64 `json:"fleetHeadTimeout"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return QueueSettings{}, fmt.Errorf("settings: %v", err)
	}
	return QueueSettings{
		Budget:        setting(s.FleetBudget, defaultBudget, maxBudget),
		BackfillAfter: setting(s.FleetBackfillAfter, defaultBackfillAfter, maxSeconds),
		HeadTimeout:   setting(s.FleetHeadTimeout, defaultHeadTimeout, maxSeconds),
	}, nil
}

// QueueOptions carry every outside read of a queue evaluation. The budget is
// Settings', so LoadOptions.Budget is never asked.
type QueueOptions struct {
	LoadOptions
	Proc proc.Table
	// Settings resolves the queue's settings; nil reads the defaults.
	Settings func(ctx context.Context) (QueueSettings, error)
	// Sender is the tripwire `crew fleet prio` runs.
	Sender sender.Probes
}

func (o QueueOptions) settings(ctx context.Context) (QueueSettings, error) {
	if o.Settings == nil {
		return defaultQueueSettings, nil
	}
	return o.Settings(ctx)
}

// queued is one outstanding fleet hold of a registry repo, and what orders it.
type queued struct {
	common, crew, id string
	branch, prio     string
	weight           int
	ts               int64 // its add row's, in ms
	repoPrio         int64
	lastRelease      int64 // its repo's newest fleet release, 0 for none
	eligible         bool
}

// releasedHold is a fleet hold released within the pending window.
type releasedHold struct {
	branch string
	weight int
}

// fleetScan is one read of the registry repos' buses: their outstanding fleet
// holds, the fleet holds released within the pending window, and each repo's
// newest fleet release.
type fleetScan struct {
	holds       []*queued
	released    []releasedHold
	lastRelease map[string]int64
}

// busReader reads one repo's bus as readBus does.
type busReader func(common string) (events []bus.Event, torn, skip string)

// busCache reads each bus once per command, so a status pass and its queue
// evaluation see one snapshot.
type busCache map[string]busRead

type busRead struct {
	events     []bus.Event
	torn, skip string
}

func (c busCache) read(common string) ([]bus.Event, string, string) {
	r, ok := c[common]
	if !ok {
		r.events, r.torn, r.skip = readBus(common)
		c[common] = r
	}
	return r.events, r.torn, r.skip
}

// scanFleet reads the registry's repos, never the walk: the walk's roots are
// the caller's, and every dispatcher must see the same queue. A repo whose bus
// or hold fold cannot be read adds nothing this evaluation.
func scanFleet(registry string, getenv func(string) string, read busReader, nowMS int64) fleetScan {
	s := fleetScan{lastRelease: map[string]int64{}}
	for _, r := range repos.Registered(registry, getenv) {
		if r.Status != repos.StatusOK {
			continue
		}
		if events, _, skip := read(r.Common); skip == "" {
			s.addRepo(r.Common, events, nowMS)
		}
	}
	return s
}

type crewHold struct{ crew, id string }

// holdAdd is the first add row of one hold id in a crew's sink.
type holdAdd struct {
	ts           int64
	fleet        bool
	branch, prio string
	weight       int
}

// addRepo adds one bus. Which holds are outstanding is hold.Outstanding's
// answer, the fold `crew hold due` matches the verdict against; a hold's ts is
// its first add row's, and a release counts once, at its first row.
func (s *fleetScan) addRepo(common string, events []bus.Event, nowMS int64) {
	adds := map[crewHold]holdAdd{}
	releasedAt := map[crewHold]int64{}
	var fleetCrews []string
	for _, e := range events {
		crew, ok := strings.CutPrefix(e.To, "hold:")
		if e.Kind != bus.KindMsg || !ok || crew != e.CrewID || !bus.ValidCrewID(crew) {
			continue
		}
		doc, _ := e.Doc.(map[string]any)
		body := holdBody(doc["body"])
		id, _ := body["id"].(string)
		k := crewHold{crew, id}
		_, added := adds[k]
		switch {
		case id == "":
		case body["released"] == true:
			if _, seen := releasedAt[k]; !seen {
				releasedAt[k] = tsOf(doc)
			}
		case !added:
			a := addOf(tsOf(doc), body)
			adds[k] = a
			if a.fleet && !slices.Contains(fleetCrews, crew) {
				fleetCrews = append(fleetCrews, crew)
			}
		}
	}
	docs := eventDocs(events)
	var holds []*queued
	for _, crew := range fleetCrews {
		out, err := hold.Outstanding(docs, crew)
		if err != nil {
			return
		}
		list, _ := out.([]any)
		seen := map[string]bool{}
		for _, h := range list {
			body, _ := h.(map[string]any)
			id, _ := body["id"].(string)
			a, ok := adds[crewHold{crew, id}]
			if !ok || !a.fleet || seen[id] {
				continue
			}
			seen[id] = true
			holds = append(holds, &queued{common: common, crew: crew, id: id, branch: a.branch, prio: a.prio, weight: a.weight, ts: a.ts})
		}
	}
	for k, at := range releasedAt {
		a, ok := adds[k]
		if !ok || !a.fleet {
			continue
		}
		s.lastRelease[common] = max(s.lastRelease[common], at)
		if age := nowMS - at; age >= 0 && age < pendingWindowMS {
			s.released = append(s.released, releasedHold{branch: a.branch, weight: a.weight})
		}
	}
	s.holds = append(s.holds, holds...)
}

// holdBody is a hold row's body as the hold fold reads it: JSON text in a
// string, decoding to an object. Anything else is no body.
func holdBody(v any) map[string]any {
	text, ok := v.(string)
	if !ok {
		return nil
	}
	var body map[string]any
	if json.Unmarshal([]byte(text), &body) != nil {
		return nil
	}
	return body
}

func addOf(ts int64, body map[string]any) holdAdd {
	a := holdAdd{ts: ts}
	wait, _ := body["wait"].(map[string]any)
	if wait["kind"] != "fleet" {
		return a
	}
	task, _ := body["task"].(map[string]any)
	prio, _ := task["prio"].(string)
	a.fleet, a.prio, a.weight = true, prioOf(prio), weightOf(wait["weight"])
	a.branch, _ = task["branch"].(string)
	return a
}

// weightOf is a fleet hold's weight, a whole number of at least 1. One that
// is no number cannot be weighed, so it reads as past any budget: parked as
// oversize, it never binds the queue.
func weightOf(v any) int {
	w, ok := number(v)
	if !ok {
		return maxBudget + 1
	}
	return int(math.Min(math.Max(math.Ceil(w), 1), maxBudget+1))
}

// prioOf is a hold's queue priority; a missing or unknown one is normal.
func prioOf(p string) string {
	if p == "high" || p == "low" {
		return p
	}
	return "normal"
}

func prioRank(p string) int {
	switch p {
	case "high":
		return 2
	case "low":
		return 0
	}
	return 1
}

type crewRef struct{ common, crew string }

// queue is one evaluation's inputs: the outstanding fleet holds in queue
// order, the capacity they wait on and the settings that judge them.
type queue struct {
	settings QueueSettings
	nowMS    int64
	load     LoadDoc
	owned    map[string]bool
	holds    []*queued
}

// newQueue weighs a scan: the machine's load, with the scan's releases as
// pending, each crew's eligibility, each repo's priority, and the holds in
// queue order.
func newQueue(ctx context.Context, o QueueOptions, scan fleetScan, s QueueSettings, now time.Time) *queue {
	t := o.Proc
	if t == nil {
		t = proc.OS{}
	}
	q := &queue{settings: s, nowMS: now.UnixMilli(), holds: slices.Clone(scan.holds)}
	q.load, q.owned = measure(ctx, s.Budget, o.LoadOptions, scan.released)
	prios := readPriorities(priorityPath(o.Getenv))
	eligible := map[crewRef]bool{}
	for _, h := range q.holds {
		ref := crewRef{h.common, h.crew}
		ok, seen := eligible[ref]
		if !seen {
			ok = readSession(filepath.Join(h.common, "crew", "crews", h.crew), t, now).eligible()
			eligible[ref] = ok
		}
		h.eligible, h.repoPrio, h.lastRelease = ok, prios[filepath.Clean(h.common)], scan.lastRelease[h.common]
	}
	slices.SortFunc(q.holds, queueOrder)
	return q
}

// queueOrder is the total order every dispatcher computes alike: repo
// priority, hold priority, the repo whose newest fleet release is oldest
// (never released first), the hold's age, then its identity.
func queueOrder(a, b *queued) int {
	return cmp.Or(
		cmp.Compare(b.repoPrio, a.repoPrio),
		cmp.Compare(prioRank(b.prio), prioRank(a.prio)),
		cmp.Compare(a.lastRelease, b.lastRelease),
		cmp.Compare(a.ts, b.ts),
		strings.Compare(a.common, b.common),
		strings.Compare(a.crew, b.crew),
		strings.Compare(a.id, b.id),
	)
}

func (q *queue) waitedS(h *queued) int64 { return max(q.nowMS-h.ts, 0) / 1000 }

// parked is why h cannot bind this evaluation, "" when it can: heavier than
// the budget, parked by demotion, or its crew not eligible, in that order.
func (q *queue) parked(h *queued, rec map[string]dueEntry) string {
	switch {
	case h.weight > q.settings.Budget:
		return "oversize"
	case rec[h.id].Parked != 0:
		return "demoted"
	case !h.eligible:
		return "ineligible"
	}
	return ""
}

// split is the holds that can bind, each part in queue order: the
// non-demoted, then the demoted.
func (q *queue) split(rec map[string]dueEntry) (fresh, demoted []*queued) {
	for _, h := range q.holds {
		switch {
		case q.parked(h, rec) != "":
		case rec[h.id].Demoted != 0:
			demoted = append(demoted, h)
		default:
			fresh = append(fresh, h)
		}
	}
	return fresh, demoted
}

// pick is the one hold due under rec, nil for none. Nothing is due while the
// load is over the cores: the gate refuses on load even when the weight fits,
// so a hold due on weight alone would loop through refused releases.
func (q *queue) pick(rec map[string]dueEntry) *queued {
	if q.load.LoadOver {
		return nil
	}
	free := 0
	if q.load.Free != nil {
		free = *q.load.Free
	}
	fits := func(h *queued) bool { return free >= h.weight }
	fresh, demoted := q.split(rec)
	i := slices.IndexFunc(fresh, fits)
	switch {
	case i < 0:
		if j := slices.IndexFunc(demoted, fits); j >= 0 {
			return demoted[j]
		}
		return nil
	case i > 0 && q.nowMS-fresh[0].ts >= int64(q.settings.BackfillAfter)*1000:
		// Head-of-line protection: past the backfill wait only the head may go,
		// so a deep hold is not starved by a stream of trivial ones.
		return nil
	}
	return fresh[i]
}

// advance is the demotion record after this evaluation, and the hold it
// finds due. It is pure: every evaluator runs it, and only a recording run
// keeps what it returns. The clock counts only time spent due, so congestion
// demotes nothing; each timeout demotes or parks one hold and the queue is
// picked again, so the next hold is due in the same evaluation.
func (q *queue) advance(rec map[string]dueEntry) (map[string]dueEntry, *queued) {
	next := map[string]dueEntry{}
	for _, h := range q.holds {
		if e, ok := rec[h.id]; ok {
			next[h.id] = e
		}
	}
	timeout := int64(q.settings.HeadTimeout) * 1000
	for {
		due := q.pick(next)
		for id, e := range next {
			if due == nil || id != due.id {
				e.FirstDue = 0
				next[id] = e
			}
		}
		if due == nil {
			return compact(next), nil
		}
		e := next[due.id]
		if e.FirstDue == 0 {
			e.FirstDue = q.nowMS
			next[due.id] = e
			return compact(next), due
		}
		if timeout == 0 || q.nowMS-e.FirstDue < timeout {
			return compact(next), due
		}
		if e.Demoted == 0 {
			e.Demoted = q.nowMS
		} else {
			e.Parked = q.nowMS
		}
		e.FirstDue = 0
		next[due.id] = e
	}
}

func compact(rec map[string]dueEntry) map[string]dueEntry {
	maps.DeleteFunc(rec, func(_ string, e dueEntry) bool { return e == dueEntry{} })
	return rec
}

// first is the first hold in order that can bind, nil when none can.
func (q *queue) first(rec map[string]dueEntry) *queued {
	fresh, demoted := q.split(rec)
	if all := append(fresh, demoted...); len(all) > 0 {
		return all[0]
	}
	return nil
}

// verdict is one crew's share of an evaluation: the due hold when it is the
// crew's, and the crew's holds parked by demotion or as oversize, which its
// owner must hear about. An ineligible crew's holds come back once it is.
func (q *queue) verdict(common, crew string, rec map[string]dueEntry, due *queued) hold.FleetVerdict {
	mine := func(h *queued) bool { return h.crew == crew && filepath.Clean(h.common) == filepath.Clean(common) }
	var v hold.FleetVerdict
	if due != nil && mine(due) {
		v.Due = []string{due.id}
	}
	for _, h := range q.holds {
		if reason := q.parked(h, rec); mine(h) && (reason == "demoted" || reason == "oversize") {
			v.Parked = append(v.Parked, hold.ParkedHold{ID: h.id, Reason: reason})
		}
	}
	return v
}

// dueEntry is one hold's demotion clocks, in ms: since when it has been the
// due hold (cleared once it is not), and when it was demoted and parked.
type dueEntry struct {
	FirstDue int64 `json:"first_due,omitempty"`
	Demoted  int64 `json:"demoted,omitempty"`
	Parked   int64 `json:"parked,omitempty"`
}

// dueFile is fleet-due.json, the machine's demotion record, keyed by hold id.
type dueFile struct {
	Holds map[string]dueEntry `json:"holds"`
}

func duePath(getenv func(string) string) string {
	return filepath.Join(stateHome(getenv), "crew", "fleet-due.json")
}

// stateHome is ${XDG_STATE_HOME:-$HOME/.local/state}.
func stateHome(getenv func(string) string) string {
	if dir := getenv("XDG_STATE_HOME"); dir != "" {
		return dir
	}
	return getenv("HOME") + "/.local/state"
}

// readRecord is the demotion record. A missing or unreadable one is empty,
// which restarts the clocks: a demotion comes at most one timeout late.
func readRecord(path string) map[string]dueEntry {
	var f dueFile
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &f) != nil || f.Holds == nil {
		return map[string]dueEntry{}
	}
	return f.Holds
}

// updateRecord runs step over the record under its lock, re-read inside it,
// and replaces the file only when the result differs. A step that changes
// nothing on an unlocked read takes no lock: every live dispatcher records on
// each stream tick. While another writer holds the lock past lockWait, step
// runs on an unlocked read and the file is left alone: the evaluation is then
// read-only, and a later run records.
func updateRecord(path string, step func(map[string]dueEntry) map[string]dueEntry) error {
	if prev := readRecord(path); maps.Equal(prev, step(prev)) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lockDir := filepath.Join(dir, "fleet-due.lock.d")
	if !acquire(lockDir) {
		step(readRecord(path))
		return nil
	}
	defer lock.Release(lockDir)
	prev := readRecord(path)
	next := step(prev)
	if maps.Equal(prev, next) {
		return nil
	}
	data, err := json.Marshal(dueFile{Holds: next})
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// acquire takes the lock at dir for this process, polling until lockWait
// runs out.
func acquire(dir string) bool {
	owner := strconv.Itoa(os.Getpid())
	deadline := time.Now().Add(lockWait)
	for !lock.Acquire(dir, owner) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// writeAtomic replaces path with data: a temp file in the same directory
// renamed over it, so a reader never sees half a file and a crash leaves the
// previous one.
func writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	// CreateTemp makes 0600; every dispatcher and the owner's tools read it.
	if err = tmp.Chmod(0o644); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Due evaluates the machine's fleet queue and returns crew's share of it: the
// due hold when it is this crew's (at most one is due machine-wide), and the
// crew's holds parked by demotion or as oversize. Every run evaluates the
// whole queue, so any live dispatcher's run moves a stuck owner's clock; only
// a recording run writes the demotion record.
func Due(ctx context.Context, common, crew string, record bool, o QueueOptions) (hold.FleetVerdict, error) {
	now := o.Clock.Now()
	scan := scanFleet(o.Registry, o.Getenv, readBus, now.UnixMilli())
	path := duePath(o.Getenv)
	var v hold.FleetVerdict
	step := func(map[string]dueEntry) map[string]dueEntry { return map[string]dueEntry{} }
	if len(scan.holds) == 0 {
		// No fleet hold anywhere: no load scan, and a record only to empty.
		if !record {
			return v, nil
		}
	} else {
		s, err := o.settings(ctx)
		if err != nil {
			return v, err
		}
		q := newQueue(ctx, o, scan, s, now)
		step = func(rec map[string]dueEntry) map[string]dueEntry {
			next, due := q.advance(rec)
			v = q.verdict(common, crew, next, due)
			return next
		}
	}
	if !record {
		step(readRecord(path))
		return v, nil
	}
	if err := updateRecord(path, step); err != nil {
		return hold.FleetVerdict{}, fmt.Errorf("demotion record: %w", err)
	}
	return v, nil
}

// QueueRequest is `crew fleet queue`'s flags. Admit, when positive, asks
// whether a new lead of that weight may start: Hold names the fleet hold it
// releases and Own its branches. Crew limits the listed holds to one crew,
// and under Admit says whose Hold it is.
type QueueRequest struct {
	JSON  bool
	Admit int
	Hold  string
	Crew  string
	Own   []string
}

type queueRef struct {
	Repo   string `json:"repo"`
	Common string `json:"common"`
	CrewID string `json:"crew_id"`
	ID     string `json:"id"`
}

type queueHead struct {
	queueRef
	WaitedS int64 `json:"waited_s"`
}

type queueRow struct {
	queueRef
	Branch    string  `json:"branch"`
	Prio      string  `json:"prio"`
	RepoPrio  int64   `json:"repo_prio"`
	Weight    int     `json:"weight"`
	TS        int64   `json:"ts"`
	WaitedS   int64   `json:"waited_s"`
	State     string  `json:"state"`
	Reason    *string `json:"reason"`
	FirstDue  *int64  `json:"first_due"`
	DemotedAt *int64  `json:"demoted_at"`
}

// queueDoc is `crew fleet queue --json`: the holds are listed non-demoted,
// demoted, then parked, each in queue order.
type queueDoc struct {
	TS       int64      `json:"ts"`
	Budget   int        `json:"budget"`
	Free     *int       `json:"free"`
	LoadOver bool       `json:"load_over"`
	Due      *queueRef  `json:"due"`
	Head     *queueHead `json:"head"`
	Holds    []queueRow `json:"holds"`
}

// Queue prints one read-only evaluation of the fleet queue, or under Admit
// judges a new lead against it: exit 0 admits, 3 refuses with the first hold
// in order named on stdout. A settings error is exit 1.
func Queue(ctx context.Context, req QueueRequest, stdout, stderr io.Writer, o QueueOptions) int {
	if req.Admit < 0 || req.Admit == 0 && (req.Hold != "" || len(req.Own) > 0) {
		say(stderr, "%s\n", QueueUsage)
		return ExitError
	}
	now := o.Clock.Now()
	scan := scanFleet(o.Registry, o.Getenv, readBus, now.UnixMilli())
	if req.Admit > 0 && len(scan.holds) == 0 {
		return ExitFits
	}
	s, err := o.settings(ctx)
	if err != nil {
		say(stderr, "crew: fleet queue: %s\n", oneLine(err))
		return ExitError
	}
	q := newQueue(ctx, o, scan, s, now)
	rec, due := q.advance(readRecord(duePath(o.Getenv)))
	names := repoNamer(ctx, o.Git)
	if req.Admit > 0 {
		return q.admit(req, rec, due, names, stdout)
	}
	doc := q.document(rec, due, req.Crew, names)
	if !req.JSON {
		return write(stdout, stderr, queueTable(doc.Holds))
	}
	var buf bytes.Buffer
	if err := jsonval.Encode(&buf, doc, false); err != nil {
		say(stderr, "crew: fleet queue: %v\n", err)
		return ExitError
	}
	return write(stdout, stderr, buf.Bytes())
}

// admit is the admission rule: a new lead may start when it re-dispatches a
// branch that already has a lead pane, when it is the release of the due
// hold, or when no hold that can bind is waiting. Parked holds bar nobody.
func (q *queue) admit(req QueueRequest, rec map[string]dueEntry, due *queued, names func(string) string, stdout io.Writer) int {
	if ownsAny(q.owned, req.Own) {
		return ExitFits
	}
	if due != nil && req.Hold == due.id && (req.Crew == "" || req.Crew == due.crew) {
		return ExitFits
	}
	h := q.first(rec)
	if h == nil {
		return ExitFits
	}
	say(stdout, "fleet holds are waiting — first in order: %s (%s, crew %s, waited %s)\n",
		cleanText(h.id), cleanText(names(h.common)), cleanText(h.crew), fmtAge(q.waitedS(h)))
	return ExitRefused
}

func (q *queue) document(rec map[string]dueEntry, due *queued, crew string, names func(string) string) queueDoc {
	ref := func(h *queued) queueRef {
		return queueRef{Repo: names(h.common), Common: h.common, CrewID: h.crew, ID: h.id}
	}
	d := queueDoc{TS: q.nowMS, Budget: q.settings.Budget, Free: q.load.Free, LoadOver: q.load.LoadOver, Holds: []queueRow{}}
	if due != nil {
		r := ref(due)
		d.Due = &r
	}
	fresh, demoted := q.split(rec)
	if len(fresh) > 0 {
		d.Head = &queueHead{queueRef: ref(fresh[0]), WaitedS: q.waitedS(fresh[0])}
	}
	listed := append(fresh, demoted...)
	for _, h := range q.holds {
		if q.parked(h, rec) != "" {
			listed = append(listed, h)
		}
	}
	for _, h := range listed {
		if crew != "" && h.crew != crew {
			continue
		}
		row := queueRow{
			queueRef: ref(h), Branch: h.branch, Prio: h.prio, RepoPrio: h.repoPrio, Weight: h.weight, TS: h.ts,
			WaitedS: q.waitedS(h), State: "waiting", FirstDue: nonZero(rec[h.id].FirstDue), DemotedAt: nonZero(rec[h.id].Demoted),
		}
		switch reason := q.parked(h, rec); {
		case reason != "":
			row.State, row.Reason = "parked", &reason
		case h == due:
			row.State = "due"
		case rec[h.id].Demoted != 0:
			row.State = "demoted"
		}
		d.Holds = append(d.Holds, row)
	}
	return d
}

func nonZero(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

func queueTable(rows []queueRow) []byte {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	say(tw, "STATE\tREPO\tCREW\tID\tPRIO\tWEIGHT\tWAITED\tBRANCH\n")
	for _, r := range rows {
		state := r.State
		if r.Reason != nil {
			state += ":" + *r.Reason
		}
		say(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			state, clean(r.Repo), clean(r.CrewID), clean(r.ID), r.Prio, r.Weight, fmtAge(r.WaitedS), clean(r.Branch))
	}
	_ = tw.Flush()
	return buf.Bytes()
}

// report is the queue as fleet status shows it: the head and its wait, the
// demoted holds with their clocks, and every parked hold with its reason.
func (q *queue) report(rec map[string]dueEntry, names func(string) string) QueueStatus {
	out := QueueStatus{Demoted: []QueueDemote{}, Parked: []QueueParked{}}
	fresh, demoted := q.split(rec)
	if len(fresh) > 0 {
		h := fresh[0]
		out.Head = &QueueHead{Repo: names(h.common), CrewID: h.crew, ID: h.id, WaitedS: q.waitedS(h)}
	}
	for _, h := range demoted {
		out.Demoted = append(out.Demoted, QueueDemote{Repo: names(h.common), CrewID: h.crew, ID: h.id, FirstDue: rec[h.id].FirstDue, DemotedAt: rec[h.id].Demoted})
	}
	for _, h := range q.holds {
		if reason := q.parked(h, rec); reason != "" {
			out.Parked = append(out.Parked, QueueParked{Repo: names(h.common), CrewID: h.crew, ID: h.id, Reason: reason})
		}
	}
	return out
}

// repoNamer names repos as status does, asking git for each origin once.
func repoNamer(ctx context.Context, g git.Client) func(common string) string {
	names := map[string]string{}
	return func(common string) string {
		if n, ok := names[common]; ok {
			return n
		}
		origin, _ := git.Origin(ctx, g, common)
		names[common] = repos.Name(common, origin)
		return names[common]
	}
}
