package reap

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// PaneCmd is one engine pane of an occupant window.
type PaneCmd struct {
	Pane    string `json:"pane"`
	Command string `json:"command"`
}

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
		selfWin = p.tmuxOut(ctx, "display-message", "-p", "-t", tmuxPane, "#{window_id}")
	}
	wins := p.tmuxOut(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{@crew_name}\t#{pane_current_path}")
	panes := p.tmuxOut(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_current_command}")

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

// occupantJSON is one element of the array `crew occupants` prints.
type occupantJSON struct {
	Window  string    `json:"window"`
	Name    string    `json:"name"`
	Pane    *string   `json:"pane"`
	Command string    `json:"command"`
	Engine  bool      `json:"engine"`
	Panes   []PaneCmd `json:"panes"`
}

// occupantsJSON is the array `crew occupants` prints:
// [{window,name,pane|null,command,engine,panes:[{pane,command}]}].
func occupantsJSON(occ []Occupant) []occupantJSON {
	rows := make([]occupantJSON, 0, len(occ))
	for _, o := range occ {
		var pane *string
		if o.Pane != "" {
			pane = &o.Pane
		}
		rows = append(rows, occupantJSON{
			Window:  o.Window,
			Name:    o.Name,
			Pane:    pane,
			Command: o.Command,
			Engine:  o.Engine,
			Panes:   append([]PaneCmd{}, o.Panes...),
		})
	}
	return rows
}

// RunOccupants is `crew occupants <worktree-path>`. It needs no repo.
func RunOccupants(ctx context.Context, args []string, stdout, stderr io.Writer, p Probes, tmuxPane string) int {
	if len(args) == 0 || args[0] == "" {
		_, _ = fmt.Fprintln(stderr, "crew: occupants <worktree-path>")
		return 1
	}
	_ = jsonval.Encode(stdout, occupantsJSON(Occupants(ctx, p, args[0], tmuxPane)), false)
	return 0
}
