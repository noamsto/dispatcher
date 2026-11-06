package fleet

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

const (
	// federateTimeout bounds each partner pull and overseer probe by default.
	federateTimeout = 10 * time.Second
	// farMaxBytes and the caps below bound what one partner's answer may hold.
	farMaxBytes   = 4 << 20
	farMaxRepos   = 200
	farMaxCrews   = 100
	farMaxWorkers = 200
	FarMaxEvents  = 500
	// FarSettle bounds how long a far msg takes from its ts to its bus: the
	// overseer lane's inbox floor rises to a far answer's ts less FarSettle.
	FarSettle = time.Minute
	overSize  = "answer over 4 MiB"
)

// RefuseOverBudget and RefuseTSGroupOverCap are the fixed codes a refusing far
// puts on its answer's stdout: the requester words each in its own sentence.
const (
	RefuseOverBudget     = "over_budget"
	RefuseTSGroupOverCap = "ts_group_over_cap"
)

// Refusal is why a far answer cannot advance: a fixed Code the requester maps
// to its own sentence, and the human Reason for stderr. It is not a FarAnswer,
// so no requester ever projects one.
type Refusal struct {
	Code   string
	Reason string
}

func (r Refusal) Error() string { return r.Reason }

// refusalSentence is this host's wording for a refusal code, the default for a
// code it does not know (mirroring overseer/msg.go's `refused (unknown
// reason)`): never empty, so finish never reads it as an ok answer, and never
// any far text.
func refusalSentence(code string) string {
	switch code {
	case RefuseOverBudget:
		return "refused: the far answer cannot advance: its oldest row is over the byte budget"
	case RefuseTSGroupOverCap:
		return "refused: the far answer cannot advance: too many rows share one ts"
	}
	return "refused (unknown reason)"
}

// refusedCode is the non-empty `refuse` key off a capture's stdout, "" when the
// stdout holds no refusal. A value that is not a JSON object with a string
// refuse key is not a refusal.
func refusedCode(stdout []byte) string {
	var obj struct {
		Refuse string `json:"refuse"`
	}
	if json.Unmarshal(stdout, &obj) != nil {
		return ""
	}
	return obj.Refuse
}

// FarAnswer is a partner's reply to `crew overseer status --origin <self>`:
// its own status in served mode and the msgs on its buses to overseer:<self>.
type FarAnswer struct {
	V         int            `json:"v"`
	Self      string         `json:"self"`
	TS        int64          `json:"ts"`
	Status    StatusDoc      `json:"status"`
	Inbox     []FarBatch     `json:"inbox"`
	Misrouted []FarMisrouted `json:"misrouted"`
	Cursor    int64          `json:"cursor"`
}

// FarBatch is one far bus's replies to the overseer, as `crew watch` batches
// them. Common is the far bus's common dir, part of each reply's key in the
// lane's inbox: only ever hashed, never validated or shown.
type FarBatch struct {
	Repo   string           `json:"repo"`
	Common string           `json:"common"`
	Cursor int64            `json:"cursor"`
	Events []map[string]any `json:"events"`
}

// FarMisrouted is a msg to the overseer from anyone but the dispatcher of the
// crew it names: who sent it, never what it said.
type FarMisrouted struct {
	Repo   string `json:"repo"`
	Common string `json:"common"`
	CrewID string `json:"crew_id"`
	From   string `json:"from"`
	TS     int64  `json:"ts"`
}

// Batches are the admitted replies of an ok partner, for the overseer lane;
// they never reach the status document.
func (h HostStatus) Batches() []FarBatch { return h.batches }

// Misroutes are the msgs an ok partner's buses hold for the overseer from
// anyone but a crew's dispatcher, by sender only.
func (h HostStatus) Misroutes() []FarMisrouted { return h.misroutes }

// ServeFar answers a partner overseer's pull: this host's status in served
// mode, and the fold of every bus that discovery found for overseer:<origin>
// above since, cut to the caps oldest-first (fitFar). A bus the fold cannot
// read fails the answer: its replies would otherwise fall below a cursor the
// origin moves past. So does a cut that cannot advance the origin's floor.
func ServeFar(ctx context.Context, origin string, since int64, stderr io.Writer, o StatusOptions) (FarAnswer, error) {
	served := &ServedStatus{Origin: origin, Since: since}
	if o.Served != nil {
		served.Root = o.Served.Root
	}
	o.Served = served
	doc, found := collectStatus(ctx, StatusRequest{}, stderr, o)
	labels := map[string]string{}
	for _, r := range doc.Repos {
		labels[r.Common] = r.Repo
	}
	ans := FarAnswer{V: 1, Self: doc.Host, TS: doc.TS, Status: doc, Inbox: []FarBatch{}, Misrouted: []FarMisrouted{}, Cursor: since}
	for _, r := range found {
		if r.Status != repos.StatusOK {
			continue
		}
		fold, ok, err := watch.FoldOverseer(filepath.Join(r.Common, "crew", "events.jsonl"), "overseer:"+origin, since)
		if errors.Is(err, bus.ErrNoLog) {
			continue
		}
		if err != nil {
			return FarAnswer{}, fmt.Errorf("%s: %s", r.Common, oneLine(err))
		}
		if !ok {
			continue
		}
		label, known := labels[r.Common]
		if !known {
			url, _ := git.Origin(ctx, o.Git, r.Common)
			label = repos.Label(r.Common, url)
		}
		ans.Cursor = max(ans.Cursor, fold.Cursor)
		for _, m := range fold.Misrouted {
			ans.Misrouted = append(ans.Misrouted, FarMisrouted{Repo: label, Common: r.Common, CrewID: m.Crew, From: m.From, TS: m.TS})
		}
		if fold.Batch == nil {
			continue
		}
		b := FarBatch{Repo: label, Common: r.Common, Cursor: fold.Cursor}
		events, _ := fold.Batch["events"].([]any)
		for _, e := range events {
			if ev, ok := e.(map[string]any); ok {
				b.Events = append(b.Events, ev)
			}
		}
		ans.Inbox = append(ans.Inbox, b)
	}
	if err := fitFar(&ans, FarMaxEvents, farMaxBytes); err != nil {
		return FarAnswer{}, err
	}
	return ans, nil
}

// fitFar keeps ans's oldest rows, inbox events and misrouted msgs together by
// ts, at most maxRows of them within maxBytes encoded, and never part of a ts
// group. When any is left out, ts drops to one below the first left out plus
// FarSettle, never above the real ts: the lane's floor, ts less FarSettle,
// then stays below every row left out, and its next pull is served them. When
// not even the oldest ts group fits, that floor could never rise: the answer
// is refused instead. The served overseer_inbox is trimmed to maxRows before
// the skeleton is measured, so each crew's inbox is already bounded when the
// answer is sized.
func fitFar(ans *FarAnswer, maxRows, maxBytes int) error {
	type row struct {
		ts           int64
		batch, index int // batch is -1 for a misrouted msg
		size         int
	}
	var rows []row
	for i, b := range ans.Inbox {
		for j, ev := range b.Events {
			ts, _ := watch.WholeMS(ev["ts"])
			rows = append(rows, row{ts, i, j, encodedLen(ev) + 1})
		}
	}
	for j, m := range ans.Misrouted {
		rows = append(rows, row{m.TS, -1, j, encodedLen(m) + 1})
	}
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(a.ts, b.ts) })

	// The sizes are json.Marshal's, an upper bound on the served encoding: it
	// escapes no HTML.
	cutOverseerInbox(&ans.Status, math.MaxInt64, maxRows)
	skeleton := *ans
	skeleton.Inbox, skeleton.Misrouted = []FarBatch{}, []FarMisrouted{}
	used := encodedLen(skeleton) + 1
	opened := make([]bool, len(ans.Inbox))
	n := 0
	for ; n < len(rows) && n < maxRows; n++ {
		r := rows[n]
		size := r.size
		if r.batch >= 0 && !opened[r.batch] {
			head := ans.Inbox[r.batch]
			head.Events = []map[string]any{}
			size += encodedLen(head) + 1
		}
		if used+size > maxBytes {
			break
		}
		used += size
		if r.batch >= 0 {
			opened[r.batch] = true
		}
	}
	if n == len(rows) {
		return nil
	}
	if n == 0 {
		return Refusal{Code: RefuseOverBudget, Reason: "answer cannot advance: its oldest row is over the byte budget"}
	}
	if ts := rows[0].ts; rows[n].ts == ts {
		group := 0
		for group < len(rows) && rows[group].ts == ts {
			group++
		}
		return Refusal{Code: RefuseTSGroupOverCap, Reason: fmt.Sprintf("answer cannot advance: %d rows share ts %d, over the caps", group, ts)}
	}
	for rows[n-1].ts == rows[n].ts {
		n--
	}
	ans.TS = min(ans.TS, rows[n].ts-1+FarSettle.Milliseconds())
	cutOverseerInbox(&ans.Status, rows[n].ts, maxRows)
	kept := map[[2]int]bool{}
	for _, r := range rows[:n] {
		kept[[2]int{r.batch, r.index}] = true
	}
	inbox := []FarBatch{}
	for i, b := range ans.Inbox {
		var events []map[string]any
		for j, ev := range b.Events {
			if kept[[2]int{i, j}] {
				events = append(events, ev)
			}
		}
		if len(events) > 0 {
			b.Events = events
			inbox = append(inbox, b)
		}
	}
	misrouted := []FarMisrouted{}
	for j, m := range ans.Misrouted {
		if kept[[2]int{-1, j}] {
			misrouted = append(misrouted, m)
		}
	}
	ans.Inbox, ans.Misrouted = inbox, misrouted
	return nil
}

// cutOverseerInbox keeps each crew's overseer_inbox, oldest first, below cut
// and at most maxRows long: the replies it summarizes are the inbox's, so it
// is cut with them, and a busy crew never puts its answer over the requester's
// cap.
func cutOverseerInbox(doc *StatusDoc, cut int64, maxRows int) {
	for i := range doc.Repos {
		for j := range doc.Repos[i].Crews {
			c := &doc.Repos[i].Crews[j]
			kept := slices.DeleteFunc(c.OverseerInbox, func(m InboxMsg) bool { return m.TS >= cut })
			c.OverseerInbox = kept[:min(len(kept), maxRows)]
		}
	}
}

func encodedLen(v any) int {
	raw, _ := json.Marshal(v)
	return len(raw)
}

// AdmitHost reads a partner's stdout as untrusted data: one JSON object of at
// most 4 MiB within the caps, decoded into the far wire types and projected
// field by field (farwire.go), every repo relabelled as host's. An inbox event
// the fold's rule for overseer:<self> above cutoff refuses is moved to
// Misrouted, by sender only. A refusal is worded here: no far text in it.
func AdmitHost(stdout []byte, host, self string, cutoff int64) (FarAnswer, error) {
	if len(stdout) > farMaxBytes {
		return FarAnswer{}, errors.New(overSize)
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	dec.UseNumber()
	var top any
	if dec.Decode(&top) != nil {
		return FarAnswer{}, errors.New("answer is not JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return FarAnswer{}, errors.New("answer is more than one JSON value")
	}
	obj, ok := top.(map[string]any)
	if !ok {
		return FarAnswer{}, errors.New("answer is not a JSON object")
	}
	if code, _ := obj["refuse"].(string); code != "" {
		return FarAnswer{}, errors.New(refusalSentence(code))
	}
	if v, ok := watch.WholeMS(obj["v"]); !ok || v != 1 {
		return FarAnswer{}, errors.New("answer version is not 1")
	}
	ts, ok := watch.WholeMS(obj["ts"])
	if !ok {
		return FarAnswer{}, errors.New("answer ts is not a whole ms")
	}
	if err := recheck(obj, self, cutoff); err != nil {
		return FarAnswer{}, err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return FarAnswer{}, errors.New("answer: cannot be re-encoded")
	}
	var wire farWire
	if decodeNumbers(raw, &wire) != nil {
		return FarAnswer{}, errors.New("answer: a field does not hold its type")
	}
	if err := capStatus(wire.Status); err != nil {
		return FarAnswer{}, err
	}
	return wire.project(host, self, ts), nil
}

func decodeNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// capStatus bounds every far list by count, so an answer within the byte cap
// cannot grow without bound as it is projected and printed.
func capStatus(s farStatus) error {
	var err error
	check := func(scope string, n, limit int, what string) {
		if err == nil && n > limit {
			err = fmt.Errorf("answer%s has %d %s, over the cap of %d %s", scope, n, what, limit, what)
		}
	}
	check("", len(s.Repos), farMaxRepos, "repos")
	check("", len(s.Skipped), farMaxRepos+farMaxCrews, "skipped repos")
	check(" queue", len(s.Queue.Demoted), farMaxCrews, "demoted holds")
	check(" queue", len(s.Queue.Parked), farMaxCrews, "parked holds")
	if m := s.Machine; m != nil {
		hot := 0
		for _, e := range m.Engines {
			hot += len(e.HotWindows)
		}
		check(" machine", len(m.Leads), farMaxWorkers, "leads")
		check(" machine", len(m.Engines), farMaxCrews, "engines")
		check(" machine", hot, farMaxWorkers, "hot windows")
	}
	for _, r := range s.Repos {
		check(" repo", len(r.Crews), farMaxCrews, "crews")
		for _, c := range r.Crews {
			check(" crew", len(c.Workers), farMaxWorkers, "workers")
			check(" crew", len(c.Holds), farMaxWorkers, "holds")
			check(" crew", len(c.OverseerInbox), FarMaxEvents, "overseer inbox msgs")
		}
	}
	return err
}

// recheck runs the overseer fold's rule again on the raw answer: an inbox
// event qualifies only as a msg to overseer:<self> from the dispatcher of the
// crew it names, whole-ms ts above cutoff, a string body within peer.MaxBody.
// Any other moves to misrouted by {repo, crew_id, from, ts}; a batch left
// empty is dropped. Events and misrouted msgs count toward one cap, as
// ServeFar fits them.
func recheck(obj map[string]any, self string, cutoff int64) error {
	batches, _ := obj["inbox"].([]any)
	misrouted, _ := obj["misrouted"].([]any)
	n := len(misrouted)
	for _, b := range batches {
		bm, _ := b.(map[string]any)
		evs, _ := bm["events"].([]any)
		n += len(evs)
	}
	if n > FarMaxEvents {
		return fmt.Errorf("answer has %d inbox events and misrouted msgs, over the cap of %d inbox events", n, FarMaxEvents)
	}
	kept := []any{}
	for _, b := range batches {
		bm, ok := b.(map[string]any)
		if !ok {
			kept = append(kept, b)
			continue
		}
		evs, _ := bm["events"].([]any)
		var stay []any
		for _, e := range evs {
			ev, _ := e.(map[string]any)
			if farEventOK(ev, self, cutoff) {
				stay = append(stay, ev)
				continue
			}
			crew, _ := ev["crew_id"].(string)
			from, _ := ev["from"].(string)
			ts, _ := watch.WholeMS(ev["ts"])
			misrouted = append(misrouted, map[string]any{"repo": bm["repo"], "common": bm["common"], "crew_id": crew, "from": from, "ts": json.Number(strconv.FormatInt(ts, 10))})
		}
		if len(stay) > 0 {
			bm["events"] = stay
			kept = append(kept, bm)
		}
	}
	obj["inbox"], obj["misrouted"] = kept, misrouted
	return nil
}

// farEventOK is the fold's rule, with the body a `crew msg` reply posts: text
// within peer.MaxBody.
func farEventOK(ev map[string]any, self string, cutoff int64) bool {
	crew, _ := ev["crew_id"].(string)
	ts, whole := watch.WholeMS(ev["ts"])
	body, text := ev["body"].(string)
	return ev["kind"] == "msg" && ev["to"] == "overseer:"+self && bus.ValidCrewID(crew) && ev["from"] == "dispatcher:"+crew &&
		whole && ts > cutoff && text && len(body) <= peer.MaxBody
}

// scrub drops control (C0, DEL, C1) and invisible characters from s and caps
// it at peer.MaxBody bytes on a rune boundary.
func scrub(s string) string {
	s = invisible.Drop(stripControl(s))
	if len(s) <= peer.MaxBody {
		return s
	}
	cut := peer.MaxBody
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// failure is why a capture that exited rc gave no answer, worded here: a far
// stderr is never read.
func failure(ctx context.Context, rc int, timeout time.Duration) string {
	switch {
	case ctx.Err() != nil:
		return "no answer within " + timeout.String()
	case rc == 255:
		return "ssh failed (255)"
	}
	return fmt.Sprintf("exit status %d", rc)
}

// cappedWriter keeps the first max bytes written to it and refuses the rest,
// so a far host cannot grow this one's memory without bound.
type cappedWriter struct {
	max  int
	buf  bytes.Buffer
	over bool
}

var errOverCap = errors.New(overSize)

func (w *cappedWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if len(p) <= room {
		return w.buf.Write(p)
	}
	w.over = true
	n, _ := w.buf.Write(p[:max(room, 0)])
	return n, errOverCap
}

func (o StatusOptions) federateTimeout() time.Duration {
	if o.FederateTimeout > 0 {
		return o.FederateTimeout
	}
	return federateTimeout
}

// federation is one collection's partner pulls. Each pull goroutine writes
// only its own slot; finish merges them once every pull returned.
type federation struct {
	hosts []string
	// gate is why no partner is pulled: the sender check's refusal, or settings
	// hosts that cannot be read.
	gate  string
	pulls []pull
	wg    sync.WaitGroup
}

type pull struct {
	ans    FarAnswer
	reason string // why there is no admitted answer, "" when there is
}

// federate starts one pull per settings host but self, unless the overseer
// sender check refused: then every partner reads unprobed with its reason.
// Settings hosts that cannot be read leave the partners an earlier pull left a
// state dir for, each unprobed with that reason.
func federate(ctx context.Context, stderr io.Writer, o StatusOptions, self, gate, stateDir string, nowMS int64) *federation {
	hosts, err := partners(o.Sender, self)
	if err != nil {
		say(stderr, "crew: fleet status: hosts: %s\n", oneLine(err))
		hosts = knownPartners(stateDir, self)
		if gate == "" {
			gate = "settings hosts cannot be read (" + scrub(oneLine(err)) + ")"
		}
	}
	f := &federation{hosts: hosts, gate: gate, pulls: make([]pull, len(hosts))}
	if gate != "" {
		return f
	}
	timeout := o.federateTimeout()
	for i, host := range hosts {
		// The lane's in-memory floor wins when it has one; a one-shot read
		// (nil hook) falls back to the file's floor and then the window, while a
		// host the lane has not seen yet gets the lane's start from hostFloor.
		since, ok := int64(0), false
		if o.HostSince != nil {
			since, ok = o.HostSince(host)
		}
		if !ok {
			since = hostSince(stateDir, host, nowMS-inboxWindow.Milliseconds())
		}
		f.wg.Go(func() { f.pulls[i] = pullHost(ctx, o.Federate, host, self, since, timeout) })
	}
	return f
}

// hostSince is the fallback --since of a pull from host: the floor of the
// lane's inbox file for it, else fallback. The lane drops what it already
// printed above that floor by key.
func hostSince(stateDir, host string, fallback int64) int64 {
	if in, ok := lane.ReadFarInbox(lane.OverseerHostInbox(stateDir, host)); ok {
		return in.Floor
	}
	return fallback
}

// knownPartners are the hosts but self with a state dir under the overseer's.
func knownPartners(stateDir, self string) []string {
	entries, _ := os.ReadDir(lane.OverseerHosts(stateDir))
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != self && remote.HostNameRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func partners(p sender.Probes, self string) ([]string, error) {
	if p.Hosts == nil {
		return nil, errors.New("no settings-hosts probe")
	}
	hosts, err := p.Hosts()
	if err != nil {
		return nil, err
	}
	var out []string
	for h := range hosts {
		if h != self && remote.HostNameRe.MatchString(h) {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}

func pullHost(ctx context.Context, capture roster.Capture, host, self string, since int64, timeout time.Duration) pull {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout := &cappedWriter{max: farMaxBytes}
	argv := []string{"crew", "overseer", "status", "--origin", self, "--since", strconv.FormatInt(since, 10)}
	rc := capture(ctx, host, argv, stdout, io.Discard)
	if stdout.over {
		return pull{reason: overSize}
	}
	// A refusal is a code the far put on its stdout, read before any exit
	// status: rc 0 with a code words the same sentence as a non-zero exit, and
	// no far text ever reaches it.
	if code := refusedCode(stdout.buf.Bytes()); code != "" {
		return pull{reason: refusalSentence(code)}
	}
	if rc != 0 {
		return pull{reason: failure(ctx, rc, timeout)}
	}
	ans, err := AdmitHost(stdout.buf.Bytes(), host, self, since)
	if err != nil {
		return pull{reason: err.Error()}
	}
	return pull{ans: ans}
}

// finish waits for every pull, then records each partner in host order: an
// admitted answer writes the ok file, a failed one reports the last ok's age.
func (f *federation) finish(stderr io.Writer, stateDir string, nowMS int64) []HostStatus {
	f.wg.Wait()
	out := make([]HostStatus, len(f.hosts))
	for i, host := range f.hosts {
		h := HostStatus{Host: host, Queue: QueueStatus{Demoted: []QueueDemote{}, Parked: []QueueParked{}}, Repos: []RepoStatus{}, Skipped: []Skipped{}}
		dir := lane.OverseerHostDir(stateDir, host)
		p := f.pulls[i]
		switch {
		case f.gate != "":
			reason := f.gate
			h.State, h.Error = "unprobed", &reason
			h.LastOkTS, h.AgeS = lastOK(dir, nowMS)
		case p.reason != "":
			reason := p.reason
			h.State, h.Error = "unreachable", &reason
			h.LastOkTS, h.AgeS = lastOK(dir, nowMS)
		default:
			if err := WriteOK(dir, nowMS); err != nil {
				say(stderr, "crew: fleet status: %s: ok file: %s\n", host, oneLine(err))
			}
			// fitFar lowers the answer ts to the cut point so an older lane keeps
			// a safe floor, while the partner's own status document carries the
			// clock the differ needs for lane_dead/blocked_aging: prefer that
			// whenever it is not behind the answer ts.
			ts, last, age := p.ans.TS, nowMS, int64(0)
			s := p.ans.Status
			if s.TS >= ts {
				ts = s.TS
			}
			h.State, h.TS, h.LastOkTS, h.AgeS = "ok", &ts, &last, &age
			h.Machine, h.Queue, h.Repos, h.Skipped = s.Machine, s.Queue, s.Repos, s.Skipped
			h.batches, h.misroutes = p.ans.Inbox, p.ans.Misrouted
			if p.ans.Self != host {
				e := fmt.Sprintf("far self-name %s ≠ settings key %s: its overseer's msgs are refused here", p.ans.Self, host)
				h.Error = &e
			}
		}
		out[i] = h
	}
	return out
}

// lastOK is dir's last ok and its age in seconds, both nil with no ok file.
func lastOK(dir string, nowMS int64) (*int64, *int64) {
	last, ok := ReadOK(dir)
	if !ok {
		return nil, nil
	}
	age := max((nowMS-last)/1000, 0)
	return &last, &age
}

// demoteGuests holds a guest crew to a home known to attend it: one whose home
// is neither the requester nor an ok partner reads as if it had no home, so it
// fails visible. Its home stays named.
func demoteGuests(doc *StatusDoc) {
	attended := map[string]bool{doc.Host: true}
	for _, h := range doc.Hosts {
		if h.State == "ok" {
			attended[h.Host] = true
		}
	}
	demote := func(rs []RepoStatus) {
		for i := range rs {
			for j := range rs[i].Crews {
				c := &rs[i].Crews[j]
				if c.Attention != "guest" || c.Dispatcher.Home != nil && attended[*c.Dispatcher.Home] {
					continue
				}
				homeless := *c
				homeless.Dispatcher.Home = nil
				c.Attention = attention(homeless)
			}
		}
	}
	demote(doc.Repos)
	for _, h := range doc.Hosts {
		demote(h.Repos)
	}
}

// probeMemo makes an overseer's --host probes read-only and bounded: only
// `crew roster --live -- <crew>` runs, each within the federate timeout and
// farMaxBytes, its rows projected as a far status's are, and a host whose
// transport failed once in this collection fails its later probes at once.
// Probes run concurrently across crews, hence the mutex.
type probeMemo struct {
	timeout time.Duration
	mu      sync.Mutex
	failed  map[string]string
}

func (m *probeMemo) wrap(capture roster.Capture, crew string) roster.Capture {
	return func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
		if !slices.Equal(argv, []string{"crew", "roster", "--live", "--", crew}) {
			say(stderr, "an overseer probe runs only crew roster --live -- %s\n", crew)
			return 1
		}
		m.mu.Lock()
		line, failed := m.failed[host]
		m.mu.Unlock()
		if failed {
			say(stderr, "%s\n", line)
			return 1
		}
		ctx, cancel := context.WithTimeout(ctx, m.timeout)
		defer cancel()
		out := &cappedWriter{max: farMaxBytes}
		rc := capture(ctx, host, argv, out, io.Discard)
		// An answer over the cap is this repo's alone, as a refused write can
		// end the transport with any status.
		if out.over {
			say(stderr, "%s\n", overSize)
			return 1
		}
		if rc == 0 {
			rows, err := projectRows(out.buf.Bytes())
			if err != nil {
				say(stderr, "%s\n", err)
				return 1
			}
			_, _ = stdout.Write(rows)
			return 0
		}
		line = failure(ctx, rc, m.timeout)
		// Only ssh's own 255 or no answer in time says the host is down; any
		// other status is this repo's checkout alone.
		if rc == 255 || ctx.Err() != nil {
			m.mu.Lock()
			if _, seen := m.failed[host]; !seen {
				m.failed[host] = line
			}
			m.mu.Unlock()
		}
		say(stderr, "%s\n", line)
		return rc
	}
}

// projectRows is a far `crew roster --live` answer with each row projected:
// one JSON array, one row per branch (the first), at most farMaxWorkers rows.
// A row that is no object or does not hold fails the answer, so its worker
// reads as a placeholder rather than vanishing.
func projectRows(stdout []byte) ([]byte, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(stdout, &rows); err != nil || rows == nil {
		return nil, errors.New("remote roster is not an array")
	}
	kept := []map[string]any{}
	branches := map[any]bool{}
	dropped := 0
	for _, r := range rows {
		var raw map[string]json.RawMessage
		var row map[string]any
		if json.Unmarshal(r, &raw) == nil && raw != nil {
			row = projectWorker(raw)
		}
		switch {
		case row == nil:
			dropped++
		case !branches[row["branch"]]:
			branches[row["branch"]] = true
			kept = append(kept, row)
		}
	}
	switch {
	case dropped > 0:
		return nil, fmt.Errorf("answer: %d roster rows dropped", dropped)
	case len(kept) > farMaxWorkers:
		return nil, fmt.Errorf("answer: %d roster rows, over the cap of %d rows", len(kept), farMaxWorkers)
	}
	return json.Marshal(kept)
}

// guestHomes reads the `@crew_id`/`@crew_dispatcher_host` window stamps: per
// crew id, the home host of a window another host's dispatcher launched here.
// Several homes for one id resolve to the first in sorted order.
func guestHomes(out, self string) map[string]string {
	homes := map[string]string{}
	for line := range strings.Lines(out) {
		id, home, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\x1f")
		if !ok || id == "" || home == "" || home == self || !remote.HostNameRe.MatchString(home) {
			continue
		}
		if cur, seen := homes[id]; !seen || home < cur {
			homes[id] = home
		}
	}
	return homes
}
