package rosterrender

import (
	"regexp"
	"strings"
	"testing"
)

func cardOf(t *testing.T, mutate func(r map[string]any)) planet {
	t.Helper()
	row := planetRow("feat/12-x", "working")
	mutate(row)
	return onlyPlanet(t, row)
}

func TestCardTextFits(t *testing.T) {
	long := strings.Repeat("alpha beta gamma ", 20)
	tests := []struct {
		name   string
		mutate func(r map[string]any)
		check  func(t *testing.T, c cardText)
	}{
		{"head with issue number", func(r map[string]any) {}, func(t *testing.T, c cardText) {
			if c.rows[0] != "lime · #12" || c.state != "working" {
				t.Errorf("head = %q state = %q", c.rows[0], c.state)
			}
		}},
		{"head with non-numeric id", func(r map[string]any) { r["branch"], r["name"] = "feat/x", "lime·ENG-7" }, func(t *testing.T, c cardText) {
			if c.rows[0] != "lime · ENG-7" {
				t.Errorf("head = %q", c.rows[0])
			}
		}},
		{"head without issue", func(r map[string]any) { r["branch"] = "feat/x" }, func(t *testing.T, c cardText) {
			if c.rows[0] != "lime" {
				t.Errorf("head = %q", c.rows[0])
			}
		}},
		{"state underscore becomes space", func(r map[string]any) { r["state"] = "pr_open" }, func(t *testing.T, c cardText) {
			if c.state != "pr open" {
				t.Errorf("state = %q", c.state)
			}
		}},
		{"state is cleaned", func(r map[string]any) { r["state"] = "wo<rk\"ing_now" }, func(t *testing.T, c cardText) {
			if c.state != "working now" {
				t.Errorf("state = %q", c.state)
			}
		}},
		{"tier is cleaned", func(r map[string]any) { r["tier"] = "st<an\"dard" }, func(t *testing.T, c cardText) {
			if !strings.HasPrefix(c.rows[3], "standard·") {
				t.Errorf("row 4 = %q", c.rows[3])
			}
		}},
		{"short title leaves row 3 empty", func(r map[string]any) { r["title"] = "fix the bug" }, func(t *testing.T, c cardText) {
			if c.rows[1] != "fix the bug" || c.rows[2] != "" {
				t.Errorf("rows = %q %q", c.rows[1], c.rows[2])
			}
		}},
		{"long title wraps then cuts", func(r map[string]any) { r["title"] = long }, func(t *testing.T, c cardText) {
			if c.rows[1] == "" || !strings.HasSuffix(c.rows[2], "…") {
				t.Errorf("rows = %q %q", c.rows[1], c.rows[2])
			}
			if strings.HasSuffix(c.rows[1], "…") || strings.HasSuffix(c.rows[1], " ") {
				t.Errorf("row 2 = %q", c.rows[1])
			}
		}},
		{"over-long word is cut by width", func(r map[string]any) { r["title"] = strings.Repeat("W", 60) + " tail" }, func(t *testing.T, c cardText) {
			if c.rows[1] == "" || !strings.HasPrefix(c.rows[1], "W") || textWidth(c.rows[1]) < cardInner/2 {
				t.Errorf("row 2 = %q", c.rows[1])
			}
			if !strings.HasSuffix(c.rows[2], "…") {
				t.Errorf("row 3 = %q", c.rows[2])
			}
		}},
		{"tier engine model", func(r map[string]any) {
			r["tier"], r["engine"], r["model"] = "deep", "claude", "claude-opus-5-5"
		}, func(t *testing.T, c cardText) {
			if c.rows[3] != "deep·claude·opus" {
				t.Errorf("row 4 = %q", c.rows[3])
			}
		}},
		{"missing engine and model show ?", func(r map[string]any) {}, func(t *testing.T, c cardText) {
			if c.rows[3] != "standard·?·?" {
				t.Errorf("row 4 = %q", c.rows[3])
			}
		}},
		{"long model line is cut", func(r map[string]any) {
			r["engine"], r["model"] = "pi", strings.Repeat("x", 80)
		}, func(t *testing.T, c cardText) {
			if !strings.HasSuffix(c.rows[3], "…") {
				t.Errorf("row 4 = %q", c.rows[3])
			}
		}},
		{"detail is cut to one row", func(r map[string]any) { r["detail"] = long }, func(t *testing.T, c cardText) {
			if !strings.HasSuffix(c.rows[4], "…") {
				t.Errorf("row 5 = %q", c.rows[4])
			}
		}},
		{"null detail leaves row 5 empty", func(r map[string]any) { r["detail"] = nil }, func(t *testing.T, c cardText) {
			if c.rows[4] != "" {
				t.Errorf("row 5 = %q", c.rows[4])
			}
		}},
		{"long head leaves room for the state", func(r map[string]any) {
			r["state"], r["name"] = "blocked", strings.Repeat("W", 24)
		}, func(t *testing.T, c cardText) {
			if !strings.HasSuffix(c.rows[0], "…") {
				t.Errorf("head = %q, want a cut one", c.rows[0])
			}
			if got := textWidth(c.rows[0]) + textWidth("  "+c.state) + 2*cardCellPad; got > cardInner {
				t.Errorf("head+state width %.1f > %.1f", got, cardInner)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := cardOf(t, tt.mutate)
			c := cardTextOf(p)
			for i, row := range c.rows {
				if textWidth(row) > cardInner {
					t.Errorf("row %d %q width %.1f > %.1f", i+1, row, textWidth(row), cardInner)
				}
			}
			tt.check(t, c)
		})
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

func TestHTMLText(t *testing.T) {
	if got, want := htmlText(`<a&"b">`), "&lt;a&amp;&quot;b&quot;&gt;"; got != want {
		t.Errorf("htmlText = %q, want %q", got, want)
	}
}

func TestHTMLTextBackslashStaysLiteral(t *testing.T) {
	tests := []struct{ in, escaped string }{
		{`\N`, `\\N`},
		{`\G`, `\\G`},
		{`C:\Notes`, `C:\\Notes`},
		{`\\`, `\\\\`},
	}
	for _, tt := range tests {
		if got := htmlText(tt.in); got != tt.escaped {
			t.Errorf("htmlText(%q) = %q, want %q", tt.in, got, tt.escaped)
		}
	}

	neato := neatoPath(t)
	textRe := regexp.MustCompile(`<text[^>]*>(.*?)</text>`)
	for _, tt := range tests {
		dot := `graph graphname { n [pos="0,0!" shape=box label=<<table><tr><td>` + htmlText(tt.in) + `</td></tr></table>>]; }`
		m := textRe.FindSubmatch(neatoSVG(t, neato, dot))
		if m == nil || string(m[1]) != tt.in {
			t.Errorf("neato drew %q for %q", m, tt.in)
		}
	}
}

func TestTruncWidth(t *testing.T) {
	if got := truncWidth("short", cardInner); got != "short" {
		t.Errorf("fitting text changed: %q", got)
	}
	got := truncWidth("alpha beta gamma delta epsilon zeta eta theta iota kappa", 150)
	if !strings.HasSuffix(got, "…") || strings.Contains(got, " …") || textWidth(got) > 150 {
		t.Errorf("truncWidth = %q (%.1f)", got, textWidth(got))
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), "e") && !strings.Contains("alpha beta gamma delta epsilon zeta eta theta iota kappa", strings.TrimSuffix(got, "…")+" ") {
		t.Errorf("cut mid-word: %q", got)
	}
}

func TestTruncWidthCountsWideSourceSansGlyphs(t *testing.T) {
	if got := textWidth("`"); got < 0.542*cardFont {
		t.Errorf("backtick width %.2f is narrower than Source Sans 3 draws it", got)
	}
	// truncWidth measures with textWidth, so a narrowed table would still pass
	// the truncation check below; the per-glyph floor is what catches it.
	for r, adv := range map[rune]int{'`': 542, 'J': 480, '\'': 249, '(': 303, ')': 303, '!': 289, '%': 824, '"': 426, '/': 350, '?': 425, '_': 500} {
		if got := runeEm(r); got < float64(adv)/1000 {
			t.Errorf("%q width %.3f em is narrower than Source Sans 3's advance %.3f", r, got, float64(adv)/1000)
		}
	}
	row := strings.Repeat("`JJJ` `fix` `JSON` ", 6)
	got := truncWidth(row, cardInner)
	if !strings.HasSuffix(got, "…") || textWidth(got) > cardInner {
		t.Errorf("truncWidth = %q (%.1f > %.1f)", got, textWidth(got), cardInner)
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
	want := cardText{
		rows:  [cardRows]string{"lime · #12", "fix the bug", "", "deep·claude·opus", "running tests"},
		state: "pr open",
	}
	if c := cardTextOf(p); c != want {
		t.Errorf("card = %+v, want %+v", c, want)
	}
}
