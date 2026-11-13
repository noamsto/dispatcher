package rosterrender

import (
	"strings"
	"testing"
)

func cardOf(t *testing.T, mutate func(r map[string]any)) planet {
	t.Helper()
	row := planetRow("feat/12-x", "working")
	mutate(row)
	return onlyPlanet(t, row)
}

func TestCardText(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(r map[string]any)
		check  func(t *testing.T, p planet)
	}{
		{"head with issue number", func(r map[string]any) {}, func(t *testing.T, p planet) {
			if cardHead(p) != "lime · #12" || stateWord(p.state) != "working" {
				t.Errorf("head = %q state = %q", cardHead(p), stateWord(p.state))
			}
		}},
		{"head with non-numeric id", func(r map[string]any) { r["branch"], r["name"] = "feat/x", "lime·ENG-7" }, func(t *testing.T, p planet) {
			if cardHead(p) != "lime · ENG-7" {
				t.Errorf("head = %q", cardHead(p))
			}
		}},
		{"head without issue", func(r map[string]any) { r["branch"] = "feat/x" }, func(t *testing.T, p planet) {
			if cardHead(p) != "lime" {
				t.Errorf("head = %q", cardHead(p))
			}
		}},
		{"state underscore becomes space", func(r map[string]any) { r["state"] = "pr_open" }, func(t *testing.T, p planet) {
			if stateWord(p.state) != "pr open" {
				t.Errorf("state = %q", stateWord(p.state))
			}
		}},
		{"state is cleaned", func(r map[string]any) { r["state"] = "wo<rk\"ing_now" }, func(t *testing.T, p planet) {
			if stateWord(p.state) != "working now" {
				t.Errorf("state = %q", stateWord(p.state))
			}
		}},
		{"tier is cleaned", func(r map[string]any) { r["tier"] = "st<an\"dard" }, func(t *testing.T, p planet) {
			if !strings.HasPrefix(metaText(p), "standard·") {
				t.Errorf("meta = %q", metaText(p))
			}
		}},
		{"tier engine model", func(r map[string]any) {
			r["tier"], r["engine"], r["model"] = "deep", "claude", "claude-opus-5-5"
		}, func(t *testing.T, p planet) {
			if metaText(p) != "deep·claude·opus" {
				t.Errorf("meta = %q", metaText(p))
			}
		}},
		{"missing engine and model show ?", func(r map[string]any) {}, func(t *testing.T, p planet) {
			if metaText(p) != "standard·?·?" {
				t.Errorf("meta = %q", metaText(p))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, cardOf(t, tt.mutate))
		})
	}
}

func TestWrapAt(t *testing.T) {
	const w = 180.0
	if line, rest := wrapAt("fix the bug", w, cardFont); line != "fix the bug" || rest != "" {
		t.Errorf("short = %q, %q", line, rest)
	}
	line, rest := wrapAt(strings.Repeat("alpha beta gamma ", 20), w, cardFont)
	if line == "" || rest == "" || strings.HasSuffix(line, " ") || textWidthAt(line, cardFont) > w {
		t.Errorf("long = %q, %q", line, rest)
	}
	line, rest = wrapAt(strings.Repeat("W", 60)+" tail", w, cardFont)
	if !strings.HasPrefix(line, "W") || textWidthAt(line, cardFont) > w || textWidthAt(line, cardFont) < w/2 || !strings.HasSuffix(rest, " tail") {
		t.Errorf("over-long word = %q, %q", line, rest)
	}
}

func TestModelAlias(t *testing.T) {
	tests := map[string]string{
		"claude-opus-5-5":              "opus",
		"anthropic/claude-sonnet-4-5":  "sonnet",
		"deepseek/deepseek-v4.1-flash": "deepseek-v4.1-flash",
		"claude-haiku-4":               "haiku",
		"":                             "",
	}
	for in, want := range tests {
		if got := modelAlias(in); got != want {
			t.Errorf("modelAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"control chars dropped", "a\x00b\x1b[31mc\u200bd", "ab[31mcd"},
		{"whitespace collapsed", "  a \t\n b   c  ", "a b c"},
		{"empty", "", ""},
		{"capped", strings.Repeat("a", maxFreeText+50), strings.Repeat("a", maxFreeText)},
		{"keeps html syntax for later escaping", `<a&"b">`, `<a&"b">`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeText(tt.in); got != tt.want {
				t.Errorf("sanitizeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncAt(t *testing.T) {
	if got := truncAt("short", 150, cardFont); got != "short" {
		t.Errorf("fitting text changed: %q", got)
	}
	got := truncAt("alpha beta gamma delta epsilon zeta eta theta iota kappa", 150, cardFont)
	if !strings.HasSuffix(got, "…") || strings.Contains(got, " …") || textWidthAt(got, cardFont) > 150 {
		t.Errorf("truncAt = %q (%.1f)", got, textWidthAt(got, cardFont))
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), "e") && !strings.Contains("alpha beta gamma delta epsilon zeta eta theta iota kappa", strings.TrimSuffix(got, "…")+" ") {
		t.Errorf("cut mid-word: %q", got)
	}
}

func TestTruncAtCountsWideSourceSansGlyphs(t *testing.T) {
	if got := textWidthAt("`", cardFont); got < 0.542*cardFont {
		t.Errorf("backtick width %.2f is narrower than Source Sans 3 draws it", got)
	}
	// truncAt measures with textWidthAt, so a narrowed table would still pass
	// the truncation check below; the per-glyph floor is what catches it.
	for r, adv := range map[rune]int{'`': 542, 'J': 480, '\'': 249, '(': 303, ')': 303, '!': 289, '%': 824, '"': 426, '/': 350, '?': 425, '_': 500} {
		if got := runeEm(r); got < float64(adv)/1000 {
			t.Errorf("%q width %.3f em is narrower than Source Sans 3's advance %.3f", r, got, float64(adv)/1000)
		}
	}
	row := strings.Repeat("`JJJ` `fix` `JSON` ", 6)
	const w = 180.0
	got := truncAt(row, w, cardFont)
	if !strings.HasSuffix(got, "…") || textWidthAt(got, cardFont) > w {
		t.Errorf("truncAt = %q (%.1f > %.1f)", got, textWidthAt(got, cardFont), w)
	}
}

func TestCardPen(t *testing.T) {
	tests := map[string]float64{"deep": 4.5, "standard": 3, "trivial": 1.75, "": 3, "x": 3}
	for tier, want := range tests {
		if got := cardPen(tier); got != want {
			t.Errorf("cardPen(%q) = %v, want %v", tier, got, want)
		}
	}
}

// TestCardFieldsMatchDashBoard pins the card text of a fixture that
// dash/internal/ui/board_test.go (TestBoardCardFieldsMatchSVGCard) asserts for
// the `crew dash` Board card: the two modules cannot share code, so keep the
// fixture and the expected strings in step.
func TestCardFieldsMatchDashBoard(t *testing.T) {
	p := cardOf(t, func(r map[string]any) {
		r["branch"], r["name"], r["state"] = "feat/12-x", "lime", "pr_open"
		r["title"], r["tier"], r["engine"] = "fix the bug", "deep", "claude"
		r["model"], r["detail"] = "claude-opus-5-5", "running tests"
	})
	got := [5]string{cardHead(p), p.title, metaText(p), p.detail, stateWord(p.state)}
	if want := [5]string{"lime · #12", "fix the bug", "deep·claude·opus", "running tests", "pr open"}; got != want {
		t.Errorf("card = %q, want %q", got, want)
	}
}
