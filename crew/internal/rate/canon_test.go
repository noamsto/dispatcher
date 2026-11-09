package rate

import (
	"reflect"
	"sort"
	"testing"
)

// TestCanonFamilies pins D3's mechanical derivation from the seed row. No claude
// tier lists all four aliases (deep has fable but no haiku; standard/trivial the
// reverse), so the four are the union across tiers.
func TestCanonFamilies(t *testing.T) {
	want := map[string]map[string]string{
		"deep": {
			"opus":   "claude-opus-*",
			"sonnet": "claude-sonnet-*",
			"fable":  "claude-fable-*",
		},
		"standard": {
			"opus":   "claude-opus-*",
			"sonnet": "claude-sonnet-*",
			"haiku":  "claude-haiku-*",
		},
		"trivial": {
			"opus":   "claude-opus-*",
			"sonnet": "claude-sonnet-*",
			"haiku":  "claude-haiku-*",
		},
	}
	for tier, w := range want {
		if got := families("claude", tier); !reflect.DeepEqual(got, w) {
			t.Errorf("families(claude, %s) = %v, want %v", tier, got, w)
		}
	}

	union := map[string]bool{}
	for _, tier := range []string{"deep", "standard", "trivial"} {
		for alias := range families("claude", tier) {
			union[alias] = true
		}
	}
	var gotAliases []string
	for alias := range union {
		gotAliases = append(gotAliases, alias)
	}
	sort.Strings(gotAliases)
	if want := []string{"fable", "haiku", "opus", "sonnet"}; !reflect.DeepEqual(gotAliases, want) {
		t.Errorf("union of claude families = %v, want %v", gotAliases, want)
	}

	// No trailing-* glob in the codex/cursor/pi rows: nothing folds there.
	for _, engine := range []string{"codex", "cursor", "pi"} {
		for _, tier := range []string{"deep", "standard", "trivial"} {
			if got := families(engine, tier); len(got) != 0 {
				t.Errorf("families(%s, %s) = %v, want none", engine, tier, got)
			}
		}
	}
}

func TestCanonIDs(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		tier   string
		ids    []string
		want   map[string]string
	}{
		{
			name:   "the current full id folds into its alias",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"fable", "claude-fable-5-1"},
			want:   map[string]string{"fable": "fable", "claude-fable-5-1": "fable"},
		},
		{
			name:   "the current full id folds with the older generation present",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"opus", "claude-opus-5", "claude-opus-5-5"},
			want: map[string]string{
				"opus": "opus", "claude-opus-5": "claude-opus-5", "claude-opus-5-5": "opus",
			},
		},
		{
			name:   "an older generation alone folds nothing",
			engine: "claude",
			tier:   "standard",
			ids:    []string{"sonnet", "claude-sonnet-5"},
			want:   map[string]string{"sonnet": "sonnet", "claude-sonnet-5": "claude-sonnet-5"},
		},
		{
			name:   "the current full id folds though the alias has no runs",
			engine: "claude",
			tier:   "standard",
			ids:    []string{"claude-sonnet-5-5"},
			want:   map[string]string{"claude-sonnet-5-5": "sonnet"},
		},
		{
			name:   "a full id with no table entry never folds",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"opus", "claude-opus-5"},
			want:   map[string]string{"opus": "opus", "claude-opus-5": "claude-opus-5"},
		},
		{
			name:   "a dated snapshot never folds",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"opus", "claude-opus-4-5-20251101"},
			want:   map[string]string{"opus": "opus", "claude-opus-4-5-20251101": "claude-opus-4-5-20251101"},
		},
		{
			name:   "a dated snapshot of the current id never folds",
			engine: "claude",
			tier:   "standard",
			ids:    []string{"claude-sonnet-5-5-20251101"},
			want:   map[string]string{"claude-sonnet-5-5-20251101": "claude-sonnet-5-5-20251101"},
		},
		{
			name:   "an alias the tier's row does not list folds nothing",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"haiku", "claude-haiku-5-5"},
			want:   map[string]string{"haiku": "haiku", "claude-haiku-5-5": "claude-haiku-5-5"},
		},
		{
			name:   "an alias only another tier lists folds nothing here",
			engine: "claude",
			tier:   "standard",
			ids:    []string{"fable", "claude-fable-5-1"},
			want:   map[string]string{"fable": "fable", "claude-fable-5-1": "claude-fable-5-1"},
		},
		{
			name:   "pi ids all map to themselves",
			engine: "pi",
			tier:   "standard",
			ids: []string{
				"anthropic/claude-opus-5-5",
				"openrouter/anthropic/claude-opus-5.5",
				"openrouter/deepseek/deepseek-v4.1-flash",
			},
			want: map[string]string{
				"anthropic/claude-opus-5-5":               "anthropic/claude-opus-5-5",
				"openrouter/anthropic/claude-opus-5.5":    "openrouter/anthropic/claude-opus-5.5",
				"openrouter/deepseek/deepseek-v4.1-flash": "openrouter/deepseek/deepseek-v4.1-flash",
			},
		},
		{
			name:   "codex and cursor ids all map to themselves",
			engine: "codex",
			tier:   "deep",
			ids:    []string{"gpt-5.6-sol", "gpt-5.5"},
			want:   map[string]string{"gpt-5.6-sol": "gpt-5.6-sol", "gpt-5.5": "gpt-5.5"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonIDs(tc.engine, tc.tier, tc.ids); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("canonIDs(%s, %s, %v) = %v, want %v", tc.engine, tc.tier, tc.ids, got, tc.want)
			}
		})
	}
}

// TestAliasCurrentIDMatchesTheSeedRow guards the hand-kept table: every alias
// must still be listed in some engine+tier seed row and every full id must
// still match that alias's family glob in that row. The guard proves an entry
// lies in that alias's family glob, not that it is still the current id; it
// catches a rename or a typo'd family, not a stale generation.
func TestAliasCurrentIDMatchesTheSeedRow(t *testing.T) {
	for alias, full := range aliasCurrentID {
		found := false
		for engine, tiers := range admitTableOnce().rows {
			for tier, row := range tiers {
				if row == nil {
					continue
				}
				glob, ok := families(engine, tier)[alias]
				if !ok {
					continue
				}
				found = true
				if !admitGlobMatch(glob, full) {
					t.Errorf("aliasCurrentID[%q] = %q does not match that row's family glob %q (%s %s)",
						alias, full, glob, engine, tier)
				}
			}
		}
		if !found {
			t.Errorf("aliasCurrentID[%q]: no engine+tier seed row lists that alias", alias)
		}
	}
}
