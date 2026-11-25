// Package tmuxstamp reads a worker's tmux windows and panes without letting a
// user-settable value shift what is read.
//
// tmux expands an option value (`@crew_branch`, `@crew_role`, …) into a
// `list-windows` or `list-panes` line raw, tabs and newlines included, so a
// process that can set options can print a whole forged row. The two lists here
// carry only ids and indexes tmux generates, and every stamp, session name,
// window name or foreground command is read one value per call for a target the
// list named. A value holding a tab or a newline is refused, so it matches nothing.
package tmuxstamp

import (
	"context"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// Window is one tmux window: its server-unique id and its index in its session.
type Window struct{ ID, Index string }

// Pane is one tmux pane: its id, its window's id and its index in the window.
type Pane struct{ ID, Window, Index string }

// Windows lists every window of the server. Only fields tmux generates are asked
// for, so a malformed line is skipped rather than parsed.
func Windows(ctx context.Context, c tmux.Client) ([]Window, error) {
	out, err := c.Run(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{window_index}")
	if err != nil {
		return nil, err
	}
	var wins []Window
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 2 && isID(f[0], '@') && isDigits(f[1]) {
			wins = append(wins, Window{ID: f[0], Index: f[1]})
		}
	}
	return wins, nil
}

// Panes lists every pane of the server, in the same tmux-generated fields.
func Panes(ctx context.Context, c tmux.Client) ([]Pane, error) {
	out, err := c.Run(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_index}")
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 && isID(f[0], '@') && isID(f[1], '%') && isDigits(f[2]) {
			panes = append(panes, Pane{Window: f[0], ID: f[1], Index: f[2]})
		}
	}
	return panes, nil
}

// WindowOption is window win's own value for the user option name (`@crew_branch`).
// ok is false for a value that holds a tab or a newline, or a read that failed.
func WindowOption(ctx context.Context, c tmux.Client, win, name string) (string, bool) {
	return clean(c.Run(ctx, "show-options", "-wqv", "-t", win, name))
}

// PaneOption is pane's own value for the user option name (`@crew_role`), read
// the same way.
func PaneOption(ctx context.Context, c tmux.Client, pane, name string) (string, bool) {
	return clean(c.Run(ctx, "show-options", "-pqv", "-t", pane, name))
}

// Field expands one format for target, for the user-settable built-ins a list may
// not carry: `#{session_name}`, `#{window_name}`, `#{pane_current_command}`.
func Field(ctx context.Context, c tmux.Client, target, format string) (string, bool) {
	return clean(c.Run(ctx, "display-message", "-p", "-t", target, format))
}

func clean(out string, err error) (string, bool) {
	if err != nil || strings.ContainsAny(out, "\t\n") {
		return "", false
	}
	return out, true
}

func isID(s string, prefix byte) bool {
	return len(s) > 1 && s[0] == prefix && isDigits(s[1:])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
