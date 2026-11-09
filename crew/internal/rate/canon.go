package rate

import "strings"

// aliasCurrentID names, per claude alias, the full id that alias resolves to
// today. Staleness under-merges: an id missing here keeps its own row, which
// is the safe direction (a split sample, never a wrong attribution). Update
// it when the engine ships a new generation of a family.
var aliasCurrentID = map[string]string{
	"opus":   "claude-opus-5-5",
	"sonnet": "claude-sonnet-5-5",
	"haiku":  "claude-haiku-5-5",
	"fable":  "claude-fable-5-1",
}

// families maps a floating alias of the seed row to the trailing-* glob that
// names its family (D3): the glob's stem, minus one trailing '-', must equal an
// exact entry of the same row or end with "-"+entry; the longest entry wins.
func families(engine, tier string) map[string]string {
	t := admitTableOnce()
	row, ok := t.rows[engine][tier]
	if !ok || row == nil {
		return map[string]string{}
	}

	exact := make([]string, 0, len(row.modelGlobs))
	for _, m := range row.modelGlobs {
		if !strings.HasSuffix(m, "*") {
			exact = append(exact, m)
		}
	}

	out := map[string]string{}
	for _, g := range row.modelGlobs {
		if !strings.HasSuffix(g, "*") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(g, "*"), "-")
		if stem == "" {
			continue
		}
		best := ""
		for _, a := range exact {
			if a == "" {
				continue
			}
			if (stem == a || strings.HasSuffix(stem, "-"+a)) && len(a) > len(best) {
				best = a
			}
		}
		if best != "" {
			out[best] = g
		}
	}
	return out
}

// canonIDs maps each id to the alias that absorbs it. An alias absorbs exactly
// the full id aliasCurrentID names for it today, and only at an engine+tier
// whose seed row lists that alias; every other id maps to itself. So an older
// generation, a dated snapshot, an id the table does not name, and every id of
// an engine with no aliases keep their own rows.
func canonIDs(engine, tier string, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = id
		present[id] = true
	}
	for alias := range families(engine, tier) {
		full, ok := aliasCurrentID[alias]
		if ok && present[full] {
			out[full] = alias
		}
	}
	return out
}
