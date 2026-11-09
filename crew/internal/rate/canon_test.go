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
			name:   "fable absorbs its full id",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"fable", "claude-fable-5-1"},
			want:   map[string]string{"fable": "fable", "claude-fable-5-1": "fable"},
		},
		{
			name:   "only the newest undated generation folds",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"opus", "claude-opus-5", "claude-opus-5-5"},
			want:   map[string]string{"opus": "opus", "claude-opus-5": "claude-opus-5", "claude-opus-5-5": "opus"},
		},
		{
			name:   "dated snapshot never folds",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"opus", "claude-opus-4-5-20251101"},
			want:   map[string]string{"opus": "opus", "claude-opus-4-5-20251101": "claude-opus-4-5-20251101"},
		},
		{
			name:   "dated-only family does not fold",
			engine: "claude",
			tier:   "standard",
			ids:    []string{"claude-sonnet-5-5-20251101"},
			want:   map[string]string{"claude-sonnet-5-5-20251101": "claude-sonnet-5-5-20251101"},
		},
		{
			name:   "a higher dated id does not outrank the newest undated one",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"opus", "claude-opus-5-5", "claude-opus-6-20260101"},
			want:   map[string]string{"opus": "opus", "claude-opus-5-5": "opus", "claude-opus-6-20260101": "claude-opus-6-20260101"},
		},
		{
			name:   "empty glob remainder does not fold",
			engine: "claude",
			tier:   "deep",
			ids:    []string{"opus", "claude-opus-"},
			want:   map[string]string{"opus": "opus", "claude-opus-": "claude-opus-"},
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonIDs(tc.engine, tc.tier, tc.ids); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("canonIDs(%s, %s, %v) = %v, want %v", tc.engine, tc.tier, tc.ids, got, tc.want)
			}
		})
	}
}

func TestCanonVer(t *testing.T) {
	pad := func(nums ...int) []int {
		out := make([]int, 8)
		for i := range out {
			out[i] = -1
		}
		copy(out, nums)
		return out
	}
	cases := []struct {
		id   string
		want []int
	}{
		{"a/1.2.3", pad(1, 2, 3)},
		{"opus", pad()},
		{"gpt-5.6-sol", pad(5, 6)},
		{"x-10-2", pad(10, 2)},
		{"openrouter/anthropic/claude-opus-4.1", pad(4, 1)},
		{"", pad()},
	}
	for _, tc := range cases {
		if got := ver(tc.id); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ver(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestCanonUndated(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"claude-opus-5-5", true},
		{"claude-opus-5-1", true},
		{"claude-opus-4-5-20251101", false},
		{"claude-opus-4-5-2025", false},
		{"claude-opus-5-preview", false},
		{"claude-3-beta-4", false},
		{"claude.exp.3", false},
		{"preview", false},
		{"claudepreview", true},
	}
	for _, tc := range cases {
		if got := undated(tc.id); got != tc.want {
			t.Errorf("undated(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
