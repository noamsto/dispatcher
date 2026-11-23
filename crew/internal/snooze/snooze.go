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

// file is the per-repo store inside the bus dir: branch -> unix ms it lapses.
func file(busDir string) string { return filepath.Join(busDir, "cleanup-snooze.json") }

func read(busDir string) map[string]int64 {
	b, err := os.ReadFile(file(busDir))
	if err != nil {
		return map[string]int64{}
	}
	m := map[string]int64{}
	if json.Unmarshal(b, &m) != nil {
		return map[string]int64{}
	}
	return m
}

// Active reports whether branch is snoozed at now. An unreadable store never
// snoozes: a stale report is cheaper than a hidden worktree.
func Active(busDir, branch string, now time.Time) bool {
	return read(busDir)[branch] > now.UnixMilli()
}

// Set snoozes branch until `until`, dropping lapsed entries on the way.
func Set(busDir, branch string, now, until time.Time) error {
	m := read(busDir)
	for b, ms := range m {
		if ms <= now.UnixMilli() {
			delete(m, b)
		}
	}
	m[branch] = until.UnixMilli()
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
