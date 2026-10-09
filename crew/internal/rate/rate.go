// Package rate is `crew rate`: the per-repo sweep that folds this repo's crew
// bus into the global ratings store, and the `--report` rollup of that store.
// The bash arm keeps flag parsing and every refusal line, and execs this
// binary for both modes — no flags for the sweep, `--report` for the report —
// so Run enforces the two-mode contract for direct invocations.
//
// The folds are the original jq programs from adapters/core/crew.sh
// (records.jq, plan.jq, view.jq, actions.jq, threads.jq, dedupe.jq, merge.jq,
// report.jq), run through gojq, so they stay one shared source with the arm
// they replace. The store reads fold every failure to `[]` in silence (`jq -s`
// under `2>/dev/null || true`, then `rows="${rows:-[]}"`), which is why a
// missing, empty or unparseable store renders the header alone: "no data yet"
// stays visibly distinct from "command did nothing".
package rate

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed report.jq
var program string

//go:embed dedupe.jq
var dedupeProgram string

const (
	// usage is the two-mode refusal; the bash arm parses flags before exec'ing,
	// so only direct crew-go calls can trip it.
	usage       = "crew-go: rate takes no flags (the sweep) or --report [--json] [--pooled]"
	exitType    = 5
	exitFailure = 1
)

// Options is everything rate reads beyond the bus, the store and the checkout.
type Options struct {
	// StorePath overrides the XDG ratings store location (tests).
	StorePath string
	// Git runs git in dir and returns stdout with trailing newlines gone,
	// ignoring the exit status — the arm reads git inside `$(... 2>/dev/null
	// || true)`, where a failed query is empty output, not an error. Tests
	// replace it.
	Git func(dir string, args ...string) string
	// LookupEnv reads DISPATCH_CONFIG_BIN and the XDG variables (tests).
	LookupEnv func(string) string
	// Now is the sweep's clock; the folds freeze jq's `now` from it.
	Now func() time.Time
	// Run executes one external command given as argv, capturing stdout and
	// dropping stderr — every arm call is `$(cmd 2>/dev/null …)`. The error is
	// its exit status: gh's call sites also treat empty stdout as failure,
	// while `git cat-file -e` succeeds by printing nothing.
	Run func(args ...string) (string, error)
}

// withDefaults fills the seams a caller left nil.
func (o Options) withDefaults() Options {
	if o.Git == nil {
		o.Git = func(dir string, args ...string) string {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			cmd.Stderr = io.Discard
			out, _ := cmd.Output()
			return strings.TrimRight(string(out), "\n")
		}
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.Getenv
	}
	if o.Run == nil {
		o.Run = func(args ...string) (string, error) {
			if len(args) == 0 {
				return "", fmt.Errorf("no command")
			}
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Stderr = io.Discard
			out, err := cmd.Output()
			// Every arm call site is `$(cmd 2>/dev/null)`: command substitution
			// drops the trailing newlines, and `originSlug`/the URL prefix checks
			// read that text as the value.
			return strings.TrimRight(string(out), "\n"), err
		}
	}
	return o
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm's two modes: no flags runs the sweep, `--report` renders the
// store, and anything else is the one-line usage. jq's exit status is mirrored
// (0, or 5 when a fold fails on a row it cannot use), its error wording is not.
func Run(args []string, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	report, jsonOut, pooled := false, false, false
	for _, arg := range args {
		switch arg {
		case "--report":
			report = true
		case "--json":
			jsonOut = true
		case "--pooled":
			pooled = true
		default:
			say(stderr, "%s\n", usage)
			return exitFailure
		}
	}
	if !report {
		if len(args) > 0 {
			say(stderr, "%s\n", usage)
			return exitFailure
		}
		return runSweep(paths, cwd, stderr, o)
	}

	store := storePath(o.StorePath, o.LookupEnv)
	var rows []jsonv.Value
	if data, err := os.ReadFile(store); err == nil {
		if vals, err := jsonv.DecodeStream(bytes.NewReader(data)); err == nil {
			rows = vals
		}
	}
	deduped, err := jqrun.Run(dedupeProgram, rows, 0, nil)
	if err != nil {
		// A row `.run_id` cannot index (a bare number, say) failed the arm's
		// dedupe jq too, and its stderr went to /dev/null: fold to [].
		deduped = jsonv.Array()
	}

	currentRepo := ""
	if !pooled {
		currentRepo = currentRepoScope(o.git(), cwd)
	}
	out, err := jqrun.Run(program, deduped.Elems(), 0, map[string]jsonv.Value{
		"want_json":    jsonv.Bool(jsonOut),
		"current_repo": jsonv.Str(currentRepo),
		"pooled":       jsonv.Bool(pooled),
	})
	if err != nil {
		say(stderr, "crew: rate: %s: %v\n", store, err)
		return exitType
	}

	// --json is the mode's only non-string output, so the only shape jq
	// pretty-printed; the table is one string the fold already joined, which
	// `jq -r` printed plus one newline. Terminal colour is a process quirk.
	if jsonOut {
		if err := jsonv.Encode(stdout, out, jsonv.Options{Indent: true}); err != nil {
			say(stderr, "crew: rate: %v\n", err)
			return exitFailure
		}
		say(stdout, "\n")
		return 0
	}
	s, _ := out.AsString()
	say(stdout, "%s\n", s)
	return 0
}

// storePath is the arm's `${XDG_DATA_HOME:-$HOME/.local/share}/crew/
// ratings.jsonl` — the global store the sweep appends to and reap reads.
func storePath(override string, lookupEnv func(string) string) string {
	if override != "" {
		return override
	}
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	base := lookupEnv("XDG_DATA_HOME")
	if base == "" {
		base = lookupEnv("HOME") + "/.local/share"
	}
	return filepath.Join(base, "crew", "ratings.jsonl")
}

// currentRepoScope is the arm's fallback chain: the origin slug, else the
// toplevel directory's basename, else "unknown-repo". Both git probes are
// local-only, so scoped mode still touches no network.
func currentRepoScope(git func(dir string, args ...string) string, cwd string) string {
	if slug := originSlug(git(cwd, "config", "--get", "remote.origin.url")); slug != "" {
		return slug
	}
	toplevel := git(cwd, "rev-parse", "--show-toplevel")
	if toplevel == "" {
		toplevel = "unknown-repo"
	}
	return filepath.Base(toplevel)
}

// slugPrefix is the arm's _origin_repo sed, first expression: a scheme (or
// the scp-style `git@`), the host and the separator. sed works on the whole
// line, so an `ssh://git@host/…` URL still loses its `git@host/` piece.
// regexp's leftmost match is the same one sed replaces.
var slugPrefix = regexp.MustCompile(`(git@|https://)[^/:]+[/:]`)

// originSlug applies _origin_repo's two sed rules to git's output: strip the
// scheme+host prefix, then a trailing .git. sed works line by line, so the
// rules map per line even though `git config --get` prints a single value.
func originSlug(out string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if loc := slugPrefix.FindStringIndex(line); loc != nil {
			line = line[:loc[0]] + line[loc[1]:]
		}
		lines[i] = strings.TrimSuffix(line, ".git")
	}
	return strings.Join(lines, "\n")
}

func (o Options) git() func(dir string, args ...string) string {
	return o.Git
}
