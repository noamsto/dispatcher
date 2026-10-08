// Command crew-go holds the Go ports of crew subcommands. crew.sh execs it as
// `crew-go <sub> <args...>` from the arms it replaces, so stdout, stderr and the
// exit status are the bash arm's.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
)

const (
	usage         = "crew-go: usage: crew-go roster [crew] | sessions <branch> [--crew ID]"
	sessionsUsage = "crew: sessions <branch> [--crew ID]"
	exitFailure   = 1
	exitOpen      = 2
	exitType      = 5
	exitUsage     = 64
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 || (args[0] != "roster" && args[0] != "sessions") {
		fmt.Fprintln(os.Stderr, usage)
		return exitUsage
	}
	sub, args := args[0], args[1:]
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "crew:", err)
		return exitFailure
	}
	paths, err := bus.Locate(ctx, cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "crew: not in a git repo")
		return exitFailure
	}

	var (
		branch, crew string
		pretty       bool
	)
	if sub == "sessions" {
		var msg string
		if branch, crew, msg = parseSessions(args); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
			return exitFailure
		}
	} else {
		pretty = true
		if len(args) > 0 {
			crew = args[0]
		}
		if crew == "" {
			crew = bus.CrewID(ctx, cwd)
		}
	}

	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		if sub == "sessions" {
			fmt.Println("[]")
		}
		return 0
	}
	palette, valid := jsonv.ParseJQColors(os.Getenv("JQ_COLORS"))
	if !valid {
		fmt.Fprintln(os.Stderr, "Failed to set $JQ_COLORS")
	}
	raw, code := fold(sub, events, err, paths.Log, func(evs []jsonv.Value) (jsonv.Value, error) {
		now := time.Now()
		nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6
		if sub == "sessions" {
			return sessions.Fold(evs, branch, crew, nowSec)
		}
		return roster.Fold(evs, crew, nowSec, probes(ctx, cwd))
	})
	if code != 0 {
		return code
	}

	opts := jsonv.Options{Indent: pretty}
	if bus.IsTerminal(1) && os.Getenv("NO_COLOR") == "" {
		opts.Colors = &palette
	}
	out := append(jsonv.Append(nil, raw, opts), '\n')
	if sub == "sessions" {
		out = append(out, '\n') // the arm's `printf '\n'` after jq -c
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintln(os.Stderr, "crew:", err)
		return exitFailure
	}
	return 0
}

// parseSessions mirrors the bash arm: branch is the first argument, the rest
// are `--crew ID` pairs, and msg is the stderr line when the arguments fail.
func parseSessions(args []string) (branch, crew, msg string) {
	if len(args) > 0 {
		branch, args = args[0], args[1:]
	}
	for len(args) > 0 {
		if args[0] != "--crew" {
			return "", "", sessionsUsage
		}
		if len(args) < 2 || args[1] == "" {
			return "", "", "crew: --crew needs a value"
		}
		crew, args = args[1], args[2:]
	}
	if branch == "" {
		return "", "", sessionsUsage
	}
	return branch, crew, ""
}

// fold runs the subcommand's fold over the read events, or maps the read or
// fold error to its stderr line and exit status.
func fold(sub string, events []bus.Event, readErr error, log string, f func([]jsonv.Value) (jsonv.Value, error)) (jsonv.Value, int) {
	var v jsonv.Value
	err := readErr
	if err == nil {
		raws := make([]jsonv.Value, len(events))
		for i, e := range events {
			raws[i] = e.Raw
		}
		v, err = f(raws)
	}
	if err == nil {
		return v, 0
	}
	var (
		open *bus.OpenError
		exit *roster.ExitError
	)
	switch {
	case errors.As(err, &exit):
		fmt.Fprint(os.Stderr, exit.Stderr)
		return v, exit.Code
	case errors.As(err, &open):
		if sub == "sessions" {
			// jq slurps zero inputs when it cannot open the file, so the
			// program runs on [] and prints its result before exiting 2.
			fmt.Println("[]")
		}
		fmt.Fprintf(os.Stderr, "crew: %s: %s: %v\n", sub, log, open.Err)
		return v, exitOpen
	default:
		fmt.Fprintf(os.Stderr, "crew: %s: %s: %v\n", sub, log, err)
		return v, exitType
	}
}

func probes(ctx context.Context, cwd string) roster.Probes {
	return roster.Probes{
		Panes: func() string {
			out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_current_command} #{pane_current_path}").Output()
			if err != nil {
				return ""
			}
			return string(out)
		},
		Worktrees: func() (string, error) {
			cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
			cmd.Dir = cwd
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err == nil {
				return string(out), nil
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				return "", &roster.ExitError{Code: 127, Stderr: "git: command not found\n"}
			}
			return "", &roster.ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
		},
	}
}
