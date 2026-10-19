package reap

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// humanProgram is `_human_present`'s transcript filter, run once per decoded
// transcript row. See the file for the patch it needs before jqrun can run it.
//
//go:embed human.jq
var humanProgram string

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// Deps is what `_release_windows` read from the arm's globals: the git common
// dir, the bus log, and the say/note output functions. Say and Note receive the
// bare message text; the caller adds any prefix.
type Deps struct {
	Common, Log string
	Say, Note   func(string)
	P           Probes
}

// ReleaseWindows is `_release_windows`: kill the WINDOWs of a finished worker
// session, never its worktree — a worker legitimately sits in `done` for as
// long as its PR takes to merge (#17). It returns 3 (nothing killed) when a
// person is present or an engine pane is not provably idle, so the caller
// re-checks on its next cycle, else 0. The error is only a failed release-row
// write or its marshal.
func ReleaseWindows(ctx context.Context, d Deps, branch, session, state string, tsMS, grace int64, dry bool) (int, error) {
	porcelain, _ := git.Worktrees(ctx, d.P.Git, "", d.Common)
	wt := roster.WorktreePath(porcelain, branch)
	if wt == "" {
		return 0, nil
	}
	if st, err := os.Stat(wt); err != nil || !st.IsDir() {
		return 0, nil
	}
	occ := Occupants(ctx, d.P, wt, os.Getenv("TMUX_PANE"))
	if len(occ) == 0 {
		return 0, nil
	}
	// `exited` is the SessionEnd backstop, not something the worker asserts, and
	// it fires for subagents and stray panes too (#69) — so an `exited` row with
	// a live engine in the tree is a false read, and killing the window would
	// kill a working agent. `done`/`failed` are the worker's own word and stay
	// releasable, engine or not: an agent legitimately idles in its pane after
	// posting `done`, which is exactly what idle-release exists to clean up (#17).
	if state == "exited" && slices.ContainsFunc(occ, func(o Occupant) bool { return o.Engine }) {
		d.Note("keeping " + branch + " — exited but an engine is still running there")
		return 0, nil
	}
	windows := make([]string, len(occ))
	for i, o := range occ {
		windows[i] = o.Window
	}
	if why, ok := humanPresent(ctx, d.P, windows, wt, tsMS, grace); ok {
		d.Note("keeping " + branch + " — human present: " + why)
		return 3, nil
	}
	if why, ok := framesBusy(ctx, d.P, occ); ok {
		d.Note("keeping " + branch + " — pane busy: " + why)
		return 3, nil
	}
	for _, w := range windows {
		if dry {
			d.Say("would release " + w + " at " + wt + " (" + branch + " " + state + ")")
			continue
		}
		_, _ = d.P.Tmux.Run(ctx, "kill-window", "-t", w)
		d.Say("released " + w + " at " + wt + " (" + branch + " " + state + ")")
	}
	if dry {
		return 0, nil
	}
	row := releaseRow{
		TS:      d.P.Now().UnixMilli(),
		Kind:    "release",
		Branch:  branch,
		Session: session,
		State:   state,
		Windows: append([]string{}, windows...),
	}
	return 0, d.P.Append(d.Log, rowLine(row))
}

// releaseRow is the bus row a release appends.
type releaseRow struct {
	TS      int64    `json:"ts"`
	Kind    string   `json:"kind"`
	Branch  string   `json:"branch"`
	Session string   `json:"session"`
	State   string   `json:"state"`
	Windows []string `json:"windows"`
}

// humanPresent is `_human_present`: why, and true, when a person is evidently
// using a finished worker's window — it is the active window of an attached
// tmux client, it saw output within the grace, or its claude transcript holds a
// user turn newer than the status.
func humanPresent(ctx context.Context, p Probes, windows []string, wtp string, tsMS, grace int64) (string, bool) {
	now := p.Now().Unix()
	rows := p.tmuxOut(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{window_active}\t#{session_attached}\t#{window_activity}")
	for row := range strings.SplitSeq(rows, "\n") {
		f := strings.SplitN(row, "\t", 4)
		for len(f) < 4 {
			f = append(f, "")
		}
		wid, active, attached, act := f[0], f[1], f[2], f[3]
		if wid == "" || !slices.Contains(windows, wid) {
			continue
		}
		if n, err := strconv.Atoi(attached); active == "1" && err == nil && n > 0 {
			return wid + " is visible in an attached tmux client", true
		}
		if a, err := strconv.ParseInt(act, 10, 64); err == nil && now-a < grace {
			return wid + " had activity within the grace", true
		}
	}
	if newerUserTurn(transcriptDir(wtp), tsMS) {
		return "the engine transcript has a user turn newer than the status", true
	}
	return "", false
}

// transcriptDir is where claude keeps the transcripts of sessions rooted at wtp.
func transcriptDir(wtp string) string {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		cfg = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	return filepath.Join(cfg, "projects", nonAlnum.ReplaceAllString(wtp, "-"))
}

// newerUserTurn reports whether the newest *.jsonl in dir holds a user turn
// stamped after tsMS. Anything unreadable is no turn. The file is
// worker-writable and unbounded, so it is streamed a line at a time (no line
// length cap) and the filter runs per row, stopping at the first match.
func newerUserTurn(dir string, tsMS int64) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	var latest string
	var latestMod time.Time
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		if latest == "" || st.ModTime().After(latestMod) {
			latest, latestMod = f, st.ModTime()
		}
	}
	if latest == "" {
		return false
	}
	prog, err := jqrun.CompileAny(humanProgram, 0, map[string]any{"ts": json.Number(strconv.FormatInt(tsMS, 10))})
	if err != nil {
		return false
	}
	f, err := os.Open(latest)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadString('\n')
		if row, err := parseOne(line); err == nil {
			if _, err := prog.RunAny(row); err == nil {
				return true
			}
		}
		if readErr != nil {
			return false
		}
	}
}

// capture is `$(_pane_capture …)`: trailing newlines are stripped.
func capture(ctx context.Context, p Probes, pane string, colored bool) string {
	text, _ := probe.Capture(ctx, p.Tmux, p.Getenv, pane, colored)
	return strings.TrimRight(text, "\n")
}

// releaseGap is `sleep "${CREW_RELEASE_GAP:-1}"`; a value sleep would refuse is
// the default.
func releaseGap() time.Duration {
	if s, err := strconv.ParseFloat(os.Getenv("CREW_RELEASE_GAP"), 64); err == nil && s >= 0 && !math.IsInf(s, 0) && s < 1e9 {
		return time.Duration(s * float64(time.Second))
	}
	return time.Second
}

// framesBusy is `_frames_busy`: why, and true, when any engine pane of a
// finished worker's windows is not provably idle. Every pane is sampled twice,
// one gap apart: claude and pi panes must pass their idle check on both with identical
// text; other engines have no idle signature, so they need an unchanged frame
// and no prompt. An unreadable pane keeps.
func framesBusy(ctx context.Context, p Probes, occ []Occupant) (string, bool) {
	var panes, cmds, plain, colored []string
	for _, o := range occ {
		for _, pc := range o.Panes {
			if pc.Pane == "" {
				continue
			}
			panes = append(panes, pc.Pane)
			cmds = append(cmds, pc.Command)
			text := capture(ctx, p, pc.Pane, false)
			plain = append(plain, text)
			colored = append(colored, capture(ctx, p, pc.Pane, true))
			if text == "" {
				return pc.Pane + " is unreadable", true
			}
		}
	}
	if len(panes) == 0 {
		return "", false
	}
	p.Sleep(releaseGap())
	for i, pane := range panes {
		engine := strings.TrimSuffix(strings.TrimPrefix(cmds[i], "."), "-wrapped")
		if capture(ctx, p, pane, false) != plain[i] {
			return pane + " is still changing", true
		}
		if engine == "claude" {
			if why, idle := frame.PaneIdleReason(plain[i], colored[i], false); !idle {
				return pane + " is not idle (" + why + ")", true
			}
		} else if engine == "pi" {
			if why, idle := frame.PiIdleReason(plain[i]); !idle {
				return pane + " is not idle (pi frame: " + why + ")", true
			}
		} else if frame.IsPrompt(engine, plain[i]) || frame.IsPermissionPrompt(engine, plain[i]) {
			return pane + " shows a prompt", true
		}
	}
	return "", false
}
