// Package ui is the interactive Bubble Tea program. Not built in G1 — the
// data and once packages are the whole read-only contract this slice ships;
// Run is a stub G2 replaces with the real tea.Program.
package ui

import (
	"errors"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// Run starts the interactive dashboard. collect re-collects a fresh
// Snapshot (bound to "r" and the roster bus watcher in G2+).
func Run(initial data.Snapshot, collect func() data.Snapshot) error {
	_ = initial
	_ = collect
	return errors.New("interactive mode not built yet")
}
