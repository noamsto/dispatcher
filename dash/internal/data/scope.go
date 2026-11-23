package data

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Scope is the crew a dash run is narrowed to. Crew is empty exactly when
// Source is "" or "none". Source is flag, env, window, pane or none; the fleet
// drill-down sets "fleet" itself.
type Scope struct {
	Crew   string
	Source string
	Known  bool
	Crews  int
	// Err is why the pane probe could not run; empty when it ran, even if it
	// found no dispatcher.
	Err string
}

// resolveScope picks the crew to scope to: an explicit id first, then the
// crew whose dispatcher pane shares the dash's tmux window.
func resolveScope(ctx context.Context, r Runner, cfg Config, rows []crewRow) Scope {
	sc := explicitScope(cfg)
	if sc.Crew == "" && cfg.Pane != "" {
		sc = paneScope(ctx, r, cfg, rows)
	}
	if sc.Crew == "" {
		return Scope{Source: "none", Err: sc.Err}
	}
	for _, row := range rows {
		if row.id == sc.Crew {
			sc.Known = true
		}
	}
	return sc
}

func explicitScope(cfg Config) Scope {
	if cfg.CrewID == "" {
		return Scope{}
	}
	return Scope{Crew: cfg.CrewID, Source: cfg.CrewIDSource}
}

// paneScope reads the window's @crew_id, else matches the window's other
// panes against each crew's recorded dispatcher pane file.
func paneScope(ctx context.Context, r Runner, cfg Config, rows []crewRow) Scope {
	out, errb, err := r.Run(ctx, "tmux", "display-message", "-p", "-t", cfg.Pane, "#{@crew_id}\t#{start_time}")
	if err != nil {
		return Scope{Err: "tmux display-message: " + firstLineOrExit(errb, exitCode(err))}
	}
	crew, start, _ := strings.Cut(strings.TrimRight(string(out), "\n"), "\t")
	if crew != "" {
		return Scope{Crew: crew, Source: "window"}
	}
	started, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return Scope{Err: "tmux display-message: unparseable start_time"}
	}
	out, errb, err = r.Run(ctx, "tmux", "list-panes", "-t", cfg.Pane, "-F", "#{pane_id}")
	if err != nil {
		return Scope{Err: "tmux list-panes: " + firstLineOrExit(errb, exitCode(err))}
	}
	events, err := EventsPath(ctx, r, cfg.Git)
	if err != nil {
		return Scope{Err: err.Error()}
	}
	crewsDir := filepath.Join(filepath.Dir(events), "crews")
	panes := map[string]bool{}
	for _, id := range strings.Fields(string(out)) {
		if id != cfg.Pane {
			panes[id] = true
		}
	}
	var best string
	var bestMod int64
	for _, row := range rows {
		file := filepath.Join(crewsDir, row.id, "pane")
		b, err := os.ReadFile(file)
		if err != nil || !panes[strings.TrimSpace(string(b))] {
			continue
		}
		fi, err := os.Stat(file)
		if err != nil {
			continue
		}
		mod := fi.ModTime().Unix()
		if mod < started || mod < bestMod || (mod == bestMod && row.id < best) {
			continue
		}
		best, bestMod = row.id, mod
	}
	if best == "" {
		return Scope{}
	}
	return Scope{Crew: best, Source: "pane"}
}
