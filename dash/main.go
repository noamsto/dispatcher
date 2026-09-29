// Command crew-dash is a read-only dashboard over the layered dispatcher
// settings, per-engine budget, and the last few runs' retro notes and
// ratings. It probes nothing itself: every figure comes from a cached or
// already-run source (dispatch-config, refresh-budget's cache, crew retro/
// rate/crews/roster/hold list).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/term"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/once"
	"github.com/noamsto/dispatcher/dash/internal/ui"
)

// dispatchConfigBin is baked at build time via -ldflags -X
// main.dispatchConfigBin=…; empty in a raw `go build`.
var dispatchConfigBin string

func main() {
	os.Exit(run())
}

func run() int {
	mode, ok := parseArgs(os.Args[1:])
	if !ok {
		fmt.Fprintln(os.Stderr, "usage: crew dash [--once | --json]")
		return 2
	}
	if mode == "" {
		mode = "once"
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			mode = "interactive"
		}
	}

	cfg := resolveConfig()
	collect := func() data.Snapshot {
		return data.Collect(context.Background(), data.ExecRunner{}, cfg)
	}

	switch mode {
	case "json":
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
		eventsPath, eventsErr := data.EventsPath(context.Background(), data.ExecRunner{}, cfg.Git)
		collectRoster := func() data.RosterSection {
			return data.CollectRoster(context.Background(), data.ExecRunner{}, cfg)
		}
		deps := ui.Deps{
			Snapshot:      collect(),
			Collect:       collect,
			CollectRoster: collectRoster,
			EventsPath:    eventsPath,
			EventsPathErr: eventsErr,
		}
		if err := ui.Run(deps); err != nil {
			fmt.Fprintln(os.Stderr, "crew dash: "+err.Error())
			return 1
		}
		return 0
	}
	return 0
}

// parseArgs: no args leaves mode empty (decided by the caller from the
// ttys), --once/--json pick a mode, anything else is a usage error.
func parseArgs(args []string) (mode string, ok bool) {
	switch len(args) {
	case 0:
		return "", true
	case 1:
		switch args[0] {
		case "--once":
			return "once", true
		case "--json":
			return "json", true
		}
	}
	return "", false
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
