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
	ellipsis  = "…"
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

// cardText is the plain text of a card. rows[0] is the head, to which state
// is appended after a gap in the state colour; rows[1:] are the title, tier
// line and detail. Nothing is escaped.
type cardText struct {
	rows  [cardRows]string
	state string
}

// runeEm is a conservative rune width in em for Source Sans 3: Graphviz
// measures with a wider fallback font, so the real text is never wider.
func runeEm(r rune) float64 {
	switch {
	case strings.ContainsRune("ijlI.,:;'|!()[] ", r):
		return 0.28
	case r == 'm' || r == 'w':
		return 0.8
	case r == 'M' || r == 'W':
		return 0.9
	case r >= 'A' && r <= 'Z':
		return 0.62
	case r > unicode.MaxASCII:
		return 0.7
	}
	return 0.52
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

// htmlText escapes s for a Graphviz HTML label.
func htmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
