// `crew fleet stream` is the per-repo lane fanned out over every bus on the
// machine that carries the crew: one bus loop per joined repo, each parking its
// own `crew watch` against that repo's per-crew cursor, all printing to one
// stdout with each batch tagged by its repo.
//
// The lane owns what is not per bus: the lock and tick `--status` reads (under
// the state dir, outside every repo), discovery, and a heartbeat with no repo.
// Holds and reaps stay with each repo's own stream or dispatch. A bus whose
// per-repo stream is alive is left to it, and `crew watch` holding
// `watch.lock.d` across its fold and cursor write is what keeps two lanes from
// delivering one event twice.
package stream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

const defaultRediscover = 300

// FleetOptions is everything RunFleet reads beyond Options.
type FleetOptions struct {
	Options
	// Discover lists the candidate buses, writing its own notes to stderr. nil
	// is repos.Discover over Env and Registry.
	Discover func(stderr io.Writer) []repos.Repo
	// Git reads a git common dir's origin URL, which labels its repo. nil labels
	// every repo by its directory.
	Git git.Client
	// LaneRoot holds one lane dir per crew. Empty is
	// ${XDG_STATE_HOME:-$HOME/.local/state}/crew/fleet-stream.
	LaneRoot string
	// Rediscover is the coordinator's tick. nil is Options.Timer; it is separate
	// so a test can drive discovery without stalling the bus loops' waits.
	Rediscover func(d time.Duration) <-chan time.Time
	Env        func(string) string
	Registry   string
}

func (o FleetOptions) withDefaults() FleetOptions {
	o.Options = o.Options.withDefaults()
	if o.Env == nil {
		o.Env = os.Getenv
	}
	if o.Discover == nil {
		registry, env := o.Registry, o.Env
		o.Discover = func(stderr io.Writer) []repos.Repo {
			return repos.Discover(repos.Options{Registry: registry, Env: env, Stderr: stderr, Prefix: "crew: fleet stream"})
		}
	}
	if o.LaneRoot == "" {
		base := o.Env("XDG_STATE_HOME")
		if base == "" {
			base = o.Env("HOME") + "/.local/state"
		}
		o.LaneRoot = filepath.Join(base, "crew", "fleet-stream")
	}
	if o.Rediscover == nil {
		o.Rediscover = o.Timer
	}
	return o
}

// FleetRequest is one `crew fleet stream` call: `crew stream`'s request, whose
// ReapEvery the lane ignores, plus --rediscover ("" for its default).
type FleetRequest struct {
	Request
	Rediscover string
}

// RunFleet is `crew fleet stream`. --crew is required: the lane spans repos,
// so no one repo's ambient crew can stand in for it.
func RunFleet(r FleetRequest, stdout, stderr io.Writer, opts FleetOptions) int {
	o := opts.withDefaults()
	rediscoverText := r.Rediscover
	if rediscoverText == "" {
		rediscoverText = strconv.Itoa(defaultRediscover)
	}
	rediscover, ok := positiveInt(rediscoverText)
	if !ok {
		say(stderr, "crew: --rediscover must be a positive integer number of seconds\n")
		return exitUsage
	}
	if r.Crew == "" {
		say(stderr, "crew: fleet stream: --crew ID is required\n")
		return exitUsage
	}
	c, msg := check(r.Request, o.Options)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitUsage
	}
	c.reapEvery = 0

	// Each bus loop's child runs in its repo's git common dir, so a relative
	// script path would resolve there instead of where it was given. bash runs
	// a bare name from the cwd before searching PATH, so one that exists here
	// is relative too.
	if !filepath.IsAbs(o.Self) && (strings.Contains(o.Self, "/") || filekind.IsRegular(o.Self)) {
		self, err := filepath.Abs(o.Self)
		if err != nil {
			say(stderr, "crew: %v\n", err)
			return 1
		}
		o.Self = self
	}

	lane := filepath.Join(o.LaneRoot, c.crew)
	if c.statusMode {
		return status(lane, c.crew, stdout, o.Options)
	}
	if err := os.MkdirAll(lane, 0o755); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}
	lockd := filepath.Join(lane, "stream.lock.d")
	if !acquire(lockd, c, o.Options, stderr) {
		return 1
	}

	// As in Run: signals are caught only once the lock is held.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	tick := min(c.park, rediscover)
	tickCall := c
	tickCall.park = tick
	f := &fleet{
		c:          c,
		o:          o,
		lane:       lane,
		lockd:      lockd,
		out:        &laneWriter{w: stdout, now: o.Now, last: o.Now()},
		stderr:     &laneWriter{w: stderr, now: o.Now, last: o.Now()},
		rediscover: time.Duration(rediscover) * time.Second,
		tick:       time.Duration(tick) * time.Second,
		ticker:     &loop{c: tickCall, o: o.Options, cdir: lane},
		buses:      map[string]*busLoop{},
		noted:      map[string]string{},
		said:       map[string]bool{},
	}
	return f.run(sigs)
}

// fleet is the coordinator: the only goroutine that discovers, joins and stops
// bus loops, so buses, noted and said need no lock.
type fleet struct {
	c          call
	o          FleetOptions
	lane       string
	lockd      string
	out        *laneWriter
	stderr     *laneWriter
	rediscover time.Duration
	tick       time.Duration
	// ticker writes the lane's stream.tick, with the coordinator's tick as its
	// park so `--status` judges it by the cadence it is actually written at.
	ticker *loop

	buses        map[string]*busLoop
	wg           sync.WaitGroup
	noted        map[string]string
	said         map[string]bool
	lastDiscover time.Time
	discovered   bool
}

// busLoop is one joined bus's running loop, keyed by its git common dir.
type busLoop struct {
	sigs    chan os.Signal
	done    chan struct{}
	scratch string
}

func (b *busLoop) signal(s os.Signal) {
	select {
	case b.sigs <- s:
	default:
	}
}

func (f *fleet) run(sigs chan os.Signal) int {
	for {
		f.ticker.writeTick()
		if now := f.o.Now(); !f.discovered || now.Sub(f.lastDiscover) >= f.rediscover {
			f.discovered, f.lastDiscover = true, now
			f.discover()
		}
		if quiet := f.out.quiet(); quiet >= time.Duration(f.c.heartbeat)*time.Second {
			say(f.out, "%s\n", encode(heartbeatLine{
				Stream: "heartbeat",
				Crew:   f.c.crew,
				QuietS: int64(quiet / time.Second),
				TS:     clockMS(f.o.Options),
			}))
		}
		select {
		case s := <-sigs:
			return f.shutdown(s)
		case <-f.o.Rediscover(f.tick):
		}
	}
}

// shutdown stops every bus loop — each TERMs its watch and drains what it
// printed — and only then gives up the lane.
func (f *fleet) shutdown(s os.Signal) int {
	for _, b := range f.buses {
		b.signal(s)
	}
	f.wg.Wait()
	_ = os.RemoveAll(filepath.Join(f.lane, "buses"))
	lock.Release(f.lockd)
	return 0
}

// discover is one pass over the candidates: skip notes, a loop stopped for a
// repo that is gone or no longer listed as a bus, and a join for each bus that
// has gained a row for the crew. A bus with none is never watched, so no cursor
// or crew dir is created in a repo the crew never touched.
func (f *fleet) discover() {
	var notes bytes.Buffer
	found := f.o.Discover(&notes)
	for _, line := range strings.SplitAfter(notes.String(), "\n") {
		if line == "" || f.said[line] {
			continue
		}
		f.said[line] = true
		say(f.stderr, "%s", line)
	}

	// A repo deleted under a sweep root just drops out of discovery, with no
	// gone row to say so.
	listed := make(map[string]repos.Status, len(found))
	for _, r := range found {
		listed[r.Common] = r.Status
	}
	for common := range f.buses {
		status, present := listed[common]
		if status == repos.StatusOK {
			continue
		}
		f.leave(common)
		if !present {
			f.skip(common, "no longer discovered")
		}
	}

	for _, r := range found {
		if r.Status != repos.StatusOK {
			f.skip(r.Common, string(r.Status))
			continue
		}
		if f.buses[r.Common] != nil {
			delete(f.noted, r.Common)
			continue
		}
		events, err := bus.ReadEventsTolerant(r.Common + "/crew/events.jsonl")
		var decode *bus.DecodeError
		switch {
		case errors.Is(err, bus.ErrNoLog):
			f.skip(r.Common, string(repos.StatusNoBus))
			continue
		case err != nil && !errors.As(err, &decode):
			f.skip(r.Common, strings.Join(strings.Fields(err.Error()), " "))
			continue
		}
		delete(f.noted, r.Common)
		if hasCrew(events, f.c.crew) {
			f.join(r.Common)
		}
	}
}

// skip notes a repo once per reason: a 300 s rediscovery must not repeat it.
func (f *fleet) skip(common, why string) {
	if f.noted[common] == why {
		return
	}
	f.noted[common] = why
	say(f.stderr, "crew: fleet stream: %s: skipped (%s)\n", common, why)
}

func hasCrew(events []bus.Event, crew string) bool {
	for _, e := range events {
		if e.CrewID == crew {
			return true
		}
	}
	return false
}

// join starts the bus loop for common. Its child runs in the git common dir,
// which `crew watch` resolves to this repo's bus, and every scratch file it
// writes is in its own dir under the lane: an empty base would make the reap
// sweep glob `.*` in the process cwd.
func (f *fleet) join(common string) {
	scratch := busScratch(f.lane, common)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		f.skip(common, strings.Join(strings.Fields(err.Error()), " "))
		return
	}
	var origin string
	if f.o.Git != nil {
		origin, _ = git.Origin(context.Background(), f.o.Git, common)
	}
	repo := repos.Name(common, origin)
	l := &loop{
		c:       f.c,
		o:       f.o.Options,
		cdir:    scratch,
		stdout:  f.out,
		stderr:  f.stderr,
		procs:   children{self: f.o.Self, dir: common},
		sup:     &suppress{key: map[string]string{}, ts: map[string]int64{}},
		outf:    filepath.Join(scratch, "stream.out"),
		errf:    filepath.Join(scratch, "stream.err"),
		holderr: filepath.Join(scratch, "stream.hold.err"),
		reapout: filepath.Join(scratch, "stream.reap.out"),
		reaperr: filepath.Join(scratch, "stream.reap.err"),
		repo:    repo,
		yield:   f.yielder(repo, filepath.Join(common, "crew", "crews", f.c.crew)),
	}
	l.relays = newRelays(l, filepath.Join(common, "crew", "events.jsonl"), filepath.Join(common, "crew", "crews", f.c.crew))
	b := &busLoop{sigs: make(chan os.Signal, 1), done: make(chan struct{}), scratch: scratch}
	f.buses[common] = b
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer close(b.done)
		l.run(b.sigs)
	}()
}

// leave stops a departed repo's loop and waits for it, so a repo that comes back
// never shares a scratch dir with the loop it replaces.
func (f *fleet) leave(common string) {
	b := f.buses[common]
	if b == nil {
		return
	}
	b.signal(syscall.SIGTERM)
	<-b.done
	delete(f.buses, common)
	_ = os.RemoveAll(b.scratch)
}

// yielder is a bus loop's yield: true while the repo's own stream for the crew
// is alive by `--status`'s rule. A stale or dead one does not count, since a
// wedged stream must not strand events. Each transition is noted once.
func (f *fleet) yielder(repo, cdir string) func() bool {
	yielding := false
	return func() bool {
		state, _, pid, _ := laneState(cdir, f.o.Options)
		alive := state == "alive"
		switch {
		case alive && !yielding:
			say(f.stderr, "crew: fleet stream: %s: per-repo stream alive (pid %s) — not watching\n", repo, pidText(pid))
		case !alive && yielding:
			say(f.stderr, "crew: fleet stream: %s: per-repo stream no longer alive — resumed\n", repo)
		}
		yielding = alive
		return alive
	}
}

func busScratch(lane, common string) string {
	sum := sha256.Sum256([]byte(common))
	return filepath.Join(lane, "buses", hex.EncodeToString(sum[:8]))
}

// laneWriter is one stream shared by every bus loop. say writes a whole line per
// Write, so the mutex keeps lines whole; the last write's time is what the lane
// heartbeat measures quiet from.
type laneWriter struct {
	mu   sync.Mutex
	w    io.Writer
	now  func() time.Time
	last time.Time
}

func (w *laneWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = w.now()
	return w.w.Write(p)
}

func (w *laneWriter) quiet() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now().Sub(w.last)
}
