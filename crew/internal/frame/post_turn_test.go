package frame

import (
	"fmt"
	"strings"
	"testing"
)

func TestDoneBehindPostTurn(t *testing.T) {
	rule := strings.Repeat("─", 20)
	box := []string{rule, "❯ ", rule, "  status", "  mode"}
	done := "✻ Churned for 3s · done 11:20 AM"
	hook := "● dispatcher: hooks.json changed — reloaded (3 hooks)"
	recapStart := "※ recap: Goal x"
	recapEnd := "  tail. (disable recaps in /config)"
	hint := "      new task? /clear to save 1k tokens"
	wrap := func(n int) []string {
		var rows []string
		for i := 0; i < n; i++ {
			rows = append(rows, fmt.Sprintf("  wrapped %d", i))
		}
		return rows
	}
	join := func(parts ...[]string) string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}
		return strings.Join(append(all, box...), "\n")
	}
	filler := wrap(8)
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"hook then recap and hint", join(filler, []string{done, hook, recapStart}, wrap(2), []string{recapEnd, hint}), true},
		{"hint alone", join(filler, []string{done, hint}), true},
		{"hook alone", join(filler, []string{done, hook}), true},
		{"single-row recap", join(filler, []string{done, recapStart + " (disable recaps in /config)"}), true},
		{"recap at wrap cap", join(filler, []string{done, recapStart}, wrap(maxRecapRows-1), []string{recapEnd}), true},
		{"recap past wrap cap", join(filler, []string{done, recapStart}, wrap(maxRecapRows), []string{recapEnd}), false},
		{"unknown row below recap", join(filler, []string{done, recapStart, recapEnd, "● Bash(rm -rf x)", hint}), false},
		{"unknown row between hook and recap", join(filler, []string{done, "  prose", hook, recapStart, recapEnd}), false},
		{"hint inside recap", join(filler, []string{done, recapStart, hint, recapEnd}), false},
		{"recap without tail", join(filler, []string{done, recapStart, "  more"}), false},
		{"unanchored hook prose", join(filler, []string{done, "● I saw hooks.json changed — reloaded it"}), false},
		{"no marker", join(filler, []string{hook}), false},
		{"marker beyond window", join([]string{done}, wrap(40), []string{hook}), false},
		{"spinner above recap", join(filler, []string{done, "✻ Churning… (3s · ↓ 1k tokens)", recapStart, recapEnd}), false},
	}
	for _, c := range cases {
		if got := doneBehindPostTurn(c.text); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
