package ui

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

// TestFleetInvariantWalk drives the root Model on the Fleet tab through random
// keys, typed text and resizes, and after every step checks the frame's
// geometry and that the chosen crew is one the filtered view still lists.
func TestFleetInvariantWalk(t *testing.T) {
	asciiProfile(t)
	const seqs, steps, groups = 300, 80, 6
	for g := range uint64(groups) {
		t.Run(fmt.Sprintf("group%d", g), func(t *testing.T) {
			t.Parallel()
			for seed := g + 1; seed <= seqs; seed += groups {
				fleetWalkSeed(t, seed, steps)
			}
		})
	}
}

func fleetWalkSeed(t *testing.T, seed uint64, steps int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 0))
	m := sized(fleetRootModel(t, viewtest.FleetInputs()), 120, 40)
	var hist []string
	for range steps {
		var name string
		m, name = fleetWalkStep(rng, m)
		hist = append(hist, name)
		if err := fleetFrameInvariant(m); err != "" {
			t.Fatalf("seed %d after %d steps: %s\nsteps: %s", seed, len(hist), err, strings.Join(hist, " "))
		}
	}
}

var fleetWalkKeys = []string{"tab", "shift+tab", "j", "k", "enter", "esc", "/", "?", "1", "2", "3", "4", "5", "6", "7", "8", "9", "L", "R", "[", "]", "y", "n"}

func fleetWalkStep(rng *rand.Rand, m Model) (Model, string) {
	if rng.IntN(12) == 0 {
		w, h := 60+rng.IntN(161), 20+rng.IntN(51)
		return sized(m, w, h), fmt.Sprintf("resize(%dx%d)", w, h)
	}
	if m.views[m.active].Capturing() && rng.IntN(2) == 0 {
		s := string("abcdegopxz0 "[rng.IntN(12)])
		nm, _ := m.Update(keyOf(s))
		return nm.(Model), "type(" + s + ")"
	}
	k := fleetWalkKeys[rng.IntN(len(fleetWalkKeys))]
	nm, _ := m.Update(keyOf(k))
	return nm.(Model), k
}

// fleetFrameInvariant returns "" when the frame has exactly m.height lines,
// none wider than m.width, and, on the undrilled Fleet tab, the selected crew
// is empty or listed by the filtered view.
func fleetFrameInvariant(m Model) string {
	frame := m.View()
	lines := strings.Split(frame, "\n")
	if len(lines) != m.height {
		return fmt.Sprintf("frame has %d lines, want %d", len(lines), m.height)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > m.width {
			return fmt.Sprintf("line %d is %d wide, want <= %d: %q", i, w, m.width, stripped(l))
		}
	}
	if m.active != fleetTab {
		return ""
	}
	fl := m.views[fleetTab].(fleetView)
	if fl.drilled {
		return ""
	}
	s := fl.sel()
	if s.crew < 0 {
		return ""
	}
	if !slices.Contains(newFleetIndex(fl.fv, fl.st.filter).visible(), s.crew) {
		return fmt.Sprintf("selected crew %d is outside the filter %q", s.crew, fl.st.filter)
	}
	return ""
}
