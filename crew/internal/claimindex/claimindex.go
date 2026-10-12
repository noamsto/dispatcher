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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

var part = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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
// `git remote get-url`, which would apply insteadOf.
func Path(common string, getenv func(string) string) string {
	out, err := exec.Command("git", "--git-dir="+common, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	slug := Slug(strings.TrimSpace(string(out)))
	if slug == "" {
		return ""
	}
	base := getenv("XDG_DATA_HOME")
	if base == "" {
		base = getenv("HOME") + "/.local/share"
	}
	return filepath.Join(base, "crew", "claims", slug+".jsonl")
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
func Release(common string, getenv func(string) string, issue, crewID string, now time.Time) error {
	path := Path(common, getenv)
	if path == "" {
		return nil
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
