package reap

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// PaneCmd is one engine pane of an occupant window.
type PaneCmd struct{ Pane, Command string }

// Occupant is a worker window rooted at a worktree path. Pane and Command name
// the window's first engine pane; Panes lists all of them.
type Occupant struct {
	Window, Name, Pane, Command string
	Engine                      bool
	Panes                       []PaneCmd
}

// Occupants is `_occupants`: the worker windows rooted at wtp. Keyed on
// @crew_name (dispatch stamps it on every worker window), NOT on the pane's
// running command: a finished agent drops back to a shell prompt, and a command
// match would then read its window as empty and let the next dispatch stack a
// second worker onto the same tree (#17). Excludes the dispatcher's own window —
// it carries @crew_name too — and the caller's. The engine and pane fields ARE a
// command check (roster.IsEngineCmd) and are advisory only; occupancy itself
// stays window-keyed.
func Occupants(ctx context.Context, p Probes, wtp, tmuxPane string) []Occupant {
	selfWin := ""
	if tmuxPane != "" {
		selfWin = p.Tmux(ctx, "display-message", "-p", "-t", tmuxPane, "#{window_id}")
	}
	wins := p.Tmux(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{@crew_name}\t#{pane_current_path}")
	panes := p.Tmux(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_current_command}")

	var out []Occupant
	for row := range strings.SplitSeq(wins, "\n") {
		f := strings.SplitN(row, "\t", 3)
		for len(f) < 3 {
			f = append(f, "")
		}
		wid, name, path := f[0], f[1], f[2]
		if wid == "" || path != wtp || name == "" || name == "dispatcher" || wid == selfWin {
			continue
		}
		o := Occupant{Window: wid, Name: name}
		for prow := range strings.SplitSeq(panes, "\n") {
			pf := strings.SplitN(prow, "\t", 3)
			for len(pf) < 3 {
				pf = append(pf, "")
			}
			if pf[0] != wid || !roster.IsEngineCmd(pf[2]) {
				continue
			}
			if o.Pane == "" {
				o.Pane, o.Command = pf[1], pf[2]
			}
			o.Panes = append(o.Panes, PaneCmd{Pane: pf[1], Command: pf[2]})
		}
		o.Engine = o.Pane != ""
		out = append(out, o)
	}
	return out
}

// OccupantsJSON is the array `crew occupants` prints:
// [{window,name,pane|null,command,engine,panes:[{pane,command}]}].
func OccupantsJSON(occ []Occupant) jsonv.Value {
	rows := make([]jsonv.Value, 0, len(occ))
	for _, o := range occ {
		panes := make([]jsonv.Value, 0, len(o.Panes))
		for _, pc := range o.Panes {
			panes = append(panes, jsonv.Object(
				jsonv.Member{Key: "pane", Val: jsonv.Str(pc.Pane)},
				jsonv.Member{Key: "command", Val: jsonv.Str(pc.Command)},
			))
		}
		pane := jsonv.Null()
		if o.Pane != "" {
			pane = jsonv.Str(o.Pane)
		}
		rows = append(rows, jsonv.Object(
			jsonv.Member{Key: "window", Val: jsonv.Str(o.Window)},
			jsonv.Member{Key: "name", Val: jsonv.Str(o.Name)},
			jsonv.Member{Key: "pane", Val: pane},
			jsonv.Member{Key: "command", Val: jsonv.Str(o.Command)},
			jsonv.Member{Key: "engine", Val: jsonv.Bool(o.Engine)},
			jsonv.Member{Key: "panes", Val: jsonv.Array(panes...)},
		))
	}
	return jsonv.Array(rows...)
}

// RunOccupants is `crew occupants <worktree-path>`. It needs no repo.
func RunOccupants(ctx context.Context, args []string, stdout, stderr io.Writer, p Probes, tmuxPane string) int {
	if len(args) == 0 || args[0] == "" {
		_, _ = fmt.Fprintln(stderr, "crew: occupants <worktree-path>")
		return 1
	}
	line := jsonv.Append(nil, OccupantsJSON(Occupants(ctx, p, args[0], tmuxPane)), jsonv.Options{})
	_, _ = fmt.Fprintf(stdout, "%s\n", line)
	return 0
}
