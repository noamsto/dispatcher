package frame

import (
	"fmt"
	"os"
	"path/filepath"
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
		{"new turn under old recap", join(filler, []string{done, recapStart, "❯ run the deploy", "● Bash(rm -rf build)", "  ⎿  removed", "  Now check the settings in /config)"}), false},
		{"prose ending in config without recap", join(filler, []string{done, "  Now check the settings in /config)"}), false},
		{"marker glyph inside recap", join(filler, []string{done, recapStart, "  ● Bash(x)", recapEnd}), false},
		{"spinner above recap", join(filler, []string{done, "✻ Churning… (3s · ↓ 1k tokens)", recapStart, recapEnd}), false},
	}
	for _, c := range cases {
		if got := doneBehindPostTurn(c.text); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPaneIdleClearHintNoDone(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", "frames", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	plain := read("idle_clear_hint_no_done.txt")
	colored := read("idle_clear_hint_no_done.color.txt")
	for _, skipBg := range []bool{false, true} {
		if reason, idle := PaneIdleReason(plain, colored, skipBg); !idle {
			t.Errorf("fixture skipBg=%v: not idle (%s)", skipBg, reason)
		}
		if reason, idle := PaneIdleReason(plain, "", skipBg); !idle {
			t.Errorf("fixture plain only skipBg=%v: not idle (%s)", skipBg, reason)
		}
	}

	rule := strings.Repeat("─", 20)
	hint := "      new task? /clear to save 1k tokens"
	spinner := "✻ Churning… (3s · ↓ 1k tokens)"
	status := []string{"  status", "  mode"}
	emptyBox := append([]string{rule, "❯ ", rule}, status...)
	var filler []string
	for i := 0; i < 30; i++ {
		filler = append(filler, fmt.Sprintf("  wrapped %d", i))
	}
	join := func(parts ...[]string) string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}
		return strings.Join(all, "\n")
	}
	cases := []struct {
		name   string
		text   string
		skipBg bool
		want   bool
	}{
		{"hint above empty box", join(filler, []string{hint}, emptyBox), true, true},
		{"hint with spinner adjacent", join(filler, []string{spinner, hint, spinner}, emptyBox), true, false},
		{"spinner above hint", join(filler, []string{spinner, "  a", "  b", hint}, emptyBox), true, false},
		{"unsent input in box", join(filler, []string{hint, rule, "❯ draft text", rule}, status), true, false},
		{"other row between hint and box", join(filler, []string{hint, "  prose"}, emptyBox), true, false},
		{"done marker elsewhere with prose", join([]string{"✻ Churned for 3s · done 11:20 AM", "  a", "● Bash(x)", hint}, filler[:20], emptyBox), true, false},
		{"background shell, skipBg false", join(filler, []string{hint, rule, "❯ ", rule, "  1 shell · ← for agents"}), false, false},
		{"esc to interrupt", join(filler, []string{hint, rule, "❯ ", rule, "  esc to interrupt"}), true, false},
		{"permission dialog", join(filler, []string{hint}, strings.Split(read("permission_bash.txt"), "\n")), true, false},
	}
	var todos []string
	for i := 1; i <= 9; i++ {
		todos = append(todos, fmt.Sprintf("  ⎿  ☐ step %d", i))
	}
	for _, sp := range []string{"✻ Churning… (3m · ↑ 12k tokens)", "✻ Churning…", "✶ Thinking… (3m · thinking)", "✻ Dilly-dallying… (3m · ↓ 12k tokens)", "✻ Flambéing… (3m · ↓ 12k tokens)", "✻ Beboppin'… (3s)"} {
		cases = append(cases, struct {
			name   string
			text   string
			skipBg bool
			want   bool
		}{"spinner far above hint: " + sp, join(filler[:10], []string{sp}, todos, []string{hint}, emptyBox), true, false})
	}
	for _, c := range cases {
		if reason, got := PaneIdleReason(c.text, "", c.skipBg); got != c.want {
			t.Errorf("%s: idle=%v (%s), want %v", c.name, got, reason, c.want)
		}
	}
}
