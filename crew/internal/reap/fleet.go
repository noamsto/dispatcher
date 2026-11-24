package reap

// `crew reap --fleet`: the mode with no repo of its own. It discovers repos the
// way `crew fleet` does, adds the repos the layout-2 repo index lists, and
// reaps each one in process.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// FleetOptions carry `crew reap --fleet`'s discovery, so tests read a
// registry and sweep root of their own.
type FleetOptions struct {
	Getenv   func(string) string
	Registry string
}

// fleetRepo is one repo --fleet covers. Exactly one of skip, err and paths
// says what to do with it; common is the candidate as found, for messages.
type fleetRepo struct {
	common string
	skip   string
	err    error
	paths  bus.Paths
}

// fleetRepos is the --fleet repo set: Discover's repos in its order (an OK
// one, or a `no bus` one the repo index lists), then the index-only repos
// sorted, deduplicated by RepoTag. Every repo's bus carries the state root.
func fleetRepos(stderr io.Writer, fo FleetOptions, prefix string) []fleetRepo {
	getenv := fo.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	state := bus.StateRoot(getenv)
	indexed := map[string]bool{}
	hdrs := bus.IndexRepos(state)
	for _, h := range hdrs {
		indexed[h.Common] = true
	}
	seen := map[string]bool{}
	var out []fleetRepo
	for _, r := range repos.Discover(repos.Options{Registry: fo.Registry, Getenv: getenv, Stderr: stderr, Prefix: prefix}) {
		// Absolutize every candidate before any reap runs. Discovery does not
		// normalize: a relative CREW_SWEEP_ROOTS entry or registry line yields
		// a relative Common, and reap.Run hands the checkout cwd to
		// wtgit.TrustedCwd, whose ancestor walk never terminates on a relative
		// path (filepath.Dir(".") == "."). reap.Run also changes the process's
		// working directory, so a relative path resolved later would use the
		// wrong base.
		common, err := filepath.Abs(r.Common)
		tag := ""
		if err == nil {
			tag = repoTag(common)
		}
		if r.Status != repos.StatusOK && (r.Status != repos.StatusNoBus || !indexed[tag]) {
			out = append(out, fleetRepo{common: r.Common, skip: string(r.Status)})
			continue
		}
		if err != nil {
			out = append(out, fleetRepo{common: r.Common, err: err})
			continue
		}
		if tag != "" {
			seen[tag] = true
		}
		out = append(out, fleetRepo{common: common, paths: bus.PathsFor(common).WithState(state)})
	}
	for _, h := range hdrs {
		if seen[h.Common] {
			continue
		}
		if why := indexOnlySkip(h.Common); why != "" {
			out = append(out, fleetRepo{common: h.Common, skip: why})
			continue
		}
		out = append(out, fleetRepo{common: h.Common, paths: bus.PathsFor(h.Common).WithState(state)})
	}
	return out
}

// indexOnlySkip is why an index header's common dir is not reaped, "" when it
// is a repo's own .git dir: the index is written by crews, so its checkout is
// only trusted once it is the realpath of a real .git directory.
func indexOnlySkip(common string) string {
	switch {
	case !filepath.IsAbs(common):
		return "not absolute"
	case !filekind.IsDir(common):
		return "not a directory"
	case repoTag(common) != common:
		return "not its own realpath"
	case filepath.Base(common) != ".git":
		return "not a .git dir"
	}
	return ""
}

// RunFleet is `crew reap --fleet`: it reaps every repo fleetRepos finds in
// process. A repo whose reap exits non-zero is reported and the rest still
// run; the run exits 1 if any failed. It runs from any directory (it never
// needs a repo of its own).
func RunFleet(ctx context.Context, req Request, stdout, stderr io.Writer, o Options, fo FleetOptions) int {
	found := fleetRepos(stderr, fo, "crew: reap --fleet")
	failed := 0
	if req.JSON {
		req.keeps = &keepLog{}
		defer req.keeps.write(stdout)
		stdout = io.Discard
	}
	var targets []bus.Paths
	for _, r := range found {
		switch {
		case r.err != nil:
			say(stderr, "crew reap --fleet: %s: %v\n", r.common, r.err)
			failed++
		case r.skip != "":
			if !req.Quiet {
				say(stdout, "crew reap --fleet: %s: skipped (%s)\n", r.common, r.skip)
			}
		default:
			targets = append(targets, r.paths)
		}
	}
	for _, p := range targets {
		// A repo's checkout is its common dir's parent, the same derivation
		// `crew fleet` and `rate --sweep-all` make.
		if rc := o.fleetReap(ctx, req, p, filepath.Dir(p.Common), stdout, stderr); rc != 0 {
			say(stderr, "crew reap --fleet: %s: failed (rc=%d)\n", p.Common, rc)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// fleetReap is the loop's per-repo child: Options.FleetReap when the caller set
// one, the repo-keyed reap otherwise.
func (o Options) fleetReap(ctx context.Context, req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer) int {
	if o.FleetReap != nil {
		return o.FleetReap(ctx, req, paths, cwd, stdout, stderr)
	}
	return Run(ctx, req, paths, cwd, stdout, stderr, o)
}
