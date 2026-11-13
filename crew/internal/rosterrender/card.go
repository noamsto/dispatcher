package rosterrender

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// cardFont is the font size a card's pen widths are drawn for; a card set at a
// larger size scales its pen with it.
const (
	cardFont = 11.0
	ellipsis = "…"
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

// asciiAdvance is the advance of each printable ASCII rune from space, in 1/44
// em: the per-rune maximum of Noto Sans's measured advance and the hmtx
// advance of SourceSans3-Regular, the face resvg draws, rounded up to 1/44 em.
// The backslash is set to a wide guess.
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

// runeEm is a conservative rune width in em: resvg draws Source Sans 3, and
// the estimate deliberately errs wide so text never runs past its box.
func runeEm(r rune) float64 {
	if r < ' ' || r > '~' {
		return wideEm
	}
	return float64(asciiAdvance[r-' ']) / 44 * runeMargin
}

// textWidthAt estimates the rendered width of s at font size font.
func textWidthAt(s string, font float64) float64 {
	var em float64
	for _, r := range s {
		em += runeEm(r)
	}
	return em * font
}

// truncAt cuts s to fit w at font size font, ending in an ellipsis. It cuts at
// a word boundary, dropping a partial last word unless that would lose more
// than half of what fits.
func truncAt(s string, w, font float64) string {
	if textWidthAt(s, font) <= w {
		return s
	}
	rs := []rune(s)
	n := 0
	for used := textWidthAt(ellipsis, font); n < len(rs); n++ {
		used += runeEm(rs[n]) * font
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

// fitPrefix is the longest prefix of s within w at font size font, never
// empty.
func fitPrefix(s string, w, font float64) string {
	rs := []rune(s)
	n := 0
	for used := 0.0; n < len(rs); n++ {
		used += runeEm(rs[n]) * font
		if used > w && n > 0 {
			break
		}
	}
	return string(rs[:n])
}

// wrapAt takes the words of s that fit in w at font size font; a first word
// wider than w is cut by width. It returns the line and the rest of s.
func wrapAt(s string, w, font float64) (line, rest string) {
	s = strings.TrimLeft(s, " ")
	if first, _, _ := strings.Cut(s, " "); textWidthAt(first, font) > w {
		line = fitPrefix(s, w, font)
		return line, s[len(line):]
	}
	rest = s
	for rest != "" {
		word, after, _ := strings.Cut(rest, " ")
		next := word
		if line != "" {
			next = line + " " + word
		}
		if textWidthAt(next, font) > w {
			break
		}
		line, rest = next, after
	}
	return line, rest
}

// cardHead is a card's head text: the codename, then `· #issue` (no `#` for a
// Linear id).
func cardHead(p planet) string {
	head := p.name
	if p.issue != "" {
		head += " · "
		if strings.Trim(p.issue, "0123456789") == "" {
			head += "#"
		}
		head += p.issue
	}
	return head
}

func stateWord(state string) string { return strings.ReplaceAll(clean(state), "_", " ") }

// metaText is a card's tier·engine·model row, `?` for each unknown part.
func metaText(p planet) string {
	return orQ(clean(p.tier)) + "·" + orQ(p.engine) + "·" + orQ(p.model)
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
