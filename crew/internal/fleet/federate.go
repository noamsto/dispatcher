package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	overSize      = "answer over 4 MiB"
	// stderrMax is how much of a far stderr is kept: only its first line is
	// read.
	stderrMax = 4 << 10
)

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
// lane's inbox.
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
// above since. A bus the fold cannot read fails the answer: its replies would
// otherwise fall below a cursor the origin moves past.
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
	return ans, nil
}

// AdmitHost reads a partner's stdout as untrusted data: one JSON object of at
// most 4 MiB within the caps, decoded into the far wire types and projected
// field by field (farwire.go), every repo relabelled as host's. An inbox event
// the fold's rule for overseer:<self> above cutoff refuses is moved to
// Misrouted, by sender only.
func AdmitHost(stdout []byte, host, self string, cutoff int64) (FarAnswer, error) {
	if len(stdout) > farMaxBytes {
		return FarAnswer{}, errors.New(overSize)
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	dec.UseNumber()
	var top any
	if err := dec.Decode(&top); err != nil {
		return FarAnswer{}, fmt.Errorf("answer is not JSON: %s", oneLine(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return FarAnswer{}, errors.New("answer is more than one JSON value")
	}
	obj, ok := top.(map[string]any)
	if !ok {
		return FarAnswer{}, errors.New("answer is not a JSON object")
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
		return FarAnswer{}, fmt.Errorf("answer: %s", oneLine(err))
	}
	var wire farWire
	if err := decodeNumbers(raw, &wire); err != nil {
		return FarAnswer{}, fmt.Errorf("answer: %s", oneLine(err))
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

func capStatus(s farStatus) error {
	if n := len(s.Repos); n > farMaxRepos {
		return fmt.Errorf("answer has %d repos, over the cap of %d repos", n, farMaxRepos)
	}
	for _, r := range s.Repos {
		if n := len(r.Crews); n > farMaxCrews {
			return fmt.Errorf("answer repo has %d crews, over the cap of %d crews", n, farMaxCrews)
		}
		for _, c := range r.Crews {
			if n := len(c.Workers); n > farMaxWorkers {
				return fmt.Errorf("answer crew has %d workers, over the cap of %d workers", n, farMaxWorkers)
			}
		}
	}
	return nil
}

// recheck runs the overseer fold's rule again on the raw answer: an inbox
// event qualifies only as a msg to overseer:<self> from the dispatcher of the
// crew it names, whole-ms ts above cutoff, a string body within peer.MaxBody.
// Any other moves to misrouted by {repo, crew_id, from, ts}; a batch left
// empty is dropped.
func recheck(obj map[string]any, self string, cutoff int64) error {
	batches, _ := obj["inbox"].([]any)
	n := 0
	for _, b := range batches {
		bm, _ := b.(map[string]any)
		evs, _ := bm["events"].([]any)
		n += len(evs)
	}
	if n > FarMaxEvents {
		return fmt.Errorf("answer has %d inbox events, over the cap of %d inbox events", n, FarMaxEvents)
	}
	misrouted, _ := obj["misrouted"].([]any)
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

// failure is why a capture that exited rc gave no answer: the first line of
// its stderr, scrubbed, else the timeout or the status.
func failure(ctx context.Context, stderr string, rc int, timeout time.Duration) string {
	if line := strings.TrimSpace(scrub(firstLine(stderr))); line != "" {
		return line
	}
	if ctx.Err() != nil {
		return "no answer within " + timeout.String()
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

// headWriter keeps the first max bytes written to it and swallows the rest: a
// far stderr is read for its first line only, and refusing the rest would cut
// the capture short.
type headWriter struct {
	max int
	buf bytes.Buffer
}

func (w *headWriter) Write(p []byte) (int, error) {
	w.buf.Write(p[:min(len(p), max(w.max-w.buf.Len(), 0))])
	return len(p), nil
}

func (w *headWriter) String() string { return w.buf.String() }

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
		since := hostSince(stateDir, host, nowMS-inboxWindow.Milliseconds())
		f.wg.Go(func() { f.pulls[i] = pullHost(ctx, o.Federate, host, self, since, timeout) })
	}
	return f
}

// hostSince is the --since of a pull from host: the floor of the lane's inbox
// for it, else fallback. The lane drops what it already printed above that
// floor by key.
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
	stderr := &headWriter{max: stderrMax}
	argv := []string{"crew", "overseer", "status", "--origin", self, "--since", strconv.FormatInt(since, 10)}
	rc := capture(ctx, host, argv, stdout, stderr)
	switch {
	case stdout.over:
		return pull{reason: overSize}
	case rc != 0:
		return pull{reason: failure(ctx, stderr.String(), rc, timeout)}
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
			ts, last, age := p.ans.TS, nowMS, int64(0)
			s := p.ans.Status
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
		errb := &headWriter{max: stderrMax}
		rc := capture(ctx, host, argv, out, errb)
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
			_, _ = io.WriteString(stderr, errb.String())
			return 0
		}
		line = failure(ctx, errb.String(), rc, m.timeout)
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
// one JSON array, its rows that are no object or do not hold dropped.
func projectRows(stdout []byte) ([]byte, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(stdout, &rows); err != nil || rows == nil {
		return nil, errors.New("remote roster is not an array")
	}
	kept := []map[string]any{}
	for _, r := range rows {
		var raw map[string]json.RawMessage
		if json.Unmarshal(r, &raw) != nil || raw == nil {
			continue
		}
		if row := projectWorker(raw); row != nil {
			kept = append(kept, row)
		}
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
