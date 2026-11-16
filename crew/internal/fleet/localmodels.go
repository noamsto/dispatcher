package fleet

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

const localModelFormat = "#{@crew_model}\x1f#{pane_current_command}"

// LocalModel is one localModels entry: its concurrency cap and the engine
// panes running on it.
type LocalModel struct {
	ID            string `json:"id"`
	MaxConcurrent int    `json:"max_concurrent"`
	InUse         int    `json:"in_use"`
}

// localModelUse counts, per entry, the engine panes stamped with its model.
// It scans every pane, role panes included: a local model's slots are held by
// any engine on it, not only by leads.
func localModelUse(ctx context.Context, c tmux.Client, entries map[string]int) []LocalModel {
	use := map[string]int{}
	for line := range strings.SplitSeq(tmux.Output(ctx, c, "list-panes", "-a", "-F", localModelFormat), "\n") {
		model, cmd, ok := strings.Cut(line, "\x1f")
		if _, listed := entries[model]; ok && listed && roster.IsEngineCmd(cmd) {
			use[model]++
		}
	}
	models := make([]LocalModel, 0, len(entries))
	for id, max := range entries {
		models = append(models, LocalModel{ID: id, MaxConcurrent: max, InUse: use[id]})
	}
	slices.SortFunc(models, func(a, b LocalModel) int { return cmp.Compare(a.ID, b.ID) })
	return models
}

// ResolveLocalModels runs dispatch-config and reads localModels: each entry's
// maxConcurrent floored, 1 when absent.
func ResolveLocalModels(ctx context.Context, bin string, stderr io.Writer) (map[string]int, error) {
	out, err := configJSON(ctx, bin, stderr)
	if err != nil {
		return nil, err
	}
	var settings struct {
		LocalModels map[string]struct {
			MaxConcurrent *float64 `json:"maxConcurrent"`
		} `json:"localModels"`
	}
	if err := json.Unmarshal(out, &settings); err != nil {
		return nil, fmt.Errorf("settings localModels: %v", err)
	}
	entries := make(map[string]int, len(settings.LocalModels))
	for id, m := range settings.LocalModels {
		n := 1.0
		if m.MaxConcurrent != nil {
			n = math.Floor(*m.MaxConcurrent)
		}
		entries[id] = int(n)
	}
	return entries, nil
}
