package rosterrender

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
)

// The cache trust bounds mirror fleet.budgetSummary: a cache older than two hours
// or more than five minutes ahead of the wall clock tells nothing.
const (
	maxBudgetAge  = 7200
	maxBudgetSkew = 300
)

// readEngineWindows is each engine's most-used live quota window from the quota
// cache: claude, codex, cursor and pi always, then any other engine the cache
// names. What the cache cannot vouch for reads as unknown (nil pct), never zero:
// a missing, unparsable, stale or future-dated cache, an engine entry that is null
// or holds only pi's `anthropic` credit estimate, or one with no window still
// ahead of its reset.
func readEngineWindows(path string, now int64) []engineReading {
	var cache struct {
		Fetched any            `json:"fetched_epoch"`
		Engines map[string]any `json:"engines"`
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	fetched, haveEpoch := cache.Fetched.(float64)
	trusted := haveEpoch && fetched >= float64(now-maxBudgetAge) && fetched <= float64(now+maxBudgetSkew)

	names := slices.Clone(engines)
	var extra []string
	for name := range cache.Engines {
		if !slices.Contains(names, name) {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)

	out := make([]engineReading, 0, len(names))
	for _, name := range names {
		r := engineReading{name: name}
		if en, ok := cache.Engines[name].(map[string]any); ok && trusted {
			r.window, r.pct = bestWindow(en, now)
		}
		out = append(out, r)
	}
	return out
}

// bestWindow is the highest-used window still ahead of its reset, ties to the
// first name.
func bestWindow(en map[string]any, now int64) (string, *float64) {
	if _, has := en["anthropic"]; has && len(en) == 1 {
		return "", nil
	}
	windows, _ := en["windows"].(map[string]any)
	keys := make([]string, 0, len(windows))
	for k := range windows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var (
		name string
		best *float64
	)
	for _, k := range keys {
		win, _ := windows[k].(map[string]any)
		pct, ok := win["used_pct"].(float64)
		if !ok {
			continue
		}
		if resets, ok := win["resets_at"].(float64); ok && resets <= float64(now) {
			continue
		}
		if best == nil || pct > *best {
			name, best = k, &pct
		}
	}
	return name, best
}
