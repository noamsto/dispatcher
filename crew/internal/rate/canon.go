package rate

import (
	"regexp"
	"strconv"
	"strings"
)

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

// canonIDs maps each id to the alias that absorbs it. Within a family only the
// highest-versioned undated id matching the family glob folds into the alias;
// every other id, and every id for an engine with no families, maps to itself.
func canonIDs(engine, tier string, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = id
	}
	for alias, glob := range families(engine, tier) {
		stem := strings.TrimSuffix(glob, "*")
		best := ""
		var bestVer []int
		for _, id := range ids {
			if !strings.HasPrefix(id, stem) || len(id) <= len(stem) {
				continue
			}
			if !admitGlobMatch(glob, id) || !undated(id) {
				continue
			}
			v := ver(id)
			if best == "" || compareVer(v, bestVer) > 0 {
				best, bestVer = id, v
			}
		}
		if best != "" {
			out[best] = alias
		}
	}
	return out
}

// datedTail reports a snapshot id tail: -(YYYY|YYYYMMDD).
var datedTail = regexp.MustCompile(`-([0-9]{4}|[0-9]{8})$`)

// prereleaseSegment reports preview/exp/beta on a segment boundary.
var prereleaseSegment = regexp.MustCompile(`(\A|[-./_])(preview|exp|beta)([-./_]|\z)`)

// undated ports model-resolve's `undated`: the id names a floating model, not
// a dated snapshot or a prerelease.
func undated(id string) bool {
	return !datedTail.MatchString(id) && !prereleaseSegment.MatchString(id)
}

// ver ports model-resolve's `ver`: the last '/'-separated segment, every run of
// digits as a number, padded to 8 slots with -1.
func ver(id string) []int {
	seg := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		seg = id[i+1:]
	}
	out := make([]int, 8)
	for i := range out {
		out[i] = -1
	}
	n := 0
	for i := 0; i < len(seg); {
		if seg[i] < '0' || seg[i] > '9' {
			i++
			continue
		}
		j := i
		for j < len(seg) && seg[j] >= '0' && seg[j] <= '9' {
			j++
		}
		if n < len(out) {
			v, err := strconv.Atoi(seg[i:j])
			if err == nil {
				out[n] = v
			}
			n++
		}
		i = j
	}
	return out
}

// compareVer compares two ver results segment-wise, missing padded to -1.
func compareVer(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := -1, -1
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}
