// `crew fleet stream --overseer` is the host's one wake stream for its overseer
// session, from two sources: the msgs to `overseer:<host>` on every bus, and
// the transitions between successive offline fleet status documents. One
// goroutine does it all in turn and starts no child: the overseer lane never
// reaps and never runs holds, which stay with the reap timer and each repo's
// own dispatcher.
//
// Msgs are at-least-once, as `crew watch`'s are: a batch is on the wire before
// the overseer's cursor for its bus moves. That cursor lives under the state
// dir, never in crews/<crew>/cursor, which is the dispatcher's own and would
// lose it its wakes. Transitions are re-derived from durable state, so one lost
// costs nothing beyond the overseer's next cold start.
package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	fleetpkg "github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	lanepkg "github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

const (
	// defaultOverseerInterval is the overseer lane's --interval: the transition
	// period, and the window that holds misrouted lines to one per repo.
	defaultOverseerInterval = 60
	// overseerPoll is how often each bus is read for msgs, in seconds: `crew
	// watch`'s default interval.
	overseerPoll = 2

	laneDeadAfter   = 10 * time.Minute
	blockedAgeAfter = 30 * time.Minute
	budgetLowS      = 1800
	headWaitS       = 1800
	senderCap       = 120
)

// wakeStates are the worker states a dead lane strands: the default --states.
var wakeStates = splitStates(defaultStates)

// runOverseer is the lane of `crew fleet stream --overseer`, one per host:
// overseer:<host> under the lane root, and the crew label of its lines.
// --states, --park, --coalesce and --retry are checked but do not apply.
func runOverseer(r Request, rediscover int64, stdout, stderr io.Writer, o FleetOptions) int {
	if r.Interval == "" {
		r.Interval = strconv.Itoa(defaultOverseerInterval)
	}
	c, msg := checkFlags(r)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitUsage
	}
	host, err := o.Host()
	if err != nil {
		say(stderr, "crew: fleet stream: host name: %s\n", oneLine(err))
		return exitUsage
	}
	c.crew = "overseer:" + host
	lane := filepath.Join(o.LaneRoot, c.crew)
	if c.statusMode {
		return status(lane, c.crew, stdout, o.Options)
	}
	if err := os.MkdirAll(lane, 0o755); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}
	// As in RunFleet: the arm precedes the acquire (#1294). ctx is how a signal
	// reaches a status read already under way.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()

	lockd := filepath.Join(lane, "stream.lock.d")
	if !acquire(lockd, c, o.Options, stderr) {
		return 1
	}

	tick := min(overseerPoll, c.interval, rediscover)
	tickCall := c
	tickCall.park = tick
	v := &overseer{
		c:          c,
		o:          o,
		lockd:      lockd,
		stateDir:   stateHome(o.Getenv),
		start:      clockMS(o.Options),
		out:        &laneWriter{w: stdout, now: o.Clock.Now, last: o.Clock.Now()},
		notes:      newNotes(&laneWriter{w: stderr, now: o.Clock.Now, last: o.Clock.Now()}),
		sup:        &suppress{key: map[string]string{}, ts: map[string]int64{}},
		collect:    o.CollectStatus,
		moves:      newTransitions(),
		interval:   time.Duration(c.interval) * time.Second,
		rediscover: time.Duration(rediscover) * time.Second,
		tick:       time.Duration(tick) * time.Second,
		ticker:     &loop{c: tickCall, o: o.Options, cdir: lane},
		buses:      map[string]*follower{},
	}
	return v.run(ctx, sigs)
}

// overseer is the lane's one goroutine: it writes the tick, rediscovers, polls
// each followed bus and diffs the fleet status in turn, so nothing in it needs
// a lock. A caught signal is acted on between those steps, never between a
// flush and its cursor write.
type overseer struct {
	c          call
	o          FleetOptions
	lockd      string
	stateDir   string
	start      int64
	out        *laneWriter
	notes      *notes
	sup        *suppress
	collect    func(context.Context, io.Writer) fleetpkg.StatusDoc
	moves      *transitions
	interval   time.Duration
	rediscover time.Duration
	tick       time.Duration
	ticker     *loop
	buses      map[string]*follower

	lastDiscover, lastDiff time.Time
	discovered, diffed     bool
}

// follower is one followed bus: its log, its repo label, the overseer's cursor
// for it, its misrouted msgs so far, and the log's size and mtime at the last
// poll that completed.
type follower struct {
	repo, log, cursorPath string
	cursor                int64
	misrouted             int
	reportedAt            int64
	reported              bool
	size                  int64
	mtime                 time.Time
}

func (v *overseer) run(ctx context.Context, sigs chan os.Signal) int {
	for {
		v.ticker.writeTick()
		if now := v.o.Clock.Now(); !v.discovered || now.Sub(v.lastDiscover) >= v.rediscover {
			v.discovered, v.lastDiscover = true, now
			discoverBuses(v.o.Discover, v.notes, v.buses,
				func(common string) { delete(v.buses, common) },
				func(common string, _ []bus.Event) { v.join(common) })
		}
		for _, common := range slices.Sorted(maps.Keys(v.buses)) {
			if pending(sigs) {
				return v.shutdown()
			}
			v.poll(v.buses[common])
		}
		if pending(sigs) {
			return v.shutdown()
		}
		if now := v.o.Clock.Now(); !v.diffed || now.Sub(v.lastDiff) >= v.interval {
			v.diffed, v.lastDiff = true, now
			v.diff(ctx)
		}
		if quiet := v.out.quiet(); quiet >= time.Duration(v.c.heartbeat)*time.Second {
			say(v.out, "%s\n", encode(heartbeatLine{
				Stream: "heartbeat",
				Crew:   v.c.crew,
				QuietS: int64(quiet / time.Second),
				TS:     clockMS(v.o.Options),
			}))
		}
		select {
		case <-sigs:
			return v.shutdown()
		case <-v.o.Rediscover(v.tick):
		}
	}
}

func (v *overseer) shutdown() int {
	lock.Release(v.lockd)
	return 0
}

// join follows a bus from the overseer's cursor for it. A missing cursor is
// seeded at the lane's start: older msgs reach the overseer through its cold
// start's overseer_inbox, not as wakes.
func (v *overseer) join(common string) {
	path := lanepkg.OverseerCursor(v.stateDir, common)
	cursor, ok := watch.ReadCursor(path)
	if !ok {
		if err := watch.WriteCursor(path, v.start); err != nil {
			v.notes.skip(common, oneLine(err))
			return
		}
		cursor = v.start
	}
	v.buses[common] = &follower{
		repo:       v.o.repoName(common),
		log:        filepath.Join(common, "crew", "events.jsonl"),
		cursorPath: path,
		cursor:     cursor,
	}
}

// poll prints what the bus holds for the overseer above its cursor, then moves
// the cursor. A batch that did not reach the wire, or a log the fold could not
// read, leaves the cursor where it was and the log's stat unrecorded, so the
// next poll, or the next lane, folds it again. A log whose size and mtime are
// those of the last completed poll is not folded again; the stat is taken
// before the fold, so an append during it is seen next time.
func (v *overseer) poll(b *follower) {
	fi, err := os.Stat(b.log)
	if err != nil {
		return
	}
	if fi.Size() == b.size && fi.ModTime().Equal(b.mtime) {
		return
	}
	fold, ok, err := watch.FoldOverseer(b.log, v.c.crew, b.cursor)
	if err != nil {
		v.fail(b, "fold "+b.log, "fold: "+oneLine(err))
		return
	}
	if !ok {
		b.size, b.mtime = fi.Size(), fi.ModTime()
		return
	}
	if fold.Batch != nil {
		fold.Batch["repo"] = b.repo
		var line bytes.Buffer
		if jsonval.Encode(&line, fold.Batch, false) != nil {
			return
		}
		if _, err := v.out.Write(line.Bytes()); err != nil {
			return
		}
		if v.o.Flush != nil && v.o.Flush() != nil {
			return
		}
	}
	v.misrouted(b, fold.Misrouted)
	b.cursor = fold.Cursor
	b.size, b.mtime = fi.Size(), fi.ModTime()
	if err := watch.WriteCursor(b.cursorPath, fold.Cursor); err != nil {
		// Kept in memory regardless: a restart re-delivers from the file, which
		// is the at-least-once side of the contract.
		v.fail(b, "cursor "+b.cursorPath, "cursor: "+oneLine(err))
	}
}

// fail prints an error line about b's bus, once per cause per --heartbeat.
func (v *overseer) fail(b *follower, kind, detail string) {
	ts := clockMS(v.o.Options)
	if v.sup.repeat(kind, digitsToN(detail), ts, v.c.heartbeat*1000) {
		say(v.out, "%s\n", encode(errorLine{Stream: "error", Crew: v.c.crew, RC: 1, Detail: detail, TS: ts, Repo: b.repo}))
	}
}

// misrouted counts the msgs to the overseer from anyone but their crew's
// dispatcher, and names the first sender at most once per repo per --interval:
// a stamped lead pane may post as any worker:<b>#s<sid>, so a line per sender
// would be unbounded.
func (v *overseer) misrouted(b *follower, ms []watch.Misrouted) {
	if len(ms) == 0 {
		return
	}
	b.misrouted += len(ms)
	now := clockMS(v.o.Options)
	if b.reported && now-b.reportedAt < v.interval.Milliseconds() {
		return
	}
	b.reported, b.reportedAt = true, now
	say(v.out, "%s\n", encode(misroutedLine{
		overseerHead: overseerHead{Stream: "overseer", Event: "misrouted_msg", Repo: b.repo, Crew: senderText(ms[0].Crew)},
		From:         senderText(ms[0].From),
		Misrouted:    b.misrouted,
		TS:           now,
	}))
}

// senderText is a sender, crew id or branch as an overseer line may carry it:
// the sender charset kept, any other rune a `?`, at most senderCap bytes.
func senderText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() == senderCap {
			break
		}
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune(":/#._@-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// diff computes the fleet status, given --interval at most, and prints every
// transition since the last. A document built under a done context is
// dropped: a probe it cut short can read as a cleared condition, re-arming
// its key.
func (v *overseer) diff(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, v.interval)
	defer cancel()
	var text bytes.Buffer
	doc := v.collect(ctx, &text)
	v.notes.lines(text.String())
	if ctx.Err() != nil {
		return
	}
	for _, line := range v.moves.next(doc, clockMS(v.o.Options)) {
		say(v.out, "%s\n", encode(line))
	}
}

// The overseer's lines, one struct per event, each opening with the event and
// the repo and crew it concerns.
type overseerHead struct {
	Stream string `json:"stream"`
	Event  string `json:"event"`
	Repo   string `json:"repo"`
	Crew   string `json:"crew"`
}

type misroutedLine struct {
	overseerHead
	From      string `json:"from"`
	Misrouted int    `json:"misrouted"`
	TS        int64  `json:"ts"`
}

type downLine struct {
	overseerHead
	State string `json:"state"`
	TS    int64  `json:"ts"`
}

type upLine struct {
	overseerHead
	TS int64 `json:"ts"`
}

type laneDeadLine struct {
	overseerHead
	DeadS int64 `json:"dead_s"`
	TS    int64 `json:"ts"`
}

type blockedAgingLine struct {
	overseerHead
	Session     string `json:"session"`
	Branch      string `json:"branch"`
	Trigger     string `json:"trigger"`
	Since       int64  `json:"since"`
	BudgetLeftS *int64 `json:"budget_left_s"`
	TS          int64  `json:"ts"`
}

type abandonedLine struct {
	overseerHead
	Session string `json:"session"`
	Branch  string `json:"branch"`
	TS      int64  `json:"ts"`
}

type capacityLine struct {
	overseerHead
	Host    string `json:"host"`
	Cause   string `json:"cause"`
	Hold    string `json:"hold"`
	WaitedS *int64 `json:"waited_s,omitempty"`
	TS      int64  `json:"ts"`
}

// transitions is the differ's memory between status documents: the level
// conditions true at the last evaluation, the crews it reported down, and when
// each live crew's lane was first seen dead.
type transitions struct {
	seen      bool
	on        map[condKey]bool
	down      map[crewRef]bool
	deadSince map[crewRef]int64
}

func newTransitions() *transitions {
	return &transitions{on: map[condKey]bool{}, down: map[crewRef]bool{}, deadSince: map[crewRef]int64{}}
}

type crewRef struct{ common, crew string }

// condKey is one level condition. A crew's is keyed by its repo's common dir
// (its lines carry the repo label); capacity_stuck's is machine-wide and has
// none.
type condKey struct {
	event, common, crew string
	id                  string // the worker's session, or the hold id
	kind                string // blocked_aging's trigger, or capacity_stuck's cause
	host, repo          string // capacity_stuck's
}

// next diffs doc against the last evaluation and returns a line for each
// condition that turned true; one that turned false re-arms. The first
// document is the baseline and returns none. A repo missing from doc was
// skipped, not cleared, so its keys carry forward unchanged. Conditions read
// doc's own ts; nowMS only stamps the lines.
func (t *transitions) next(doc fleetpkg.StatusDoc, nowMS int64) []any {
	present := map[string]bool{}
	for _, r := range doc.Repos {
		present[r.Common] = true
	}
	on := map[condKey]bool{}
	for k := range t.on {
		if k.common != "" && !present[k.common] {
			on[k] = true
		}
	}
	deadSince := map[crewRef]int64{}
	for ref, ts := range t.deadSince {
		if !present[ref.common] {
			deadSince[ref] = ts
		}
	}
	var out []any
	fire := func(k condKey, line any) bool {
		if on[k] {
			return false
		}
		on[k] = true
		if !t.seen || t.on[k] {
			return false
		}
		out = append(out, line)
		return true
	}

	for _, r := range doc.Repos {
		for _, c := range r.Crews {
			ref := crewRef{r.Common, c.CrewID}
			head := func(event string) overseerHead {
				return overseerHead{Stream: "overseer", Event: event, Repo: r.Repo, Crew: c.CrewID}
			}
			live := c.Dispatcher.State == "live"
			if live && t.down[ref] {
				delete(t.down, ref)
				out = append(out, upLine{overseerHead: head("dispatcher_up"), TS: nowMS})
			}
			// A crew going idle never fires: nothing waits on it. An idle one
			// gaining work again does.
			if !live && c.Attention == "unattended" {
				k := condKey{event: "dispatcher_down", common: r.Common, crew: c.CrewID}
				if fire(k, downLine{overseerHead: head("dispatcher_down"), State: c.Dispatcher.State, TS: nowMS}) {
					t.down[ref] = true
				}
			}
			if live && c.Dispatcher.Lane != nil && *c.Dispatcher.Lane == "dead" {
				since, ok := t.deadSince[ref]
				if !ok {
					since = doc.TS
				}
				deadSince[ref] = since
				if doc.TS-since >= laneDeadAfter.Milliseconds() && strandsWork(c.Workers) {
					fire(condKey{event: "lane_dead", common: r.Common, crew: c.CrewID},
						laneDeadLine{overseerHead: head("lane_dead"), DeadS: (doc.TS - since) / 1000, TS: nowMS})
				}
			}
			for _, w := range c.Workers {
				session, _ := w["from"].(string)
				raw, _ := w["branch"].(string)
				// A stamped lead pane can post as any worker:<branch>#s<sid>.
				who, branch := senderText(session), senderText(raw)
				if w["state"] == "failed" && w["detail"] == "blocked, no dispatcher reply" {
					fire(condKey{event: "abandoned", common: r.Common, crew: c.CrewID, id: session},
						abandonedLine{overseerHead: head("abandoned"), Session: who, Branch: branch, TS: nowMS})
				}
				b, _ := w["blocked"].(map[string]any)
				// A watchdog block has no reply to wait for, and an attended
				// crew's dispatcher owns it.
				if answered, _ := b["answered"].(bool); b == nil || answered || (b["source"] == "watchdog" && c.Attention != "unattended") {
					continue
				}
				since, _ := wholeNumber(b["since"])
				var left *int64
				if n, ok := wholeNumber(b["budget_left_s"]); ok {
					left = &n
				}
				aging := func(trigger string) (condKey, any) {
					return condKey{event: "blocked_aging", common: r.Common, crew: c.CrewID, id: session, kind: trigger},
						blockedAgingLine{overseerHead: head("blocked_aging"), Session: who, Branch: branch,
							Trigger: trigger, Since: since, BudgetLeftS: left, TS: nowMS}
				}
				if doc.TS-since >= blockedAgeAfter.Milliseconds() {
					fire(aging("age"))
				}
				if left != nil && *left < budgetLowS {
					fire(aging("budget"))
				}
			}
		}
	}

	// capacity_stuck reads the queue only once it has entries: before fleet
	// holds exist it stays empty, and an overshoot alone is no stuck queue.
	if q := doc.Queue; q.Head != nil || len(q.Demoted) > 0 || len(q.Parked) > 0 {
		stuck := func(cause, repo, crew, id string, waited *int64) {
			fire(condKey{event: "capacity_stuck", kind: cause, host: doc.Host, repo: repo, crew: crew, id: id},
				capacityLine{overseerHead: overseerHead{Stream: "overseer", Event: "capacity_stuck", Repo: repo, Crew: crew},
					Host: doc.Host, Cause: cause, Hold: id, WaitedS: waited, TS: nowMS})
		}
		if h := q.Head; h != nil && h.WaitedS >= headWaitS {
			waited := h.WaitedS
			stuck("head_wait", h.Repo, h.CrewID, h.ID, &waited)
		}
		for _, d := range q.Demoted {
			stuck("demoted", d.Repo, d.CrewID, d.ID, nil)
		}
		for _, p := range q.Parked {
			switch p.Reason {
			case "demoted":
				stuck("parked", p.Repo, p.CrewID, p.ID, nil)
			case "oversize":
				stuck("oversize", p.Repo, p.CrewID, p.ID, nil)
			}
		}
		// A budget of 0 disables the budget. No machine document this round is
		// unobserved, not cleared.
		overshoot := condKey{event: "capacity_stuck", kind: "overshoot", host: doc.Host}
		switch m := doc.Machine; {
		case m == nil:
			if t.on[overshoot] {
				on[overshoot] = true
			}
		case m.Budget > 0 && m.Weight > m.Budget:
			stuck("overshoot", "", "", "", nil)
		}
	}

	t.on, t.deadSince, t.seen = on, deadSince, true
	return out
}

// strandsWork is whether a dead lane leaves work unread: a worker that is not
// stale, in a state its dispatcher wakes on.
func strandsWork(workers []map[string]any) bool {
	for _, w := range workers {
		if state, _ := w["state"].(string); w["stale"] == false && slices.Contains(wakeStates, state) {
			return true
		}
	}
	return false
}

// wholeNumber reads a number in a status document, whichever Go type built it.
func wholeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
