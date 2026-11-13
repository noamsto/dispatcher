package rosterrender

import (
	"flag"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the testdata goldens")

// modelNow is the wall clock a hand-built model is read at.
const modelNow = 1_800_000_000.0

// emit draws a model's board as the pass would, with no slots yet.
func emit(m map[string]any) (string, board, legend) {
	ps, lg := buildPlanets(m, modelNow)
	b, _ := buildBoard(ps, buildHolds(m), buildQueued(m), newSlotState(), lg)
	return renderBoardSVG(b, layoutBoard(b, defaultAspect), headerInfo{crew: "c", strip: stripText(lg)}, selection{}), b, lg
}

// cardCount is how many cards a drawn svg holds, holds and queued tasks included.
func cardCount(svg string) int { return strings.Count(svg, `<g id="card-`) }

// text is every text under n, in drawing order, joined by " | ".
func (n xnode) text() string {
	var parts []string
	n.walk(func(c xnode) {
		if c.XMLName.Local == "text" {
			parts = append(parts, c.Text)
		}
	})
	return strings.Join(parts, " | ")
}

// hasClass reports whether n or an element under it carries class.
func (n xnode) hasClass(class string) bool {
	found := false
	n.walk(func(c xnode) {
		found = found || slices.Contains(strings.Fields(c.attr("class")), class)
	})
	return found
}

// cardWith is the one card of a drawn svg whose text holds s, and its id.
func cardWith(t *testing.T, svg, s string) (string, xnode) {
	t.Helper()
	var id string
	var card xnode
	n := 0
	for k, g := range cardGroups(decodeSVG(t, svg)) {
		if strings.Contains(g.text(), s) {
			id, card = k, g
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d cards hold %q, want 1:\n%s", n, s, svg)
	}
	return id, card
}

// pulses reports whether the card holding s pulses.
func pulses(t *testing.T, svg, s string) bool {
	t.Helper()
	_, card := cardWith(t, svg, s)
	return card.hasClass("aeye-pulse")
}
