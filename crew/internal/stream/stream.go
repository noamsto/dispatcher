// Package stream is `crew stream`: the long-lived process a streaming lane
// arms once, so the dispatcher's monitor is *pushed* batches instead of
// re-arming a one-shot `crew watch` every turn. One cycle writes the tick
// `--status` reads, announces matured holds, parks on the inner watch, passes
// one batch line through, and reaps finished workers.
//
// Every stdout line is a protocol message a dispatcher parses: the batch
// itself, then `{"stream":"heartbeat"|"error"|"hold_due"|"reap"|"status",…}`.
// The stream's own stderr stays empty in normal operation — the inner watch's
// stderr goes to a file, or its prose would sit next to the JSON.
//
// Three children are the crew script re-entered the way the bash arm re-entered
// `$0` (`bash -euo pipefail <self> …`): `watch` — the park, which reaches Go
// through that arm — `hold due`, and `reap`, which stays bash. The delegation
// arm exports the script's path as CREW_SELF; with it unset the `crew` on PATH
// is the same entrypoint.
//
// `stream.lock.d` goes through internal/lock, and is acquired before any signal
// is caught: the cleanup releases it unconditionally, so a handler armed on the
// refusal path would remove the *incumbent* stream's lock and let a second
// stream start for one crew.
//
// The clock is the wall clock — `date +%s`, jq's `now`, the real `sleep` — and
// never `$CREW_CLOCK`, which the suite exports for `await`'s sake: the arm read
// none of it.
package stream

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

//go:embed wants_reap.jq
var wantsReapProg string

const (
	defaultStates    = "blocked,pr_open,done,failed,exited"
	defaultPark      = 300
	defaultHeartbeat = 3300
	defaultCoalesce  = 5
	defaultRetry     = 30
	defaultInterval  = 2
	defaultReapEvery = 900

	// forceTries × forcePoll is the arm's 50×0.1s: bounded, because a holder
	// that ignores TERM would hang an indefinite wait forever.
	forceTries = 50
	forcePoll  = 100 * time.Millisecond

	// holdDueNone is `crew hold due`'s own "nothing matured" status: the one
	// non-zero rc that is not a failure.
	holdDueNone = 1

	exitUsage = 64
)

// Options is everything Run reads beyond the bus.
type Options struct {
	// CrewID resolves the crew `--crew` defaults to (`_crew_id`).
	CrewID func() string
	// Now is the wall clock: the arm timed the cadence with `date +%s` and its
	// line timestamps with jq's `now`.
	Now func() time.Time
	// Self is the crew script the loop re-enters for `watch`, `hold due` and
	// `reap`. Empty resolves CREW_SELF, then the `crew` on PATH.
	Self string
	// Bash is the interpreter the arm called its children with.
	Bash string
	// Probes is `_pid_alive`, the read `--status` makes of a lock's pid.
	Probes crews.Probes
	// Timer is each wait's channel, so a caught signal wins the race with a
	// coalesce or retry sleep. nil is time.After.
	Timer func(d time.Duration) <-chan time.Time
}

func (o Options) withDefaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CrewID == nil {
		o.CrewID = func() string { return "" }
	}
	if o.Bash == "" {
		o.Bash = "bash"
	}
	if o.Probes.Alive == nil {
		o.Probes = crews.DefaultProbes()
	}
	if o.Timer == nil {
		o.Timer = func(d time.Duration) <-chan time.Time { return time.After(d) }
	}
	if o.Self == "" {
		o.Self = crewScript()
	}
	return o
}

// crewScript is the entrypoint the loop re-enters: the path the delegation arm
// exported as its own `$0`, and failing that the `crew` on PATH — the same
// script, for a Go binary run directly rather than through it.
func crewScript() string {
	if self := os.Getenv("CREW_SELF"); self != "" {
		return self
	}
	if self, err := exec.LookPath("crew"); err == nil {
		return self
	}
	return "crew"
}

// call is one parsed invocation. park, interval and states stay the caller's
// bytes as well as their values: they are what the inner `crew watch` is
// re-entered with, exactly as the arm passed `"$park"` and `"$states"`.
type call struct {
	crew, statesText, parkText, intervalText string
	states                                   []string
	park, heartbeat, coalesce                int64
	retry, interval, reapEvery               int64
	force, statusMode                        bool
}

// Run is the arm, flag for flag and refusal for refusal. Usage and
// crew-resolution failures exit 64, not 1 — otherwise a `--status` call that
// simply could not find its crew reads as the `dead` state it never measured.
func Run(argv []string, paths bus.Paths, stdout, stderr io.Writer, opts Options) int {
	o := opts.withDefaults()
	c, msg := parse(argv, o)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitUsage
	}
	cdir := paths.CrewDir(c.crew)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}
	if c.statusMode {
		return status(cdir, c.crew, stdout, o)
	}

	lockd := filepath.Join(cdir, "stream.lock.d")
	if !acquire(lockd, c, o, stderr) {
		return 1
	}

	// Only now, with the lock held, is a signal caught: the cleanup releases
	// the lock unconditionally.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	l := &loop{
		c:       c,
		o:       o,
		cdir:    cdir,
		lockd:   lockd,
		stdout:  stdout,
		procs:   children{bash: o.Bash, self: o.Self},
		sup:     &suppress{key: map[string]string{}, ts: map[string]int64{}},
		outf:    filepath.Join(cdir, "stream.out"),
		errf:    filepath.Join(cdir, "stream.err"),
		holderr: filepath.Join(cdir, "stream.hold.err"),
		reapout: filepath.Join(cdir, "stream.reap.out"),
		reaperr: filepath.Join(cdir, "stream.reap.err"),
	}
	return l.run(sigs)
}

// acquire is the arm's lock step. A second stream for one crew is refused —
// enforced by the tool rather than maintained by hand — and --force reclaims a
// lock whose holder is a single positive pid.
func acquire(lockd string, c call, o Options, stderr io.Writer) bool {
	pid := strconv.Itoa(os.Getpid())
	if lock.Acquire(lockd, pid) {
		return true
	}
	holder := lock.Holder(lockd)
	if !c.force {
		say(stderr, "crew: another stream is already running for this crew (%s) — pid %s; use --force to reclaim a stale one\n",
			c.crew, orUnknown(holder))
		return false
	}
	// The same sanitisation `--status` applies before trusting a lock pid, plus
	// the range a pid can be: 0 and -1 both read as live to `kill -0`, but as a
	// signal target 0 hits this whole process group and a negative pid hits every
	// process we can signal. Out-of-range is negative in disguise — syscall.Kill
	// truncates its pid to pid_t, so 4294967295 arrives at the kernel as -1 — and
	// it is what bash's own kill refused before the arm ever sent a TERM.
	target, ok := signalTarget(holder)
	if !ok {
		say(stderr, "crew: --force found no valid holder pid for crew (%s) stream lock (got '%s') — refusing to signal\n",
			c.crew, orEmpty(holder))
		return false
	}
	_ = syscall.Kill(target, syscall.SIGTERM)
	if !cleared(target, o) {
		say(stderr, "crew: --force sent TERM but pid %s for crew (%s) did not clear\n", holder, c.crew)
		return false
	}
	if !lock.Acquire(lockd, pid) {
		say(stderr, "crew: another stream is already running for this crew (%s)\n", c.crew)
		return false
	}
	return true
}

// cleared is the arm's bounded wait: bare `kill -0`, paired with the TERM
// above, confirms signal delivery rather than general liveness.
func cleared(pid int, o Options) bool {
	for tries := 0; tries < forceTries; tries++ {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		<-o.Timer(forcePoll)
	}
	return false
}

// status is `--status`: the state machine a dispatcher reads before it decides
// whether to arm a stream at all.
//
// A live pid is not evidence notifications are flowing (a rate-limit
// auto-stop, a closed stdout and pid reuse all leave a pid that looks alive),
// so `alive` also needs a tick younger than 2×park+60 — computed from the park
// THE TICK ITSELF recorded, not this call's, so a stale `--park` on the status
// call cannot shift the answer. A lock with no readable tick counts as `stale`,
// the safe direction: the tick is written immediately on acquiring the lock.
func status(cdir, crew string, stdout io.Writer, o Options) int {
	holder := statusPID(lock.Holder(filepath.Join(cdir, "stream.lock.d")))
	live := isSinglePositivePID(holder) && o.Probes.Alive(pidNumber(holder))

	// The holder is reported whether or not it answers: `dead` still names the
	// pid that is gone.
	pid := jsonv.Null()
	if holder != "" {
		if v, ok := jsonv.ParseNumber(holder); ok {
			pid = v
		}
	}
	st, rc := "dead", 2
	age := jsonv.Null()
	if live {
		st, rc = "stale", 1
		if tick := readTick(filepath.Join(cdir, "stream.tick")); tick.ok {
			ageS := (clockMS(o) - tick.ts) / 1000
			age = jsonv.Num(float64(ageS))
			if ageS < 2*tick.park+60 {
				st, rc = "alive", 0
			}
		}
	}
	say(stdout, "%s\n", encode(jsonv.Object(
		member("stream", jsonv.Str("status")),
		member("state", jsonv.Str(st)),
		member("crew", jsonv.Str(crew)),
		member("pid", pid),
		member("age_s", age),
	)))
	return rc
}

// tickState is a stream.tick: the two numbers the writer recorded, and ok for
// whether the file read as both of them.
type tickState struct {
	ts, park int64
	ok       bool
}

// readTick is the arm's `jq -r '.ts // empty'` plus its digits-only `case`: a
// missing file, one that will not decode, and a non-numeric field all read the
// same — no tick.
func readTick(path string) tickState {
	v, err := decodeOne(readFile(path))
	if err != nil {
		return tickState{}
	}
	ts, okTS := digitsField(v, "ts")
	park, okPark := digitsField(v, "park")
	if !okTS || !okPark {
		return tickState{}
	}
	return tickState{ts: ts, park: park, ok: true}
}

// statusPID is the arm's `case "$lockpid" in ” | *[!0-9]* | 0)`: empty,
// non-numeric and a literal 0 are all no holder at all.
func statusPID(text string) string {
	if !isDigits(text) || text == "0" {
		return ""
	}
	return text
}

// isSinglePID is the same guard before a pid becomes a *signal target*, and
// the range check is what keeps a truncated pid out of syscall.Kill.
func isSinglePositivePID(text string) bool { return lock.ValidHolder(text) }

// signalTarget is that guard with the value it names: the only holder --force
// will ever signal. It is one step stricter than the arm's `case`, which let the
// digit string "00" through to `kill -TERM 00` — this whole process group — and
// waited five seconds to report that it had not cleared. Signalling a group is
// refused; `--status` still reads "00" as held, exactly as the arm did.
func signalTarget(holder string) (int, bool) {
	if !isSinglePositivePID(holder) {
		return 0, false
	}
	n, err := strconv.Atoi(holder)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// pidNumber is the arm's arithmetic context: the digits as an int. Every caller
// is behind isSinglePositivePID, so an unparsable or out-of-range holder never
// reaches a probe; -1 keeps that true if a caller forgets.
func pidNumber(text string) int {
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 || n > lock.MaxPID {
		return -1
	}
	return n
}

// parse reads the arm's flag loop and its value checks, in its order: the loop
// first, then the five positive intervals, then --reap-every (non-negative: 0
// is how the cadence reap is disabled, not an error), then --states, the crew
// and the crew's charset.
func parse(argv []string, o Options) (call, string) {
	var (
		c                 call
		heartbeatText     = strconv.Itoa(defaultHeartbeat)
		coalesceText      = strconv.Itoa(defaultCoalesce)
		retryText         = strconv.Itoa(defaultRetry)
		reapEveryText     = strconv.Itoa(defaultReapEvery)
		statesText        = defaultStates
		parkText          = strconv.Itoa(defaultPark)
		intervalText      = strconv.Itoa(defaultInterval)
		crew              string
		force, statusMode bool
	)
	for len(argv) > 0 {
		var dst *string
		switch flag := argv[0]; flag {
		case "--crew":
			dst = &crew
		case "--states":
			dst = &statesText
		case "--park":
			dst = &parkText
		case "--heartbeat":
			dst = &heartbeatText
		case "--coalesce":
			dst = &coalesceText
		case "--retry":
			dst = &retryText
		case "--interval":
			dst = &intervalText
		case "--reap-every":
			dst = &reapEveryText
		case "--force":
			force = true
			argv = argv[1:]
			continue
		case "--status":
			statusMode = true
			argv = argv[1:]
			continue
		default:
			return c, fmt.Sprintf("crew: stream: unknown arg '%s'", flag)
		}
		if len(argv) < 2 || argv[1] == "" {
			return c, fmt.Sprintf("crew: %s needs a value", argv[0])
		}
		*dst, argv = argv[1], argv[2:]
	}

	c.statesText, c.parkText, c.intervalText, c.crew = statesText, parkText, intervalText, crew
	c.force, c.statusMode = force, statusMode
	for _, f := range []struct {
		name, text string
		dst        *int64
	}{
		{"park", parkText, &c.park},
		{"heartbeat", heartbeatText, &c.heartbeat},
		{"coalesce", coalesceText, &c.coalesce},
		{"retry", retryText, &c.retry},
		{"interval", intervalText, &c.interval},
	} {
		n, ok := positiveInt(f.text)
		if !ok {
			return c, fmt.Sprintf("crew: --%s must be a positive integer number of seconds", f.name)
		}
		*f.dst = n
	}
	if !isNonNegative(reapEveryText) {
		return c, "crew: --reap-every must be a non-negative integer number of seconds"
	}
	c.reapEvery, _ = strconv.ParseInt(reapEveryText, 10, 64)

	c.states = splitStates(statesText)
	if len(c.states) == 0 {
		return c, "crew: --states must be non-empty"
	}
	if c.crew == "" {
		c.crew = o.CrewID()
	}
	if c.crew == "" {
		return c, "crew: CREW_ID unset and no WORKER_TASK.md crew_id"
	}
	// The guard `adopt` applies to its caller-supplied id: `--crew` here is
	// just as caller-supplied, and unvalidated would let `/` or `..` mkdir and
	// write a pid file outside the bus dir.
	if !bus.ValidCrewID(c.crew) {
		return c, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'"
	}
	return c, ""
}

// splitStates is the arm's `jq -Rc 'split(",") | map(select(length>0))'`, so
// `a,,b` is two states and `,` alone is the empty set it refuses.
func splitStates(text string) []string {
	var out []string
	for _, s := range strings.Split(text, ",") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// positiveInt is the arm's two tests: the digits-only `case`, then `-gt 0` —
// which is also where a value past int64 lands, since bash's `test` cannot
// judge it either.
func positiveInt(text string) (int64, bool) {
	if !isDigits(text) {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func isNonNegative(text string) bool {
	if !isDigits(text) {
		return false
	}
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil
}

// suppress is the arm's "one line per cause, re-emitted once --heartbeat has
// passed" memory, one key per kind. A key normalises away the digits a stable
// failure embeds — a cursor value, a timestamp — so a persistent cause prints
// once instead of once per --retry.
type suppress struct {
	key map[string]string
	ts  map[string]int64
}

// repeat reports whether this kind's new key earns a line now, and remembers it
// when it does.
func (s *suppress) repeat(kind, key string, nowMS, heartbeatMS int64) bool {
	if s.key[kind] != key || nowMS-s.ts[kind] >= heartbeatMS {
		s.key[kind] = key
		s.ts[kind] = nowMS
		return true
	}
	return false
}

// clear forgets one kind, so the same key earns a line again next time — the
// arm's `last_hold_key=""` when nothing is matured any more.
func (s *suppress) clear(kind string) { s.key[kind] = "" }

// loop is one running stream: its cycle state, its children and its files.
type loop struct {
	c      call
	o      Options
	cdir   string
	lockd  string
	stdout io.Writer
	procs  children
	sup    *suppress

	outf    string
	errf    string
	holderr string
	reapout string
	reaperr string

	// quiet is the seconds of park that have come and gone with nothing to
	// report; the heartbeat is what it has to reach.
	quiet    int64
	lastReap int64
	child    *child
	reap     *reapRun
}

func (l *loop) run(sigs chan os.Signal) int {
	// Pinned temp names, not mktemp: they are per-crew and already mutually
	// excluded by stream.lock.d, so two streams cannot collide on them. Only
	// the reap pairs are per-child, and the sweep takes the base names plus any
	// orphan pair a previous child left behind.
	sweepReaped(l.reapout, l.reaperr)
	l.lastReap = l.o.Now().Unix()

	for {
		if pending(sigs) {
			// 0, not 128+n: a caught signal is a deliberate stop — a --force
			// takeover, the harness ending the lane — and a non-zero one reads as
			// "the lane crashed".
			l.cleanup()
			return 0
		}
		// Written at the top of every iteration — including the first, right
		// after the lock is acquired — because a live lock pid is not evidence
		// that notifications are flowing (see --status).
		l.writeTick()
		l.flushReap()
		l.holdDue()
		rc, sig := l.park(sigs)
		if sig != nil {
			l.cleanup()
			return 0
		}
		if sig := l.afterWatch(rc, sigs); sig != nil {
			l.cleanup()
			return 0
		}
		// Cadence: every branch above may have consumed anywhere from --interval
		// to --park+--retry seconds, so the elapsed check belongs here rather
		// than pinned to one branch.
		if l.c.reapEvery > 0 && l.o.Now().Unix()-l.lastReap >= l.c.reapEvery {
			l.startReap()
		}
	}
}

// park is the inner `crew watch`, a child so a caught signal can stop it rather
// than waiting out --park. Never --since: the per-crew cursor file self-seeds
// the park, exactly as a bare `crew watch` would, so the cursor keeps advancing
// across iterations without this process tracking it.
func (l *loop) park(sigs chan os.Signal) (int, os.Signal) {
	args := []string{"watch", "--crew", l.c.crew, "--timeout", l.c.parkText,
		"--states", l.c.statesText, "--interval", l.c.intervalText}
	ch, err := l.procs.start(args, l.outf, l.errf)
	if err != nil {
		// The arm had the same hole in its redirect: the loop treats a child it
		// could not start as one that failed, with the reason in its stderr file.
		_ = os.WriteFile(l.errf, []byte(err.Error()+"\n"), 0o644)
		return exitCantExec, nil
	}
	l.child = ch
	done := make(chan int, 1)
	go func() { done <- ch.wait() }()

	select {
	case s := <-sigs:
		return 0, s
	case rc := <-done:
		l.child = nil
		return rc, nil
	}
}

// afterWatch reads one cycle's outcome: the batch it printed, the quiet it
// accumulated, or the failure it will retry. It returns a signal that cut the
// cycle's wait short.
func (l *loop) afterWatch(rc int, sigs chan os.Signal) os.Signal {
	text, ok := readIfNonEmpty(l.outf)
	if ok {
		// A qualifying batch. Re-entering `watch` immediately would turn N
		// events trickling in over N seconds into N notifications, so the
		// --coalesce sleep is what holds one turn to one batch.
		say(l.stdout, "%s\n", text)
		l.flushReap()
		// React to the event we already have rather than waiting out the
		// cadence: a terminal status or a PR-merged msg is worth a reap now.
		if wantsReap(text) {
			l.startReap()
		}
		truncate(l.outf)
		l.quiet = 0
		return l.wait(time.Duration(l.c.coalesce)*time.Second, sigs)
	}
	if rc == 0 {
		// The park expired with nothing to report.
		l.quiet += l.c.park
		if l.quiet >= l.c.heartbeat {
			l.emit(jsonv.Object(
				member("stream", jsonv.Str("heartbeat")),
				member("crew", jsonv.Str(l.c.crew)),
				member("quiet_s", jsonv.Num(float64(l.quiet))),
				member("ts", jsonv.Num(float64(l.nowMS()))),
			))
			l.quiet = 0
		}
		return nil
	}
	// The inner watch failed. Its own stderr stays in the file, or it would put
	// prose next to the JSON event stream.
	detail := firstLine(l.errf)
	ts := l.nowMS()
	if l.sup.repeat("watch", fmt.Sprintf("%d:%s", rc, digitsToN(detail)), ts, l.heartbeatMS()) {
		l.emit(jsonv.Object(
			member("stream", jsonv.Str("error")),
			member("crew", jsonv.Str(l.c.crew)),
			member("rc", jsonv.Num(float64(rc))),
			member("detail", jsonv.Str(detail)),
			member("ts", jsonv.Num(float64(ts))),
		))
	}
	return l.wait(time.Duration(l.c.retry)*time.Second, sigs)
}

// holdDue runs `crew hold due` ahead of the park, so a matured hold announces
// on the batch path too, and at --park resolution rather than --heartbeat —
// which is coarser than the longest wait a hold can legally carry.
//
// The rc is read rather than discarded: `due` exits 1 for the ordinary "nothing
// matured", so a genuine failure would fold into that same silence, and a hold
// exists precisely because nobody is watching the lane.
func (l *loop) holdDue() {
	out, rc := l.procs.run([]string{"hold", "due", "--crew", l.c.crew, "--json"}, l.holderr)
	if rc > holdDueNone {
		detail := firstLine(l.holderr)
		ts := l.nowMS()
		if l.sup.repeat("holderr", fmt.Sprintf("%d:%s", rc, digitsToN(detail)), ts, l.heartbeatMS()) {
			l.emit(jsonv.Object(
				member("stream", jsonv.Str("error")),
				member("crew", jsonv.Str(l.c.crew)),
				member("rc", jsonv.Num(float64(rc))),
				member("detail", jsonv.Str("hold due: "+detail)),
				member("ts", jsonv.Num(float64(ts))),
			))
		}
		out = ""
	}
	matured := strings.TrimRight(out, "\n")
	if matured == "" || matured == "[]" {
		// Empty as well as `[]`: a failed shell-out above may print nothing at
		// all, and neither answer may reach the fold. Cleared rather than kept,
		// so a hold released and later re-added announces again.
		l.sup.clear("hold")
		return
	}
	holds, err := decodeOne(matured)
	if err != nil {
		return
	}
	// Keyed on the matured set, not on the transition into it: releasing one of
	// several holds changes the key, so the rest re-announce on the next
	// iteration instead of stranding until the stream restarts.
	ts := l.nowMS()
	if l.sup.repeat("hold", holdIDs(holds), ts, l.heartbeatMS()) {
		l.emit(jsonv.Object(
			member("stream", jsonv.Str("hold_due")),
			member("crew", jsonv.Str(l.c.crew)),
			member("holds", holds),
			member("ts", jsonv.Num(float64(ts))),
		))
	}
}

// startReap is the arm's `_stream_reap`: a background `crew reap`, never
// awaited and never killed by the cleanup. One runs at a time; a cadence tick
// that lands while one is in flight is simply the tick that waits.
func (l *loop) startReap() {
	if l.c.reapEvery == 0 || l.reap.live() {
		return
	}
	l.flushReap()
	l.lastReap = l.o.Now().Unix()
	r, err := l.procs.startReap(l.cdir)
	if err != nil {
		return
	}
	l.reap = r
	// The arm's subshell did this part: wait for the reap, then turn its two
	// files into stream lines. done closes only once they are written, so a
	// flush that sees the child gone never reads a half-written pair.
	go func() {
		defer close(r.done)
		r.code = waitStatus(r.cmd.Wait())
		r.ts = l.nowMS()
		l.writeReapLines(r)
	}()
}

// writeReapLines turns one finished reap's two files into its stream lines: one
// `reap` line when it said anything, and an `error` line prefixed `reap:` when
// it did not exit clean. It rewrites the child's own file, which the flush
// below reads and prints.
func (l *loop) writeReapLines(r *reapRun) {
	out := strings.TrimRight(readFile(r.out), "\n")
	var lines []jsonv.Value
	if out != "" {
		lines = append(lines, jsonv.Object(
			member("stream", jsonv.Str("reap")),
			member("crew", jsonv.Str(l.c.crew)),
			member("lines", reapLines(out)),
			member("ts", jsonv.Num(float64(r.ts))),
		))
	}
	if r.code != 0 {
		lines = append(lines, jsonv.Object(
			member("stream", jsonv.Str("error")),
			member("crew", jsonv.Str(l.c.crew)),
			member("rc", jsonv.Num(float64(r.code))),
			member("detail", jsonv.Str("reap: "+firstLine(r.errf))),
			member("ts", jsonv.Num(float64(r.ts))),
		))
	}
	var b strings.Builder
	for _, v := range lines {
		b.WriteString(encode(v) + "\n")
	}
	_ = os.WriteFile(r.out, []byte(b.String()), 0o644)
}

// flushReap prints and drops the tracked reap child's pair once it has exited;
// a running child may still be writing, and another stream's outlived child
// writes a pair this one never reads.
func (l *loop) flushReap() {
	if l.reap == nil || l.reap.live() {
		return
	}
	l.printReap(l.reap.out)
	_ = os.Remove(l.reap.out)
	_ = os.Remove(l.reap.errf)
	l.reap = nil
}

// printReap prints and empties one finished reap's lines. Its errors share the
// suppression scheme of the inner-watch errors: one line per key, re-emitted
// after --heartbeat.
func (l *loop) printReap(path string) {
	text, ok := readIfNonEmpty(path)
	if !ok {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if v, err := decodeOne(line); err == nil && fieldText(v, "stream") == "error" {
			key := digitsToN(fieldText(v, "rc") + ":" + fieldText(v, "detail"))
			if !l.sup.repeat("reaperr", key, l.nowMS(), l.heartbeatMS()) {
				continue
			}
		}
		say(l.stdout, "%s\n", line)
	}
	truncate(path)
}

// cleanup is the arm's EXIT trap: stop the child, drain what it printed but
// this process never got to, take the temp files and the lock with you, and
// leave the way its closing `exit 0` did.
func (l *loop) cleanup() {
	// TERM (not KILL): the inner watch's own handler releases watch.lock.d, and
	// it is normally parked in `sleep`, so the wait is what bounds this —
	// draining without it reads stream.out empty or partial for a batch whose
	// cursor had already advanced.
	if l.child != nil {
		l.child.stop()
	}
	// Drain before delete: stream.out is what an orphaned-then-reaped child
	// wrote but this process never got to read.
	if text, ok := readIfNonEmpty(l.outf); ok {
		// With the newline: `readIfNonEmpty` strips it as command substitution
		// does, and the arm's `printf '%s\n'`/`cat` always ended the line.
		say(l.stdout, "%s\n", text)
	}
	if l.reap != nil && !l.reap.live() {
		l.printReap(l.reap.out)
	}
	_ = os.Remove(l.outf)
	_ = os.Remove(l.errf)
	_ = os.Remove(l.holderr)
	sweepReaped(l.reapout, l.reaperr)
	lock.Release(l.lockd)
}

// wait is one of the loop's two real sleeps, interruptible by a caught signal.
func (l *loop) wait(d time.Duration, sigs chan os.Signal) os.Signal {
	select {
	case s := <-sigs:
		return s
	case <-l.o.Timer(d):
		return nil
	}
}

// writeTick is the tick `--status` measures: this pid, this park, this instant.
func (l *loop) writeTick() {
	tick := encode(jsonv.Object(
		member("pid", jsonv.Num(float64(os.Getpid()))),
		member("ts", jsonv.Num(float64(l.nowMS()))),
		member("park", jsonv.Num(float64(l.c.park))),
	))
	_ = os.WriteFile(filepath.Join(l.cdir, "stream.tick"), []byte(tick+"\n"), 0o644)
}

func (l *loop) emit(line jsonv.Value) { say(l.stdout, "%s\n", encode(line)) }

func (l *loop) nowMS() int64 { return clockMS(l.o) }

func (l *loop) heartbeatMS() int64 { return l.c.heartbeat * 1000 }

// pending is a non-blocking look at the signal channel: the loop acts on a
// caught signal at a cycle boundary, never mid-fold.
func pending(sigs chan os.Signal) bool {
	select {
	case <-sigs:
		return true
	default:
		return false
	}
}

// wantsReap is the arm's `jq -e -s` over the batch it just printed.
func wantsReap(batch string) bool {
	rows, err := decodeLines(batch)
	if err != nil {
		return false
	}
	out, err := jqrun.Run(wantsReapProg, rows, 0, nil)
	if err != nil {
		return false
	}
	return out.Truthy()
}

// reapLines is the arm's `split("\n") | map(select(length>0) | sub("^crew reap: ";""))`.
func reapLines(out string) jsonv.Value {
	var vs []jsonv.Value
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		vs = append(vs, jsonv.Str(strings.TrimPrefix(line, "crew reap: ")))
	}
	return jsonv.Array(vs...)
}

// holdIDs is the arm's `[.[].id] | sort | join(",")`: the matured set, keyed on
// its ids.
func holdIDs(holds jsonv.Value) string {
	var ids []string
	for _, h := range holds.Elems() {
		id, _ := h.Get("id")
		s, _ := id.AsString()
		ids = append(ids, s)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// sweepReaped removes the reap pair base names and every per-child pair, the
// stream's own and any orphan a previous child left behind. A child still
// running keeps its own pair open and can recreate nothing: the names it writes
// to are its own.
func sweepReaped(out, errf string) {
	for _, base := range []string{out, errf} {
		_ = os.Remove(base)
		matches, _ := filepath.Glob(base + ".*")
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

func clockMS(o Options) int64 {
	now := o.Now()
	return now.UnixMilli()
}
