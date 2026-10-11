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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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
	// Origin is a git common dir's origin URL, which labels its repo. nil labels
	// every repo by its directory.
	Origin func(common string) string
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
	if o.Origin == nil {
		o.Origin = func(string) string { return "" }
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

// RunFleet is `crew fleet stream`. It takes `crew stream`'s flags except
// --reap-every, plus --rediscover, and --crew is required: the lane spans
// repos, so no one repo's ambient crew can stand in for it.
func RunFleet(argv []string, stdout, stderr io.Writer, opts FleetOptions) int {
	o := opts.withDefaults()
	rest, rediscover, msg := fleetFlags(argv)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitUsage
	}
	po := o.Options
	po.CrewID = func() string { return "" }
	c, msg := parse(rest, po)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitUsage
	}
	c.reapEvery = 0

	// Each bus loop's child runs in its repo's git common dir, so a relative
	// script path would resolve there instead of where it was given.
	if strings.Contains(o.Self, "/") {
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

	// As in Run: a handler armed before the lock is held would release the
	// incumbent's lock on the refusal path.
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

// fleetFlags takes what only the fleet reads — --rediscover, the required
// --crew, the refused --reap-every — and leaves the rest, --crew included, to
// parse.
func fleetFlags(argv []string) (rest []string, rediscover int64, msg string) {
	var crew string
	rediscoverText := strconv.Itoa(defaultRediscover)
	for i := 0; i < len(argv); i++ {
		switch flag := argv[i]; flag {
		case "--force", "--status":
			rest = append(rest, flag)
			continue
		case "--reap-every":
			return nil, 0, "crew: fleet stream: --reap-every is not supported — the fleet lane does not reap (each repo's own stream or dispatch does)"
		case "--crew", "--states", "--park", "--heartbeat", "--coalesce", "--retry", "--interval", "--rediscover":
		default:
			return nil, 0, fmt.Sprintf("crew: fleet stream: unknown arg '%s'", flag)
		}
		if i+1 >= len(argv) || argv[i+1] == "" {
			return nil, 0, fmt.Sprintf("crew: %s needs a value", argv[i])
		}
		flag, value := argv[i], argv[i+1]
		i++
		switch flag {
		case "--rediscover":
			rediscoverText = value
			continue
		case "--crew":
			crew = value
		}
		rest = append(rest, flag, value)
	}
	rediscover, ok := positiveInt(rediscoverText)
	if !ok {
		return nil, 0, "crew: --rediscover must be a positive integer number of seconds"
	}
	if crew == "" {
		return nil, 0, "crew: fleet stream: --crew ID is required"
	}
	return rest, rediscover, ""
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
			say(f.out, "%s\n", encode(jsonv.Object(
				member("stream", jsonv.Str("heartbeat")),
				member("crew", jsonv.Str(f.c.crew)),
				member("quiet_s", jsonv.Num(float64(int64(quiet/time.Second)))),
				member("ts", jsonv.Num(float64(clockMS(f.o.Options)))),
			)))
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
// repo that is gone or no longer listed as a bus, and a join for each bus that has gained a row for the crew.
// A bus with none is never watched, so no cursor or crew dir is created in a
// repo the crew never touched.
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
	listed := make(map[string]bool, len(found))
	for _, r := range found {
		listed[r.Common] = r.Status == repos.StatusOK
	}
	for common := range f.buses {
		if !listed[common] {
			f.leave(common)
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
	repo := repos.Name(common, f.o.Origin(common))
	l := &loop{
		c:       f.c,
		o:       f.o.Options,
		cdir:    scratch,
		stdout:  f.out,
		procs:   children{bash: f.o.Bash, self: f.o.Self, dir: common},
		sup:     &suppress{key: map[string]string{}, ts: map[string]int64{}},
		outf:    filepath.Join(scratch, "stream.out"),
		errf:    filepath.Join(scratch, "stream.err"),
		holderr: filepath.Join(scratch, "stream.hold.err"),
		reapout: filepath.Join(scratch, "stream.reap.out"),
		reaperr: filepath.Join(scratch, "stream.reap.err"),
		repo:    repo,
		yield:   f.yielder(repo, filepath.Join(common, "crew", "crews", f.c.crew)),
	}
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
			say(f.stderr, "crew: fleet stream: %s: per-repo stream alive (pid %s) — not watching\n", repo, encode(pid))
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
