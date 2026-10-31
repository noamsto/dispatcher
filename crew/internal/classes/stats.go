package classes

import "encoding/json"

// Key identifies one model within an engine and tier.
type Key struct{ Engine, Tier, Model string }

// Stats aggregates the rated runs of one Key.
type Stats struct {
	N, Merged, Failed, Costed int
	ReworkN                   int
	ReworkSum, CostMS         float64
}

// Rated reports whether row is a run with a settled outcome: an owned PR that
// was merged or closed, or a PR-less run that failed. Open, unreconciled,
// duplicate (owns_pr false) and PR-less done runs say nothing about the model.
func Rated(row map[string]any) bool {
	for _, f := range []string{"engine", "model", "tier"} {
		if s, _ := row[f].(string); s == "" {
			return false
		}
	}
	if row["task_kind"] == "review" {
		return false
	}
	if row["owns_pr"] == true {
		state, _ := row["pr_state"].(string)
		return state == "MERGED" || state == "CLOSED"
	}
	if url, _ := row["pr_url"].(string); url != "" {
		return false
	}
	return row["terminal_state"] == "failed"
}

// Collect folds the rated rows into per-model stats; other rows are skipped.
func Collect(rows []any) map[Key]*Stats {
	out := map[Key]*Stats{}
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok || !Rated(row) {
			continue
		}
		k := Key{row["engine"].(string), row["tier"].(string), row["model"].(string)}
		s := out[k]
		if s == nil {
			s = &Stats{}
			out[k] = s
		}
		s.N++
		if row["pr_state"] == "MERGED" {
			s.Merged++
		}
		if row["owns_pr"] != true {
			s.Failed++
		}
		if v, ok := number(row["rework_count"]); ok {
			s.ReworkN++
			s.ReworkSum += v
		}
		if v, ok := number(row["cost_proxy"]); ok {
			s.Costed++
			s.CostMS += v
		}
	}
	return out
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// MergeRate is the share of rated runs whose PR merged.
func (s Stats) MergeRate() (float64, bool) {
	if s.N == 0 {
		return 0, false
	}
	return float64(s.Merged) / float64(s.N), true
}

// Rework is the mean rework count over runs that recorded one.
func (s Stats) Rework() (float64, bool) {
	if s.ReworkN == 0 {
		return 0, false
	}
	return s.ReworkSum / float64(s.ReworkN), true
}

// BurnPerMergedHours is the weighted wall-clock spent per merged PR, in hours.
// It is measured only when every rated run is costed: an uncosted run would
// add to the merged count and not to the cost, reading too cheap.
func (s Stats) BurnPerMergedHours() (float64, bool) {
	if s.Merged == 0 || s.Costed != s.N {
		return 0, false
	}
	return s.CostMS / float64(s.Merged) / 3_600_000, true
}
