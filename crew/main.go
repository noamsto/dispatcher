// Command crew-go holds the Go ports of crew subcommands. crew.sh execs it as
// `crew-go <sub> <args...>` from the arms it replaces, so stdout, stderr and the
// exit status are the bash arm's.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// env is everything run reads from the process, so tests can supply their own.
type env struct {
	getwd    func() (string, error)
	jqColors string // $JQ_COLORS
	color    bool   // stdout is a terminal and $NO_COLOR is unset
	probes   func(ctx context.Context, cwd string) roster.Probes
}

func main() {
	e := env{
		getwd:    os.Getwd,
		jqColors: os.Getenv("JQ_COLORS"),
		color:    bus.IsTerminal(1) && os.Getenv("NO_COLOR") == "",
		probes:   probes,
	}
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, e))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
	if len(args) == 0 || (args[0] != "roster" && args[0] != "sessions") {
		say(stderr, "%s\n", usage)
		return exitUsage
	}
	sub, args := args[0], args[1:]
	cwd, err := e.getwd()
	if err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	paths, err := bus.Locate(ctx, cwd)
	if err != nil {
		say(stderr, "crew: not in a git repo\n")
		return exitFailure
	}

	var (
		branch, crew string
		pretty       bool
	)
	if sub == "sessions" {
		var msg string
		if branch, crew, msg = parseSessions(args); msg != "" {
			say(stderr, "%s\n", msg)
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

	out := bufio.NewWriterSize(stdout, 64<<10)
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		if sub == "sessions" {
			say(out, "[]\n")
		}
		return flush(out, stderr, 0)
	}
	// From here jq would have started, so it warns about a bad $JQ_COLORS even
	// when it then fails to open the log.
	palette, valid := jsonv.ParseJQColors(e.jqColors)
	if !valid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	opts := jsonv.Options{Indent: pretty}
	if e.color {
		opts.Colors = &palette
	}

	raw, code := fold(sub, events, err, paths.Log, stderr, func(evs []jsonv.Value) (jsonv.Value, error) {
		now := time.Now()
		nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6
		if sub == "sessions" {
			return sessions.Fold(evs, branch, crew, nowSec)
		}
		return roster.Fold(evs, crew, nowSec, e.probes(ctx, cwd))
	})
	if code == exitOpen && sub == "sessions" {
		// jq slurps zero inputs when it cannot open the file, so the program
		// runs on [] and prints its result before exiting 2.
		raw = jsonv.Array()
	} else if code != 0 {
		return code
	}

	if err := jsonv.Encode(out, raw, opts); err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	say(out, "\n")
	if sub == "sessions" && code == 0 {
		say(out, "\n") // the arm's `printf '\n'` after jq -c
	}
	return flush(out, stderr, code)
}

// say writes to a stream whose failure is reported elsewhere (out, through
// flush) or has nowhere to go (stderr).
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// flush writes out what is buffered and keeps code unless the write fails.
func flush(out *bufio.Writer, stderr io.Writer, code int) int {
	if err := out.Flush(); err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	return code
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
func fold(sub string, events []bus.Event, readErr error, log string, stderr io.Writer, f func([]jsonv.Value) (jsonv.Value, error)) (jsonv.Value, int) {
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
		open   *bus.OpenError
		decode *bus.DecodeError
		exit   *roster.ExitError
	)
	switch {
	case errors.As(err, &exit):
		say(stderr, "%s", exit.Stderr)
		return v, exit.Code
	case errors.As(err, &open):
		say(stderr, "crew: %s: %s: %v\n", sub, log, withoutPath(open.Err))
		return v, exitOpen
	case errors.As(err, &decode):
		say(stderr, "crew: %s: %s: %v\n", sub, log, decode.Err)
		return v, exitType
	default:
		say(stderr, "crew: %s: %s: %v\n", sub, log, err)
		return v, exitType
	}
}

// withoutPath is err minus the "op path:" prefix of an *fs.PathError, since the
// caller prints the path itself.
func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
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
