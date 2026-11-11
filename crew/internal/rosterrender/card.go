package rosterrender

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Card geometry, shared by the label text and the layout.
const (
	cardW    = 2.6 // inches
	cardH    = 1.3 // inches
	cardFont = 11.0
	cardPadX = 8.0 // points, each side

	// cardCellPad is the label table's cell padding in points.
	cardCellPad = 1

	cardInner = cardW*72 - 2*cardPadX // text width in points
	cardRows  = 5

	// cardTableH is the label table's height in points: five rows at cardFont
	// with cardCellPad, measured at 88.7 by neato with Noto Sans, rounded up.
	// The table is pinned to cardInner by cardTableH so its left edge, where the
	// text starts, does not depend on how Graphviz measures the text.
	cardTableH = 89.0
	ellipsis   = "…"
)

// cardPen is the border weight of a tier; an unknown tier draws as standard.
func cardPen(tier string) float64 {
	switch tier {
	case "deep":
		return 4.5
	case "trivial":
		return 1.75
	}
	return planetPenwidth
}

// cardText is the unescaped text of a card. rows[0] is the head, to which state
// is appended after a gap in the state colour.
type cardText struct {
	rows  [cardRows]string
	state string
}

// asciiAdvance is the advance of each printable ASCII rune from space, in 1/44
// em: the per-rune maximum of Graphviz's own measurement (what fontconfig and
// pango resolve "Source Sans 3" to, Noto Sans here; a plaintext node of 20
// repeats per rune through `neato -Tplain`, each advance rounded up to a whole
// 0.75pt) and the hmtx advance of SourceSans3-Regular, the face resvg draws,
// rounded up to 1/44 em. The backslash, which Graphviz reads as an escape, is
// set to a wide guess.
var asciiAdvance = [...]uint8{
	12, 13, 19, 27, 24, 37, 33, 11, 14, 14, 24, 24, 12, 15, 12, 16, // space to /
	24, 24, 24, 24, 24, 24, 24, 24, 24, 24, 12, 12, 24, 24, 24, 19, // 0 to ?
	39, 27, 30, 27, 33, 24, 24, 33, 33, 15, 22, 27, 24, 39, 33, 33, // @ to O
	27, 33, 27, 24, 24, 33, 27, 42, 27, 24, 24, 15, 24, 15, 24, 22, // P to _
	24, 24, 27, 21, 27, 24, 15, 27, 27, 12, 12, 24, 12, 42, 27, 27, // ` to o
	27, 27, 18, 21, 15, 27, 21, 36, 24, 21, 21, 18, 24, 18, 24, // p to ~
}

const (
	// runeMargin pads every advance, for a font that renders a little wider.
	runeMargin = 1.03

	// wideEm is the width assumed for a rune outside ASCII.
	wideEm = 1.0
)

// runeEm is a conservative rune width in em, bounding both what Graphviz
// measures and what resvg draws in Source Sans 3.
func runeEm(r rune) float64 {
	if r < ' ' || r > '~' {
		return wideEm
	}
	return float64(asciiAdvance[r-' ']) / 44 * runeMargin
}

// textWidth estimates the rendered width of s in points at cardFont.
func textWidth(s string) float64 {
	var em float64
	for _, r := range s {
		em += runeEm(r)
	}
	return em * cardFont
}

// truncWidth cuts s to fit w points, ending in an ellipsis. It cuts at a word
// boundary, dropping a partial last word unless that would lose more than half
// of what fits.
func truncWidth(s string, w float64) string {
	if textWidth(s) <= w {
		return s
	}
	rs := []rune(s)
	n := 0
	for used := textWidth(ellipsis); n < len(rs); n++ {
		used += runeEm(rs[n]) * cardFont
		if used > w {
			break
		}
	}
	cut := string(rs[:n])
	if n < len(rs) && !unicode.IsSpace(rs[n]) {
		if i := strings.LastIndexByte(cut, ' '); i >= 0 && utf8.RuneCountInString(cut[:i]) >= n/2 {
			cut = cut[:i]
		}
	}
	return strings.TrimRight(cut, " ") + ellipsis
}

// fitPrefix is the longest prefix of s within w points, never empty.
func fitPrefix(s string, w float64) string {
	rs := []rune(s)
	n := 0
	for used := 0.0; n < len(rs); n++ {
		used += runeEm(rs[n]) * cardFont
		if used > w && n > 0 {
			break
		}
	}
	return string(rs[:n])
}

// wrapLine takes the words of s that fit in w points; a first word wider than
// w is cut by width. It returns the line and the rest of s.
func wrapLine(s string, w float64) (line, rest string) {
	s = strings.TrimLeft(s, " ")
	if first, _, _ := strings.Cut(s, " "); textWidth(first) > w {
		line = fitPrefix(s, w)
		return line, s[len(line):]
	}
	rest = s
	for rest != "" {
		word, after, _ := strings.Cut(rest, " ")
		next := word
		if line != "" {
			next = line + " " + word
		}
		if textWidth(next) > w {
			break
		}
		line, rest = next, after
	}
	return line, rest
}

// cardTextOf lays out the five rows of p's card, each within cardInner.
func cardTextOf(p planet) cardText {
	var c cardText
	c.state = strings.ReplaceAll(clean(p.state), "_", " ")

	head := p.name
	if p.issue != "" {
		head += " · "
		if strings.Trim(p.issue, "0123456789") == "" {
			head += "#"
		}
		head += p.issue
	}
	gap := textWidth("  ") + textWidth(c.state)
	c.rows[0] = truncWidth(head, cardInner-gap)

	line, rest := wrapLine(p.title, cardInner)
	c.rows[1] = line
	c.rows[2] = truncWidth(strings.TrimSpace(rest), cardInner)

	c.rows[3] = truncWidth(orQ(clean(p.tier))+"·"+orQ(p.engine)+"·"+orQ(p.model), cardInner)
	c.rows[4] = truncWidth(p.detail, cardInner)
	return c
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// htmlText escapes s for a Graphviz HTML label, whose text still expands
// backslash escapes such as \N and \G.
func htmlText(s string) string {
	return strings.NewReplacer(`\`, `\\`, "&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
