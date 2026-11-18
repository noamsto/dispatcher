// Command crew-dash is a read-only dashboard over the layered dispatcher
// settings, per-engine budget, and the last few runs' retro notes and
// ratings. It probes nothing itself: every figure comes from a cached or
// already-run source (dispatch-config, refresh-budget's cache, crew retro/
// rate/crews/roster/hold list).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/noamsto/dispatcher/dash/internal/buswatch"
	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/kitty"
	"github.com/noamsto/dispatcher/dash/internal/once"
	"github.com/noamsto/dispatcher/dash/internal/ui"
	"github.com/noamsto/dispatcher/dash/internal/view"
)

// dispatchConfigBin is baked at build time via -ldflags -X
// main.dispatchConfigBin=…; empty in a raw `go build`.
var dispatchConfigBin string

func main() {
	os.Exit(run())
}

func run() int {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "crew dash: %s\n%s", err, usage())
		return 2
	}
	mode := o.mode
	if mode == "" {
		mode = "once"
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			mode = "interactive"
		}
	}

	cfg := resolveConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collect := func() data.Snapshot {
		return data.Collect(ctx, data.ExecRunner{}, cfg)
	}

	switch mode {
	case "json":
		if o.view != "" {
			return runView(ctx, cfg, o)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(collect()); err != nil {
			fmt.Fprintln(os.Stderr, "crew dash: "+err.Error())
			return 1
		}
		return 0
	case "once":
		fmt.Print(once.Render(collect(), colorEnabled()))
		return 0
	case "interactive":
		eventsPath, eventsErr := data.EventsPath(ctx, data.ExecRunner{}, cfg.Git)
		runner := data.ExecRunner{}
		scanner := data.NewBusScanner(eventsPath)
		mapDeps, closeMap := resolveMap(runner)
		defer closeMap()
		fleet := newFleetCollector(runner, cfg)
		fleet.background = true
		deps := ui.Deps{
			Ctx: ctx,
			Load: ui.Loaders{
				Settings: func(ctx context.Context) data.SettingsSection { return data.CollectSettings(ctx, runner, cfg) },
				Budget:   func(ctx context.Context) data.BudgetSection { return data.CollectBudget(ctx, runner, cfg) },
				Retro:    func(ctx context.Context) data.RunsSection { return data.CollectRetro(ctx, runner, cfg) },
				Ratings:  func(ctx context.Context) data.RunsSection { return data.CollectRatings(ctx, runner, cfg) },
				Roster:   func(ctx context.Context) data.RosterSection { return data.CollectRoster(ctx, runner, cfg) },
				Bus: func(context.Context) data.BusDigest {
					return busDigest(scanner, eventsErr, time.Now().UnixMilli())
				},
				ReapKeeps: func() map[data.WorkerKey]string { return fleet.reapKeeps(ctx) },
				Fleet: func(ctx context.Context, emit func(view.FleetInputs)) view.FleetInputs {
					return fleet.stream(ctx, time.Now().Unix(), emit)
				},
			},
			Runner:          runner,
			Crew:            cfg.Crew,
			InTmux:          os.Getenv("TMUX") != "",
			EventsPath:      eventsPath,
			EventsPathErr:   eventsErr,
			Now:             cfg.Now,
			Narrow:          o.narrow,
			Map:             mapDeps,
			DispatchConfig:  cfg.DispatchConfigBin,
			FleetWatchPaths: func() []string { return watchPaths(fleet.knownCommons()) },
		}
		if err := ui.Run(deps); err != nil {
			fmt.Fprintln(os.Stderr, "crew dash: "+err.Error())
			return 1
		}
		return 0
	}
	return 0
}

// resolveMap builds the Map tab's terminal seams. It runs before bubbletea
// owns the terminal because the capability probe shells out to tmux and the
// cell-size query reads the terminal's reply from the tty.
func resolveMap(runner data.Runner) (ui.MapDeps, func()) {
	notCapable := func(why string) ui.MapDeps {
		return ui.MapDeps{Capable: func() (bool, string) { return false, why }}
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return notCapable("no controlling tty"), func() {}
	}
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, _, err := runner.Run(ctx, name, args...)
		return out, err
	}
	if ok, why := kitty.Capable(os.Getenv, run); !ok {
		_ = tty.Close()
		return notCapable(why), func() {}
	}
	png, err := os.CreateTemp("", "crew-dash-map-*.png")
	if err != nil {
		_ = tty.Close()
		return notCapable("cannot create the map image file: " + err.Error()), func() {}
	}
	_ = png.Close()

	cellW, cellH := kitty.FallbackCellW, kitty.FallbackCellH
	if query, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		cellW, cellH = kitty.QueryCellPx(query)
	}
	host, _ := os.Hostname()
	return ui.MapDeps{
			TTY:     tty,
			CellPx:  func() (int, int) { return cellW, cellH },
			Capable: func() (bool, string) { return true, "" },
			OutPath: png.Name(),
			IDBase:  kitty.ID(host, os.Getenv("TMUX_PANE")),
		}, func() {
			_ = tty.Close()
			_ = os.Remove(png.Name())
		}
}

// runView prints one view: indented once, or one compact line per change
// with --watch. One BusScanner (or FleetScanners for fleet) serves every
// collect so later scans only read appended lines.
func runView(ctx context.Context, cfg data.Config, o opts) int {
	runner := data.ExecRunner{}
	eventsPath, eventsErr := data.EventsPath(ctx, runner, cfg.Git)
	var collect func(context.Context) (any, error)
	var commons func() []string
	if o.view == "fleet" {
		fc := newFleetCollector(runner, cfg)
		fc.background = o.watch
		commons = fc.knownCommons
		collect = func(ctx context.Context) (any, error) {
			if o.keeps {
				fc.blockReapKeeps(ctx)
			}
			now := cfg.Now
			if o.watch {
				now = resolveNow()
			}
			return view.BuildFleet(fc.collect(ctx, now)), nil
		}
	} else {
		scanner := data.NewBusScanner(eventsPath)
		var fc *fleetCollector
		if o.keeps {
			fc = newFleetCollector(runner, cfg)
		}
		if eventsErr == nil {
			repo := filepath.Dir(filepath.Dir(eventsPath))
			commons = func() []string { return []string{repo} }
		}
		collect = func(ctx context.Context) (any, error) {
			cfg := cfg
			if o.watch {
				cfg.Now = resolveNow()
			}
			nowMS := cfg.Now * 1000
			var keeps map[data.WorkerKey]string
			if fc != nil {
				fc.blockReapKeeps(ctx)
				keeps = fc.reapKeeps(ctx)
			}
			return view.Build(o.view, view.Inputs{
				Snap:      data.Collect(ctx, runner, cfg),
				Bus:       busDigest(scanner, eventsErr, nowMS),
				Loaded:    view.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
				NowMS:     nowMS,
				Crew:      cfg.Crew,
				ReapKeeps: keeps,
			})
		}
	}

	if !o.watch {
		v, err := collect(ctx)
		if err == nil {
			enc := json.NewEncoder(os.Stdout)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			err = enc.Encode(v)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "crew dash: "+err.Error())
			return 1
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The poll covers every events.jsonl, relay cursor and peer file; the
	// fsnotify watcher adds low latency for the current repo's bus. Fleet
	// has no single bus to watch.
	watchers := []buswatch.Watcher{buswatch.NewPoll(func() []string {
		if commons == nil {
			return nil
		}
		return watchPaths(commons())
	}, time.Second)}
	if o.view == "fleet" {
		// Until a collect learns the repo commons nothing is polled, so a
		// slow tick lets a consumer recover from a failed first collect.
		watchers = append(watchers, buswatch.NewTicker(fleetWatchFallback))
	} else if eventsErr == nil {
		watchers = append(watchers, buswatch.New(eventsPath))
	}
	w := buswatch.Merge(watchers...)
	defer func() { _ = w.Close() }()
	if err := runWatch(ctx, os.Stdout, w.Events(), collect, 200*time.Millisecond); err != nil {
		fmt.Fprintln(os.Stderr, "crew dash: "+err.Error())
		return 1
	}
	return 0
}

const fleetWatchFallback = 30 * time.Second

// watchPaths lists the change sources of each repo common as poll patterns:
// the bus, relay cursors (a relayed remote-worker batch moves its cursor
// without touching events.jsonl) and peer sync files.
func watchPaths(commons []string) []string {
	var paths []string
	for _, common := range commons {
		crew := filepath.Join(common, "crew")
		paths = append(paths,
			filepath.Join(crew, "events.jsonl"),
			filepath.Join(crew, "crews", "*", "relay", "*.cursor"),
			filepath.Join(crew, "peers", "*.link"),
			filepath.Join(crew, "peers", "*.roster"),
		)
	}
	return paths
}

// fleetCollector gathers the fleet view's inputs. One instance serves every
// collect: scanners and the host prober keep state across them, and commons
// records which repos the watcher should poll.
type fleetCollector struct {
	runner   data.Runner
	cfg      data.Config
	scanners *data.FleetScanners
	prober   *data.HostProber

	mu      sync.Mutex
	commons []string
	last    *view.FleetInputs

	keeps keepCache
	// background lets stream start keep refreshes; a one-shot process would
	// orphan the probe when it exits.
	background bool
}

const (
	reapKeepsTTL   = 10 * time.Minute
	reapKeepsRetry = 30 * time.Second
)

// keepCache holds the batched `crew reap --dry-run` answer for a TTL. The
// probe is slow, so readers never wait on it: cached starts at most one
// background refresh and returns what it has, and only fetchNow blocks. A
// failed (nil) fetch keeps the last good value and is retried no sooner than
// reapKeepsRetry.
type keepCache struct {
	mu       sync.Mutex
	val      map[data.WorkerKey]string
	at       time.Time // last successful fetch
	tried    time.Time // last attempt
	inflight chan struct{}
}

// begin claims the refresh when the value is stale, none is in flight and the
// retry spacing allows; the caller then runs fetch and calls finish. wait is
// non-nil when another refresh is in flight.
func (k *keepCache) begin(now time.Time, force bool) (val map[data.WorkerKey]string, done chan struct{}, wait chan struct{}) {
	k.mu.Lock()
	defer k.mu.Unlock()
	val = k.val
	if k.inflight != nil {
		return val, nil, k.inflight
	}
	fresh := !k.at.IsZero() && now.Sub(k.at) < reapKeepsTTL
	backoff := !k.tried.IsZero() && now.Sub(k.tried) < reapKeepsRetry
	if fresh || (backoff && !force) {
		return val, nil, nil
	}
	k.tried = now
	k.inflight = make(chan struct{})
	return val, k.inflight, nil
}

func (k *keepCache) finish(now time.Time, v map[data.WorkerKey]string, done chan struct{}) map[data.WorkerKey]string {
	k.mu.Lock()
	if v != nil {
		k.val, k.at = v, now
	}
	v = k.val
	k.inflight = nil
	k.mu.Unlock()
	close(done)
	return v
}

func (k *keepCache) cached(now func() time.Time, fetch func() map[data.WorkerKey]string) map[data.WorkerKey]string {
	val, done, _ := k.begin(now(), false)
	if done == nil {
		return val
	}
	go func() { k.finish(now(), fetch(), done) }()
	return val
}

// fetchNow blocks for one fetch unless the value is fresh, joining a refresh
// already in flight; it returns nil when ctx ends.
func (k *keepCache) fetchNow(ctx context.Context, now func() time.Time, fetch func() map[data.WorkerKey]string) map[data.WorkerKey]string {
	val, done, wait := k.begin(now(), true)
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.val
	}
	if done == nil {
		return val
	}
	return k.finish(now(), fetch(), done)
}

// keepsFetch is the probe bound to this collector. It outlives the stream
// that started it, so it ignores that stream's cancellation.
func (fc *fleetCollector) keepsFetch(ctx context.Context) func() map[data.WorkerKey]string {
	ctx = context.WithoutCancel(ctx)
	return func() map[data.WorkerKey]string {
		return data.CollectReapKeeps(ctx, fc.runner, fc.cfg)
	}
}

// reapKeeps reads the cached keeps, starting a background refresh when stale.
func (fc *fleetCollector) reapKeeps(ctx context.Context) map[data.WorkerKey]string {
	return fc.keeps.cached(time.Now, fc.keepsFetch(ctx))
}

// blockReapKeeps waits for one fresh probe; one-shot JSON views opt in.
func (fc *fleetCollector) blockReapKeeps(ctx context.Context) {
	fc.keeps.fetchNow(ctx, time.Now, fc.keepsFetch(ctx))
}

func newFleetCollector(r data.Runner, cfg data.Config) *fleetCollector {
	return &fleetCollector{
		runner:   r,
		cfg:      cfg,
		scanners: data.NewFleetScanners(),
		prober:   data.NewHostProber(r, time.Now),
	}
}

func (fc *fleetCollector) knownCommons() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return slices.Clone(fc.commons)
}

// collect is stream without the partials: the JSON and watch paths want
// only the final inputs.
func (fc *fleetCollector) collect(ctx context.Context, nowSec int64) view.FleetInputs {
	return fc.stream(ctx, nowSec, func(view.FleetInputs) {})
}

// stream gathers the fleet inputs and emits the accumulated value as each
// source lands, so a consumer can paint before the slowest one returns. The
// first emit precedes any subprocess: the previous final result when there is
// one (each source then overwrites its fields as it lands), else an empty
// skeleton with every source pending. Settings, budget,
// fleet status and fleet queue start together; reach waits only for settings
// (the tailscale flag and host list), and the bus scans only for status. emit
// runs under the accumulator lock, so it must not block. The returned final
// equals what the sources produce when gathered serially.
func (fc *fleetCollector) stream(ctx context.Context, nowSec int64, emit func(view.FleetInputs)) view.FleetInputs {
	cfg := fc.cfg
	cfg.Now = nowSec
	nowMS := nowSec * 1000

	fc.mu.Lock()
	prev := fc.last
	fc.mu.Unlock()
	acc := view.FleetInputs{
		Fleet:   data.FleetSection{Queue: []data.FleetQueueHold{}, Hosts: []data.HostConfig{}, Reach: []data.HostReach{}, Suggested: []data.SuggestedHost{}},
		Snap:    data.Snapshot{Now: nowSec},
		Loaded:  view.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
		NowMS:   nowMS,
		Crew:    cfg.Crew,
		Pending: view.FleetPending{Status: true, Queue: true, Reach: true, Hosts: true},
	}
	if prev != nil {
		// Stale data stays visible while revalidating. Every source assigns
		// whole fields, so nothing shared with the emitted *prev is mutated.
		acc = *prev
		acc.Snap.Now = nowSec
		acc.NowMS = nowMS
		acc.Crew = cfg.Crew
		acc.Pending = view.FleetPending{}
	}
	fc.keeps.mu.Lock()
	acc.ReapKeeps = fc.keeps.val
	fc.keeps.mu.Unlock()
	emit(acc)
	// Started after the first emit so no subprocess precedes it; a stale value
	// is replaced for the next stream.
	if fc.background {
		fc.reapKeeps(ctx)
	}

	var mu sync.Mutex
	update := func(apply func(*view.FleetInputs), done func(*view.FleetPending)) {
		mu.Lock()
		defer mu.Unlock()
		apply(&acc)
		done(&acc.Pending)
		emit(acc)
	}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		settings := data.CollectSettings(ctx, fc.runner, cfg)
		hosts := data.HostsFromSettings(settings.Rows)
		update(func(in *view.FleetInputs) {
			in.Snap.Settings = settings
			in.Fleet.Hosts = hosts
		}, func(p *view.FleetPending) { p.Hosts = false })
		reach, suggested, tsErr := data.CollectReach(ctx, fc.runner, data.TailscaleEnabled(settings.Rows), hosts, fc.prober)
		update(func(in *view.FleetInputs) {
			in.Fleet.Reach, in.Fleet.Suggested, in.Fleet.TailscaleError = reach, suggested, tsErr
		}, func(p *view.FleetPending) { p.Reach = false })
	}()
	go func() {
		defer wg.Done()
		budget := data.CollectBudget(ctx, fc.runner, cfg)
		update(func(in *view.FleetInputs) { in.Snap.Budget = budget }, func(*view.FleetPending) {})
	}()
	go func() {
		defer wg.Done()
		st, stErr := data.CollectFleetStatus(ctx, fc.runner, cfg)
		sec := data.FleetSection{Status: st}
		digests, digestErrs := fc.scanners.Scan(view.ActiveCommons(sec), nowMS)

		// A failed fleet status keeps the last known commons so the watcher
		// still polls the repos it knew.
		if st != nil {
			var all []string
			for _, repo := range st.Repos {
				if !slices.Contains(all, repo.Common) {
					all = append(all, repo.Common)
				}
			}
			fc.mu.Lock()
			fc.commons = all
			fc.mu.Unlock()
		}
		update(func(in *view.FleetInputs) {
			in.Fleet.Status, in.Fleet.StatusError = st, stErr
			in.Digests, in.DigestErrs = digests, digestErrs
		}, func(p *view.FleetPending) { p.Status = false })
	}()
	go func() {
		defer wg.Done()
		queue, queueErr := data.CollectFleetQueue(ctx, fc.runner, cfg)
		update(func(in *view.FleetInputs) {
			in.Fleet.Queue, in.Fleet.QueueError = queue, queueErr
		}, func(p *view.FleetPending) { p.Queue = false })
	}()
	wg.Wait()

	if ctx.Err() == nil {
		fc.mu.Lock()
		fc.last = &acc
		fc.mu.Unlock()
	}
	return acc
}

// busDigest scans the bus, or returns an empty digest carrying the error so
// the view lists it instead of aborting.
func busDigest(s *data.BusScanner, pathErr error, nowMS int64) data.BusDigest {
	err := pathErr
	if err == nil {
		var d data.BusDigest
		if d, err = s.Scan(nowMS); err == nil {
			return d
		}
	}
	msg := err.Error()
	return data.BusDigest{Err: &msg}
}

type opts struct {
	mode, view string
	watch      bool
	keeps      bool // one-shot --json probes crew reap for keep reasons
	narrow     bool // force the stacked narrow layout in the interactive UI
}

func usage() string {
	return "usage: crew dash [--narrow | --once | --json [--view <name> [--watch] [--keeps]]]\n" +
		"views: " + strings.Join(view.Names, ", ") + "\n"
}

// parseArgs: no args leaves mode empty (decided by the caller from the
// ttys), --once/--json pick a mode, --view needs --json and --watch needs
// --view.
func parseArgs(args []string) (opts, error) {
	var o opts
	var onceFlag, jsonFlag bool
	for i := 0; i < len(args); i++ {
		var seen *bool
		switch args[i] {
		case "--once":
			seen = &onceFlag
		case "--json":
			seen = &jsonFlag
		case "--watch":
			seen = &o.watch
		case "--keeps":
			seen = &o.keeps
		case "--narrow":
			seen = &o.narrow
		case "--view":
			if o.view != "" {
				return opts{}, errors.New("--view given twice")
			}
			if i+1 == len(args) {
				return opts{}, errors.New("--view needs a name")
			}
			i++
			o.view = args[i]
			if !slices.Contains(view.Names, o.view) {
				return opts{}, fmt.Errorf("unknown view %q (valid: %s)", o.view, strings.Join(view.Names, ", "))
			}
			continue
		default:
			return opts{}, fmt.Errorf("unknown argument %q", args[i])
		}
		if *seen {
			return opts{}, fmt.Errorf("%s given twice", args[i])
		}
		*seen = true
	}

	switch {
	case onceFlag && (jsonFlag || o.view != "" || o.watch || o.keeps || o.narrow):
		return opts{}, errors.New("--once takes no other flags")
	case o.narrow && (jsonFlag || o.view != "" || o.watch || o.keeps):
		return opts{}, errors.New("--narrow takes no other flags")
	case o.view != "" && !jsonFlag:
		return opts{}, errors.New("--view needs --json")
	case o.watch && o.view == "":
		return opts{}, errors.New("--watch needs --view")
	case o.keeps && o.watch:
		return opts{}, errors.New("--keeps does not combine with --watch")
	case o.keeps && (!jsonFlag || (o.view != "fleet" && o.view != "overview")):
		return opts{}, errors.New("--keeps needs --json --view fleet or overview")
	case onceFlag:
		o.mode = "once"
	case jsonFlag:
		o.mode = "json"
	}
	return o, nil
}

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch os.Getenv("CREW_DASH_COLOR") {
	case "never":
		return false
	case "always":
		return true
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func resolveConfig() data.Config {
	return data.Config{
		Crew:              resolveCrew(),
		DispatchConfigBin: resolveDispatchConfig(),
		RefreshBudget:     "refresh-budget",
		Git:               "git",
		Now:               resolveNow(),
	}
}

func resolveNow() int64 {
	if v := os.Getenv("CREW_DASH_NOW"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return time.Now().Unix()
}

// resolveCrew resolves the crew binary to exec: $CREW_BIN executable → exec
// it directly; set but not executable → run it through bash; unset → "crew"
// on PATH.
func resolveCrew() []string {
	bin := os.Getenv("CREW_BIN")
	if bin == "" {
		return []string{"crew"}
	}
	// Actions run in another repo's directory, so a relative path must be
	// pinned to the dash's own cwd first.
	if abs, err := filepath.Abs(bin); err == nil {
		bin = abs
	}
	if isExecutable(bin) {
		return []string{bin}
	}
	return []string{"bash", "-euo", "pipefail", bin}
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0o111 != 0
}

func resolveDispatchConfig() string {
	if v := os.Getenv("DISPATCH_CONFIG_BIN"); v != "" {
		return v
	}
	if dispatchConfigBin != "" {
		return dispatchConfigBin
	}
	return "dispatch-config"
}
