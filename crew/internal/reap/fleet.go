package reap

// `crew reap --fleet`: the mode with no repo of its own. It discovers repos the
// way `crew fleet` does and reaps each one in process.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// FleetOptions carry `crew reap --fleet`'s discovery, so tests read a
// registry and sweep root of their own.
type FleetOptions struct {
	Getenv   func(string) string
	Registry string
}

// RunFleet is `crew reap --fleet`: it discovers every repo on the machine
// with a crew bus (the same discovery `crew fleet` and `crew rate
// --sweep-all` use) and reaps each one in process. A repo whose reap exits
// non-zero is reported and the rest still run; the run exits 1 if any
// failed. It runs from any directory (it never needs a repo of its own).
func RunFleet(ctx context.Context, req Request, stdout, stderr io.Writer, o Options, fo FleetOptions) int {
	found := repos.Discover(repos.Options{Registry: fo.Registry, Getenv: fo.Getenv, Stderr: stderr, Prefix: "crew: reap --fleet"})
	failed := 0
	// Absolutize every candidate before any reap runs. Discovery does not
	// normalize: a relative CREW_SWEEP_ROOTS entry or registry line yields a
	// relative Common, and reap.Run hands the checkout cwd to
	// wtgit.TrustedCwd, whose ancestor walk never terminates on a relative
	// path (filepath.Dir(".") == "."). reap.Run also changes the process's
	// working directory, so a relative path resolved in the second pass would
	// use the wrong base.
	commons := make([]string, 0, len(found))
	for _, r := range found {
		if r.Status != repos.StatusOK {
			if !req.Quiet {
				say(stdout, "crew reap --fleet: %s: skipped (%s)\n", r.Common, r.Status)
			}
			continue
		}
		common, err := filepath.Abs(r.Common)
		if err != nil {
			say(stderr, "crew reap --fleet: %s: %v\n", r.Common, err)
			failed++
			continue
		}
		commons = append(commons, common)
	}
	for _, common := range commons {
		// A repo's checkout is its common dir's parent, the same derivation
		// `crew fleet` and `rate --sweep-all` make.
		workdir := filepath.Dir(common)
		if rc := o.fleetReap(ctx, req, busAt(common), workdir, stdout, stderr); rc != 0 {
			say(stderr, "crew reap --fleet: %s: failed (rc=%d)\n", common, rc)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// busAt is a discovered repo's bus: the bus.Locate layout, spelled here
// because discovery already proved <common>/crew/events.jsonl exists.
func busAt(common string) bus.Paths {
	dir := common + "/crew"
	return bus.Paths{Common: common, Dir: dir, Log: dir + "/events.jsonl"}
}

// fleetReap is the loop's per-repo child: Options.FleetReap when the caller set
// one, the repo-keyed reap otherwise.
func (o Options) fleetReap(ctx context.Context, req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer) int {
	if o.FleetReap != nil {
		return o.FleetReap(ctx, req, paths, cwd, stdout, stderr)
	}
	return Run(ctx, req, paths, cwd, stdout, stderr, o)
}
