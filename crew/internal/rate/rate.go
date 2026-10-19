// Package rate is `crew rate`: the per-repo sweep that folds this repo's crew
// bus into the global ratings store, the `--report` rollup of that store, and
// the `--sweep-all` loop that sweeps every repo on the machine. The package owns
// the mode-combination refusals; the command line itself is main's kong grammar.
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
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

//go:embed report.jq
var program string

//go:embed dedupe.jq
var dedupeProgram string

const (
	// Usage is the line an agent reads when it hands rate flags it cannot run;
	// it is the arm's own, kept verbatim.
	Usage       = "crew: rate takes --report, --json, --pooled, and --sweep-all/--root"
	exitType    = 5
	exitFailure = 1
)

// Options is everything rate reads beyond the bus, the store and the checkout.
type Options struct {
	// StorePath overrides the XDG ratings store location (tests).
	StorePath string
	// Git runs the arm's git queries, each as `git -C <dir> …`. The arm reads
	// them inside `$(... 2>/dev/null || true)`, so a failure is empty output.
	Git git.Client
	// Getenv reads DISPATCH_CONFIG_BIN and the XDG variables (tests).
	Getenv func(string) string
	// Clock.Now is the sweep's clock; the folds freeze jq's `now` from it.
	Clock clock.Clock
	// Gh runs the arm's gh calls, each `$(gh … 2>/dev/null)`; the call sites
	// also treat empty stdout as failure.
	Gh gh.Client
	// Run executes the dispatch-config resolver given as argv, capturing stdout
	// and dropping stderr.
	Run func(args ...string) (string, error)
	// SweepRepo sweeps one discovered repo, given its bus and checkout. It
	// defaults to the repo-keyed sweep; tests replace it to pin the --sweep-all
	// loop's lines and exit status without a store, gh or a lock.
	SweepRepo func(paths bus.Paths, workdir string, stderr io.Writer) int
}

// withDefaults fills the seams a caller left nil.
func (o Options) withDefaults() Options {
	if o.Git == nil {
		o.Git = git.Exec{}
	}
	if o.Gh == nil {
		o.Gh = gh.Exec{}
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
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

// Request is one parse of `crew rate`'s command line.
type Request struct {
	Report   bool
	JSON     bool
	Pooled   bool
	SweepAll bool
	Roots    []string
}

// refusal is the mode-combination check, in the arm's order: --root without
// --sweep-all before the modes are read, then each mode refuses the flags it
// cannot use. It is the whole line to print; empty is no refusal.
func (r Request) refusal() string {
	switch {
	case len(r.Roots) > 0 && !r.SweepAll:
		return "crew: rate --root needs --sweep-all"
	case r.SweepAll && (r.Report || r.JSON || r.Pooled):
		return Usage
	case (r.JSON || r.Pooled) && !r.Report:
		return Usage
	}
	return ""
}

// Run is `crew rate`: no flags sweeps this repo, `--report` renders the store,
// `--sweep-all` sweeps every repo it can find, and anything else is refused with
// one line. jq's exit status is mirrored (0, or 5 when a fold fails on a row it
// cannot use), its error wording is not.
func Run(req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	if why := req.refusal(); why != "" {
		say(stderr, "%s\n", why)
		return exitFailure
	}
	if req.SweepAll {
		// The loop reads the repos it discovers, never the caller's own, which is
		// why main.go may hand Run the zero paths.
		return sweepAll(req.Roots, stdout, stderr, o)
	}
	if req.Report {
		return report(req, cwd, stdout, stderr, o)
	}
	return runSweep(paths, cwd, stderr, o)
}

// report renders the global store: no bus, and no network beyond the one local
// git probe the scoped mode makes.
func report(fl Request, cwd string, stdout, stderr io.Writer, o Options) int {
	store := storePath(o.StorePath, o.Getenv)
	rows := []any{}
	if data, err := os.ReadFile(store); err == nil {
		if vals, err := decodeAll(bytes.NewReader(data)); err == nil && vals != nil {
			rows = vals
		}
	}
	deduped, err := jqrun.RunAny(dedupeProgram, rows, 0, nil)
	if err != nil {
		// A row `.run_id` cannot index (a bare number, say) failed the arm's
		// dedupe jq too, and its stderr went to /dev/null: fold to [].
		deduped = []any{}
	}
	list, _ := deduped.([]any)
	if list == nil {
		list = []any{}
	}

	currentRepo := ""
	if !fl.Pooled {
		currentRepo = currentRepoScope(o.Git, cwd)
	}
	out, err := jqrun.RunAny(program, list, 0, map[string]any{
		"want_json":    fl.JSON,
		"current_repo": currentRepo,
		"pooled":       fl.Pooled,
	})
	if err != nil {
		say(stderr, "crew: rate: %s: %v\n", store, err)
		return exitType
	}

	// --json is the mode's only non-string output, so the only shape jq
	// pretty-printed; the table is one string the fold already joined, which
	// `jq -r` printed plus one newline. Terminal colour is a process quirk.
	if fl.JSON {
		if err := encode(stdout, out, true); err != nil {
			say(stderr, "crew: rate: %v\n", err)
			return exitFailure
		}
		return 0
	}
	s, _ := out.(string)
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
func currentRepoScope(c git.Client, cwd string) string {
	ctx := context.Background()
	if slug := git.RepoSlug(git.Output(ctx, c, "", "-C", cwd, "config", "--get", "remote.origin.url")); slug != "" {
		return slug
	}
	toplevel := git.Output(ctx, c, "", "-C", cwd, "rev-parse", "--show-toplevel")
	if toplevel == "" {
		toplevel = "unknown-repo"
	}
	return filepath.Base(toplevel)
}
