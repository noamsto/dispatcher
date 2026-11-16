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
	"slices"
	"strconv"
	"strings"
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
			},
			Runner:         runner,
			Crew:           cfg.Crew,
			InTmux:         os.Getenv("TMUX") != "",
			EventsPath:     eventsPath,
			EventsPathErr:  eventsErr,
			Now:            cfg.Now,
			Map:            mapDeps,
			DispatchConfig: cfg.DispatchConfigBin,
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

// runView prints one view: indented once, or one compact line per bus change
// with --watch. One BusScanner serves every collect so later scans only read
// appended lines.
func runView(ctx context.Context, cfg data.Config, o opts) int {
	runner := data.ExecRunner{}
	eventsPath, eventsErr := data.EventsPath(ctx, runner, cfg.Git)
	scanner := data.NewBusScanner(eventsPath)
	collect := func(ctx context.Context) (any, error) {
		cfg := cfg
		if o.watch {
			cfg.Now = resolveNow()
		}
		nowMS := cfg.Now * 1000
		return view.Build(o.view, view.Inputs{
			Snap:   data.Collect(ctx, runner, cfg),
			Bus:    busDigest(scanner, eventsErr, nowMS),
			Loaded: view.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
			NowMS:  nowMS,
			Crew:   cfg.Crew,
		})
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
	// Without a bus path nothing can change: a nil channel never fires.
	var changes <-chan struct{}
	if eventsErr == nil {
		w := buswatch.New(eventsPath)
		defer func() { _ = w.Close() }()
		changes = w.Events()
	}
	if err := runWatch(ctx, os.Stdout, changes, collect, 200*time.Millisecond); err != nil {
		fmt.Fprintln(os.Stderr, "crew dash: "+err.Error())
		return 1
	}
	return 0
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
}

func usage() string {
	return "usage: crew dash [--once | --json [--view <name> [--watch]]]\n" +
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
	case onceFlag && (jsonFlag || o.view != "" || o.watch):
		return opts{}, errors.New("--once takes no other flags")
	case o.view != "" && !jsonFlag:
		return opts{}, errors.New("--view needs --json")
	case o.watch && o.view == "":
		return opts{}, errors.New("--watch needs --view")
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
