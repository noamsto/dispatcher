package data

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Event is one bus line, kept as a generic map so a future drill-down view
// can read any field (ts, kind, from, to, body, state/detail) without a
// second typed contract to keep in sync with the bus's own shape.
type Event = map[string]any

// EventsPath resolves the repo's bus log path: the common git dir (works
// from any worktree) plus "crew/events.jsonl".
func EventsPath(ctx context.Context, r Runner, git string) (string, error) {
	out, errb, err := r.Run(ctx, git, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %s", firstLineOrExit(errb, exitCode(err)))
	}
	dir := strings.TrimSpace(string(out))
	return dir + "/crew/events.jsonl", nil
}

// decodeLine parses one bus line. A blank or torn line (a hard-killed worker's
// partial append) reports false so callers skip it without aborting the rest.
func decodeLine(line []byte) (Event, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, false
	}
	return ev, true
}

// RecentEvents reads the last n bus events whose from is exactly
// "worker:<branch>" or starts with "worker:<branch>#" (a worker's own
// per-session suffix — "worker:feat/x-y#s1" does not match "worker:feat/x").
// Oldest-first. A torn last line (a hard-killed worker's partial append) is
// dropped, not an error.
func RecentEvents(path, branch string, n int) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	exact := "worker:" + branch
	prefix := "worker:" + branch + "#"

	var matched []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		ev, ok := decodeLine(scanner.Bytes())
		if !ok {
			continue
		}
		from, _ := ev["from"].(string)
		if from == exact || strings.HasPrefix(from, prefix) {
			matched = append(matched, ev)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(matched) > n {
		matched = matched[len(matched)-n:]
	}
	return matched, nil
}
