// Package claimindex is the machine-wide index of `dispatched` issue claims,
// one append-only JSONL per repo under
// `${XDG_DATA_HOME:-$HOME/.local/share}/crew/claims/<owner>/<repo>.jsonl`.
//
// dispatch.sh writes the claim rows and judges their liveness; this package
// only writes the release rows `crew reap` and `crew adopt` leave when they
// remove the label. The key derivation and the row shapes are a contract with
// dispatch.sh's `_claim_index_file`, which has a bash copy of Slug.
package claimindex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
)

var part = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// StaleAfter is the claims-index aging rule, dispatch.sh's _CLAIM_STALE_MS: a
// worker on another host never writes its `reaped` row to this bus, so after a
// day its claim and its remote dispatch row stop counting as live.
const StaleAfter = 24 * time.Hour

// Slug is the lowercased `owner/repo` of an origin URL: its last two path
// segments, so https, ssh and scp forms of one repo agree. Empty when the URL
// does not end in two safe segments.
func Slug(url string) string {
	p := strings.TrimRight(url, "/")
	p = strings.TrimRight(strings.TrimSuffix(p, ".git"), "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	repo, rest := p[i+1:], p[:i]
	owner := rest[strings.LastIndex(rest, "/")+1:]
	owner = owner[strings.LastIndex(owner, ":")+1:]
	for _, s := range []string{owner, repo} {
		if !part.MatchString(s) || s == "." || s == ".." {
			return ""
		}
	}
	return strings.ToLower(owner + "/" + repo)
}

// Path is the index file for the repo whose git common dir is common, or "" when
// its origin yields no slug. Like dispatch.sh it reads the configured URL, not
// `git remote get-url`, which would apply insteadOf. An error means git could
// not be asked; an unset origin is "".
func Path(ctx context.Context, g git.Client, common string, getenv func(string) string) (string, error) {
	out, err := git.Origin(ctx, g, common)
	if err != nil {
		return "", err
	}
	slug := Slug(strings.TrimSpace(out))
	if slug == "" {
		return "", nil
	}
	base := getenv("XDG_DATA_HOME")
	if base == "" {
		base = getenv("HOME") + "/.local/share"
	}
	return filepath.Join(base, "crew", "claims", slug+".jsonl"), nil
}

type release struct {
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Issue  string `json:"issue"`
	CrewID string `json:"crew_id"`
	Clone  string `json:"clone"`
}

// Release records that issue's claim was released, so claims older than now no
// longer count as evidence. A repo with no slug has no index and is a no-op.
func Release(ctx context.Context, g git.Client, common string, getenv func(string) string, issue, crewID string, now time.Time) error {
	path, err := Path(ctx, g, common, getenv)
	if err != nil || path == "" {
		return err
	}
	row, err := json.Marshal(release{TS: now.UnixMilli(), Kind: "release", Issue: issue, CrewID: crewID, Clone: filepath.Dir(common)})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return bus.Append(path, string(row))
}
