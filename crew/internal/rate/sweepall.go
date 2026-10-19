package rate

// `crew rate --sweep-all`: the mode with no repo of its own. It discovers repos
// the way `crew fleet` does and sweeps each one in process.

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"os"
	"path/filepath"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

//go:embed runs.jq
var runsProgram string

// sweepAll is the arm's loop: one stdout line per repo — skipped, failed, or
// swept with the runs the store now holds for it — and exit 1 when any repo
// failed. Discovery is `crew fleet`'s (registry plus a walk of the roots, with
// the CREW_SWEEP_ROOTS/$HOME defaulting), so its notes keep the arm's
// `crew: rate --sweep-all` prefix.
//
// Every repo is swept in this one process because runSweep takes its bus and
// checkout as arguments and reads no process state: git runs with `-C`, gh names
// its repo in a URL or an API path, and the burn-table resolver reads no cwd.
func sweepAll(roots []string, stdout, stderr io.Writer, o Options) int {
	found := repos.Discover(repos.Options{
		Roots:  roots,
		Getenv: o.Getenv,
		Stderr: stderr,
		Prefix: "crew: rate --sweep-all",
	})
	failed := 0
	for _, r := range found {
		if r.Status != repos.StatusOK {
			say(stdout, "%s: skipped (%s)\n", r.Common, r.Status)
			continue
		}
		// A repo's checkout is its common dir's parent, and the origin there folds
		// to the slug the store keys that repo's rows by.
		workdir := filepath.Dir(r.Common)
		origin := git.Output(context.Background(), o.Git, "", "-C", workdir, "config", "--get", "remote.origin.url")
		if origin == "" {
			say(stdout, "%s: skipped (no origin remote)\n", workdir)
			continue
		}
		slug := git.RepoSlug(origin)
		if rc := o.sweepRepo(busAt(r.Common), workdir, stderr); rc != 0 {
			say(stdout, "%s: failed (rc=%d)\n", slug, rc)
			failed++
			continue
		}
		say(stdout, "%s: swept, %d runs in store\n", slug, runsInStore(storePath(o.StorePath, o.Getenv), slug))
	}
	if failed > 0 {
		return exitFailure
	}
	return 0
}

// sweepRepo is the loop's per-repo child: Options.SweepRepo when the caller set
// one, the repo-keyed sweep otherwise.
func (o Options) sweepRepo(paths bus.Paths, workdir string, stderr io.Writer) int {
	if o.SweepRepo != nil {
		return o.SweepRepo(paths, workdir, stderr)
	}
	return runSweep(paths, workdir, stderr, o)
}

// busAt is a discovered repo's bus: the bus.Locate layout, spelled here because
// discovery already proved `<common>/crew/events.jsonl` exists. `fleet` reads
// the same paths off the same discovery.
func busAt(common string) bus.Paths {
	dir := common + "/crew"
	return bus.Paths{Common: common, Dir: dir, Log: dir + "/events.jsonl"}
}

// runsInStore is the loop's `jq -s --arg r <slug> '[.[] | select(.repo == $r) |
// .run_id] | unique | length' … 2>/dev/null || echo 0`: the distinct run ids the
// store holds for one repo, counted over the raw rows — not the report's dedupe,
// which also collapses by swept_at. A missing store, a torn line or a row `.repo`
// cannot index is jq's error, and jq's error is the arm's 0.
func runsInStore(store, slug string) int {
	data, err := os.ReadFile(store)
	if err != nil {
		return 0
	}
	rows, err := decodeAll(bytes.NewReader(data))
	if err != nil {
		return 0
	}
	if rows == nil {
		rows = []any{}
	}
	n, err := jqrun.RunAny(runsProgram, rows, 0, map[string]any{"r": slug})
	if err != nil {
		return 0
	}
	count, ok := asFloat(n)
	if !ok {
		return 0
	}
	return int(count)
}
