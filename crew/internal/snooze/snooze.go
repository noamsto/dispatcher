// Package snooze records the branches a human told `crew cleanup` to keep for
// a while, so neither it nor reap's stale report raises them again until the
// snooze lapses.
package snooze

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Default is how long a kept branch stays quiet.
const Default = 14 * 24 * time.Hour

// Key identifies what was snoozed: a snooze holds only while the branch still
// has the tip and worktree it had then, so a re-dispatched branch of the same
// name is never hidden by an old one.
type Key struct{ Tip, Worktree string }

type entry struct {
	Until int64  `json:"until"`
	Tip   string `json:"tip"`
	WT    string `json:"worktree"`
}

// file is the per-repo store inside the bus dir: branch -> its snooze.
func file(busDir string) string { return filepath.Join(busDir, "cleanup-snooze.json") }

func read(busDir string) map[string]entry {
	m, _ := load(busDir)
	return m
}

// load is the store, empty when absent; a corrupt store is an error.
func load(busDir string) (map[string]entry, error) {
	m := map[string]entry{}
	b, err := os.ReadFile(file(busDir))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]entry{}, err
	}
	return m, nil
}

// Active reports whether branch is snoozed at now. An unreadable store never
// snoozes: a stale report is cheaper than a hidden worktree.
func Active(busDir, branch string, key Key, now time.Time) bool {
	e, ok := read(busDir)[branch]
	return ok && e.Until > now.UnixMilli() && e.Tip == key.Tip && e.WT == key.Worktree
}

// Set snoozes branch until `until`, dropping lapsed entries on the way.
func Set(busDir, branch string, key Key, now, until time.Time) error {
	m, err := load(busDir)
	if err != nil {
		return err
	}
	for b, e := range m {
		if e.Until <= now.UnixMilli() {
			delete(m, b)
		}
	}
	m[branch] = entry{Until: until.UnixMilli(), Tip: key.Tip, WT: key.Worktree}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(busDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	tmp, err := os.CreateTemp(busDir, "cleanup-snooze-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), file(busDir)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
