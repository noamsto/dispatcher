package reap

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// tipPastReason is the keep reason of a branch whose tip holds commits past
// the merged PR that vouched for it; prGate writes it.
const tipPastReason = "its tip has commits past "

// Candidate is a kept branch a human may want to clean up (a stale keep, or a
// tip past its merged PR) with the bus facts needed to decide.
type Candidate struct {
	Keep
	// Codename is the branch's recorded codename, "" when none.
	Codename string
	// State is the latest status state, StatusTS its ts in ms (0 unknown).
	State    string
	StatusTS int64
	// PR is the bus's PR url for the branch, "" when none.
	PR string
	// Issues are the branch's claimed issue numbers.
	Issues []string
	// UnverifiedClaims counts claimed issues dropped from Issues because gh
	// cannot be pointed at them (non-numeric or cross-repo).
	UnverifiedClaims int
}

// Candidates runs reap's own classification read-only (--dry-run --json, which
// takes no lock and posts nothing) and returns its stale keeps and its
// tip-past-merged-PR keeps. A non-zero code is reap's own failure.
func Candidates(ctx context.Context, paths bus.Paths, cwd string, stderr io.Writer, o Options) ([]Candidate, int) {
	if !gh.Installed(ctx, o.Probes.Gh) {
		_, _ = io.WriteString(stderr, "crew cleanup: needs gh\n")
		return nil, 1
	}
	log := &keepLog{}
	req := Request{DryRun: true, JSON: true, Quiet: true, keeps: log}
	if code := Run(ctx, req, paths, cwd, io.Discard, stderr, o); code != 0 {
		return nil, code
	}
	r := &run{flags: newFlags(req), paths: paths, p: o.Probes}
	rows := map[string]latestRow{}
	if latest, err := r.latest(""); err == nil {
		for _, row := range latest {
			rows[row.branch] = row
		}
	}
	raws, _ := readRaws(paths.Log)
	// Bus pr_urls and claimed issues are worker-written: only this repo's PRs
	// and plain numbers may point gh anywhere, as reap's own gates require.
	repo := originGitHubRepo(ctx, o.Probes.Git)
	var out []Candidate
	for _, k := range log.keeps {
		if !k.Stale && !strings.HasPrefix(k.Reason, tipPastReason) {
			continue
		}
		c := Candidate{Keep: k}
		// A non-stale tip-past keep is listed only for a finished worker.
		if id, ok := identity.Recorded(raws, k.Branch); ok {
			c.Codename = id.Name
		}
		row, ok := rows[k.Branch]
		if !k.Stale && (!ok || !slices.Contains(terminalStates, any(row.state))) {
			continue
		}
		if ok {
			c.State = row.state
			c.StatusTS = int64(row.statusTS())
			for _, u := range []string{row.own, row.earlier, row.reaprow} {
				if prURLInRepo(u, repo) {
					c.PR = u
					break
				}
			}
		}
		if claimed, err := r.claimed(k.Branch); err == nil && claimed != "" {
			for n := range strings.SplitSeq(claimed, "\n") {
				if numericRe.MatchString(n) {
					c.Issues = append(c.Issues, n)
				} else if n != "" {
					c.UnverifiedClaims++
				}
			}
		}
		out = append(out, c)
	}
	return out, 0
}

func (l latestRow) statusTS() float64 { return (&candidate{latestRow: l}).statusTS() }

// FleetBuses are the buses of every repo `crew reap --fleet` covers.
func FleetBuses(stderr io.Writer, fo FleetOptions) []bus.Paths {
	var out []bus.Paths
	for _, r := range repos.Discover(repos.Options{Registry: fo.Registry, Getenv: fo.Getenv, Stderr: stderr, Prefix: "crew cleanup --fleet"}) {
		if r.Status != repos.StatusOK {
			continue
		}
		if common, err := filepath.Abs(r.Common); err == nil {
			out = append(out, bus.PathsFor(common))
		}
	}
	return out
}
