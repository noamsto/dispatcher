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
	"os"
	"os/exec"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/log"
	"github.com/noamsto/dispatcher/crew/internal/report"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
)

const (
	usage         = "crew-go: usage: crew-go roster [crew] | sessions <branch> [--crew ID] | crews [--mine] | log [crew] | report [crew]"
	sessionsUsage = "crew: sessions <branch> [--crew ID]"
	exitFailure   = 1
	exitOpen      = 2 // sessions prints [] where jq slurps zero inputs
	exitUsage     = 64
)

// env is everything run reads from the process, so tests can supply their own.
type env struct {
	getwd    func() (string, error)
	jqColors string // $JQ_COLORS
	color    bool   // stdout is a terminal and $NO_COLOR is unset
	probes   func(ctx context.Context, cwd string) roster.Probes
	procs    crews.Probes // the pid probes crews reads
}

func main() {
	e := env{
		getwd:    os.Getwd,
		jqColors: os.Getenv("JQ_COLORS"),
		color:    bus.IsTerminal(1) && os.Getenv("NO_COLOR") == "",
		probes:   probes,
		procs:    crews.DefaultProbes(),
	}
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, e))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "roster", "sessions", "crews", "log", "report":
	default:
		say(stderr, "%s\n", usage)
		return exitUsage
	}
	args = args[1:]
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

	// crews emits TSV, reads the log with its own torn-line tolerance, and
	// --mine never reads it at all, so it stays off the shared fold path.
	if sub == "crews" {
		out := bufio.NewWriterSize(stdout, 64<<10)
		_, colorsOK := jsonv.ParseJQColors(e.jqColors)
		code := crews.Run(args, paths, out, stderr, crews.Options{
			Probes:          e.procs,
			JQColorsInvalid: !colorsOK,
			Now:             time.Now,
		})
		return flush(out, stderr, code)
	}

	// log and report print lines rather than one JSON value, and each reads the
	// bus with its arm's tolerance (log keeps jq -c's torn-tail prefix, report
	// has none), so they own their read too.
	if sub == "log" || sub == "report" {
		crew := crewArg(args, ctx, cwd)
		out := bufio.NewWriterSize(stdout, 64<<10)
		palette, colorsOK := jsonv.ParseJQColors(e.jqColors)
		var code int
		if sub == "log" {
			opts := log.Options{JQColorsInvalid: !colorsOK}
			if e.color {
				opts.Colors = &palette
			}
			code = log.Run(crew, paths, out, stderr, opts)
		} else {
			code = report.Run(crew, paths, out, stderr, report.Options{
				JQColorsInvalid: !colorsOK,
				Now:             time.Now,
			})
		}
		return flush(out, stderr, code)
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
		crew = crewArg(args, ctx, cwd)
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

// crewArg is an arm's `"${1:-$(_crew_id)}"`: the first argument, falling back
// to this repo's crew when it is absent or empty.
func crewArg(args []string, ctx context.Context, cwd string) string {
	crew := ""
	if len(args) > 0 {
		crew = args[0]
	}
	if crew == "" {
		crew = bus.CrewID(ctx, cwd)
	}
	return crew
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
	var exit *roster.ExitError
	if errors.As(err, &exit) {
		say(stderr, "%s", exit.Stderr)
		return v, exit.Code
	}
	// Every other outcome is a bus-read failure: bus.JQFailure is jq's message
	// and status for the same file (2 unreadable, 5 corrupt), shared with the
	// log and report arms.
	msg, code := bus.JQFailure(err)
	say(stderr, "crew: %s: %s: %v\n", sub, log, msg)
	return v, code
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
