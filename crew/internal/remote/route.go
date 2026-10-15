package remote

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
)

// Worker is a branch whose newest dispatch row in a crew is remote:true: where
// it runs, and the codename and ts the row records.
type Worker struct {
	Host, Branch, Name string
	TS                 jsonv.Value
}

// RemoteWorkers is crew's remote workers, sorted by host then branch.
func RemoteWorkers(rows []bus.Event, crew string) []Worker {
	newest := map[string]Worker{}
	for _, e := range rows {
		doc, ok := dispatchRow(e, crew)
		if !ok {
			continue
		}
		name, _ := doc["name"].(string)
		if host, remote := isRemote(doc); remote && host != "" {
			newest[e.Branch] = Worker{Host: host, Branch: e.Branch, Name: name, TS: e.TS}
		} else {
			delete(newest, e.Branch)
		}
	}
	workers := make([]Worker, 0, len(newest))
	for _, w := range newest {
		workers = append(workers, w)
	}
	slices.SortFunc(workers, func(a, b Worker) int {
		return cmp.Or(strings.Compare(a.Host, b.Host), strings.Compare(a.Branch, b.Branch))
	})
	return workers
}

// ResolveTarget is the remote half of the two-sided target resolution nudge and
// where share: rows are the local bus (resolve.Rows), and the target is looked
// up as `crew resolve-target` would, keeping only the workers whose newest
// dispatch row is remote:true. localBranch is the branch the caller's own tmux
// windows matched the target to, "" for none.
//
// Exactly one side matching picks that side: a remote match returns its host
// and branch, and no match returns "" for both, which leaves the local path to
// answer, errors included. Both sides matching is refused. A pane id names a
// pane on whichever host the caller is on, so it never routes.
func ResolveTarget(rows []any, target, crew, localBranch string) (host, branch string, err error) {
	if isPaneID(target) {
		return "", "", nil
	}
	var remote []resolve.Row
	for _, r := range resolve.Resolve(rows, target, crew) {
		if r.Remote {
			remote = append(remote, r)
		}
	}
	switch {
	case len(remote) == 0:
		return "", "", nil
	case localBranch != "":
		return "", "", fmt.Errorf("ambiguous target '%s' — matches a local worker (%s) and a remote one on %s (%s); pass a branch",
			target, localBranch, remote[0].Host, remote[0].Branch)
	case len(remote) > 1:
		matches := make([]string, len(remote))
		for i, r := range remote {
			matches[i] = fmt.Sprintf("%s (%s)", r.Branch, r.Host)
		}
		return "", "", fmt.Errorf("ambiguous target '%s' — matches remote workers: %s; pass a branch", target, strings.Join(matches, ", "))
	}
	return remote[0].Host, remote[0].Branch, nil
}

// isPaneID is `%` and a digit, the shape tmux gives a pane.
func isPaneID(target string) bool {
	return len(target) >= 2 && target[0] == '%' && target[1] >= '0' && target[1] <= '9'
}
