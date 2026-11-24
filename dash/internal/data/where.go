package data

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CrewBus is `crew where --json`: where one crew's bus lives. Layout 1 keeps
// the log in the repo's git common dir; layout 2 keeps it under crew state,
// with RepoDir nil for a crew that has no home repo.
type CrewBus struct {
	CrewID  string  `json:"crew_id"`
	Layout  int     `json:"layout"`
	Dir     string  `json:"dir"`
	Log     string  `json:"log"`
	RepoDir *string `json:"repo_dir"`
}

// WhereError is a failed crew where run; Code 3 is a refusal (an archived or
// index-only crew), which must not fall back to another bus.
type WhereError struct {
	Code int
	Msg  string
}

func (e *WhereError) Error() string { return "crew where: " + e.Msg }

// WhereRefused is the crew where exit code for a refused crew.
const WhereRefused = 3

// ResolveCrewBus asks crew where for the bus of crewID (the caller's own
// crew when empty). Any non-zero exit is a *WhereError; unusable output is an error.
func ResolveCrewBus(ctx context.Context, r Runner, cfg Config, crewID string) (CrewBus, error) {
	args := []string{"where", "--json"}
	if crewID != "" {
		args = append(args, "--crew", crewID)
	}
	out, errb, err := r.Run(ctx, cfg.Crew[0], crewArgs(cfg, args...)...)
	if err != nil {
		return CrewBus{}, &WhereError{Code: exitCode(err), Msg: FailureLine(errb, err)}
	}
	var b CrewBus
	if err := json.Unmarshal(bytes.TrimSpace(out), &b); err != nil {
		return CrewBus{}, fmt.Errorf("crew where: %w", err)
	}
	return b, nil
}

// ResolveCrewDir asks crew where for the crew's state directory, run from dir
// so a repo-scoped lookup sees that repo.
func ResolveCrewDir(ctx context.Context, r Runner, cfg Config, dir, id string) (string, error) {
	out, errb, err := r.RunIn(ctx, dir, cfg.Crew[0], crewArgs(cfg, "where", "--dir", "--crew", id)...)
	if err != nil {
		return "", fmt.Errorf("crew where: %s", FailureLine(errb, err))
	}
	got := strings.TrimSpace(string(out))
	if got == "" {
		return "", fmt.Errorf("crew where: empty output")
	}
	return got, nil
}

// RepoName is the display name of a repo path, which may be its git common dir.
func RepoName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(strings.TrimSuffix(path, "/.git"))
}

// RepoFromLog is the repo name on the crew's first dispatch row in the log,
// "" when there is none or the log is unreadable.
func RepoFromLog(path, crewID string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		ev, ok := decodeLine(scanner.Bytes())
		if !ok || ev["kind"] != "dispatch" || ev["crew_id"] != crewID {
			continue
		}
		if repo, _ := ev["repo"].(string); repo != "" {
			return RepoName(repo)
		}
	}
	return ""
}
