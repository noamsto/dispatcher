package rosterrender

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/gh"
)

// fileCI keeps each open PR's last CI lookup, so the 2 s daemon asks GitHub once
// a minute per PR and a one-shot render reads it without gh.
const fileCI = "roster-render.ci.json"

const (
	ciTTL      = 60 * time.Second
	ciTimeout  = 8 * time.Second
	ciParallel = 4
	ciVer      = 1

	// ciMaxAge is how old a lookup may be and still be drawn: a gh that keeps
	// failing, or a map read of a cache no daemon refreshes, must not show a state
	// that has long gone.
	ciMaxAge = 10 * ciTTL

	// ciMaxKey and ciMaxFile bound what an edited cache file can cost to read.
	ciMaxKey  = 200
	ciMaxFile = 1 << 20

	maxPRDigits = 10
)

// ciEntry is one PR's CI state and review decision; TS is the unix second of
// the lookup that produced them. Tried is the last lookup that failed: it holds
// the retry off for a TTL, so a gh that is down costs one timeout a minute, not
// one per pass. An entry whose lookup never succeeded has an empty CI.
type ciEntry struct {
	CI     string `json:"ci"`
	Review string `json:"review,omitempty"`
	TS     int64  `json:"ts"`
	Tried  int64  `json:"tried,omitempty"`
}

// prOf is whether url is a PR page of the owner/name repo. It is the cheap
// check ahead of fleet.PRChecks's own, so a worker-written url of another repo,
// or one too long for the cache to keep, never costs a lookup attempt or reaches
// the log.
func prOf(url, repo string) bool {
	number, ok := strings.CutPrefix(strings.ToLower(url), "https://github.com/"+strings.ToLower(repo)+"/pull/")
	return ok && len(url) <= ciMaxKey && len(number) <= maxPRDigits && ascii.IsDigits(number)
}

// ciWant is a PR to look up, with the raw owner/name of the bus it belongs to.
type ciWant struct{ URL, Repo string }

type ciFile struct {
	V   int                `json:"v"`
	PRs map[string]ciEntry `json:"prs"`
}

// loadCI reads the cache. The file is plain text anyone can edit, so an entry
// it cannot vouch for is dropped, and no more than maxPlanets are kept.
func loadCI(cdir string) map[string]ciEntry {
	out := map[string]ciEntry{}
	f, err := os.Open(filepath.Join(cdir, fileCI))
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	var file ciFile
	if err := json.NewDecoder(io.LimitReader(f, ciMaxFile)).Decode(&file); err != nil || file.V != ciVer {
		return out
	}
	for _, url := range slices.Sorted(maps.Keys(file.PRs)) {
		if len(out) >= maxPlanets {
			break
		}
		e := file.PRs[url]
		if len(url) > ciMaxKey || !validCI(e) {
			continue
		}
		out[url] = e
	}
	return out
}

func validCI(e ciEntry) bool {
	switch e.CI {
	case "", "pass", "pending", "fail", "none":
	default:
		return false
	}
	switch e.Review {
	case "", "REVIEW_REQUIRED", "APPROVED", "CHANGES_REQUESTED":
		return true
	}
	return false
}

// refreshCI looks up the wanted PRs whose entry is missing or older than ciTTL
// and returns the cache for the wanted ones. A failed lookup keeps the old
// entry. Without a client it only reads. The file is rewritten only when the
// cache changed, and a save that fails costs nothing but the next pass's lookup.
func refreshCI(ctx context.Context, c gh.Client, cdir string, wants []ciWant, now time.Time, stderr io.Writer) map[string]ciEntry {
	loaded := loadCI(cdir)
	if c == nil {
		return loaded
	}
	entries := maps.Clone(loaded)

	var due []ciWant
	seen := map[string]bool{}
	for _, w := range wants {
		if seen[w.URL] {
			continue
		}
		seen[w.URL] = true
		if e, ok := entries[w.URL]; !ok || now.Sub(time.Unix(max(e.TS, e.Tried), 0)) >= ciTTL {
			due = append(due, w)
		}
	}

	var mu sync.Mutex
	sem := make(chan struct{}, ciParallel)
	var wg sync.WaitGroup
	for _, w := range due {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			info, err := fleet.PRChecks(ctx, c, w.URL, w.Repo, ciTimeout)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				say(stderr, "crew: roster-render: ci %s: %v\n", w.URL, err)
				old := entries[w.URL]
				old.Tried = now.Unix()
				entries[w.URL] = old
				return
			}
			entries[w.URL] = ciEntry{CI: info.CI, Review: info.Review, TS: now.Unix()}
		})
	}
	wg.Wait()

	maps.DeleteFunc(entries, func(url string, _ ciEntry) bool { return !seen[url] })
	if maps.Equal(entries, loaded) {
		return entries
	}
	if data, err := json.Marshal(ciFile{V: ciVer, PRs: entries}); err == nil {
		_ = put(filepath.Join(cdir, fileCI), string(data)+"\n", stderr)
	}
	return entries
}

// applyCI paints an open PR's planet with its CI state. A review still
// required on a PR whose checks have not failed is a merge waiting on a human.
// An entry older than ciMaxAge says nothing.
func applyCI(ps []planet, entries map[string]ciEntry, now time.Time) {
	for i := range ps {
		p := &ps[i]
		if p.class != classPROpen || p.prURL == "" {
			continue
		}
		e, ok := entries[p.prURL]
		if !ok || e.CI == "" || now.Sub(time.Unix(e.TS, 0)) > ciMaxAge {
			continue
		}
		p.ci = e.CI
		if e.Review == "REVIEW_REQUIRED" && e.CI != "fail" {
			p.waiting = true
		}
	}
}
