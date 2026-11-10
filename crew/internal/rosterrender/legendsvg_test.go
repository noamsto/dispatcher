package rosterrender

import (
	"bytes"
	"image/png"
	"os/exec"
	"strings"
	"testing"
)

func fullKeyLegend() legend {
	budget, pct := 6, 23.4
	return legend{
		holds: 1, repos: 2, waiting: 1, ci: 1, stale: 1, weight: 4, budget: &budget,
		engines: []engineReading{{name: "claude", window: "7d", pct: &pct}, {name: "codex"}},
	}
}

func TestLegendTargetIsTheDiagramStemWithLegendSVG(t *testing.T) {
	if got, want := legendTarget("/d/roster-repo-11-10-1234-abcd.svg"), "/d/roster-repo-11-10-1234-abcd.legend.svg"; got != want {
		t.Errorf("legendTarget = %q, want %q", got, want)
	}
}

func TestLegendSVGHoldsTheFullKey(t *testing.T) {
	lg := fullKeyLegend()
	svg := legendSVG(lg)
	for _, want := range []string{
		"working", "blocked", "pr open", "done", "failed",
		"size = tier", "small dot = role", "hexagon = remote host", "dashed arrow = stacked on",
		"dashed outer dot = queued hold", "wedge = repo", "waiting on you",
		"ci pass", "ci running", "ci fail", "no runner", "dim = no status 45m+",
		stripText(lg),
		"<polygon", `fill="` + colBG + `"`, `font-family="'Source Sans 3'"`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("the key lacks %q:\n%s", want, svg)
		}
	}
	root := parseSVG(t, []byte(svg))
	if root.XMLName.Local != "svg" || root.attr("viewBox") == "" {
		t.Errorf("not an svg with a viewBox: %s %q", root.XMLName.Local, root.attr("viewBox"))
	}
}

// TestLegendSVGKeysFollowTheDiagram: the marker keys appear only for what the
// diagram can draw, as in the full graph legend, and the strip follows the bus.
func TestLegendSVGKeysFollowTheDiagram(t *testing.T) {
	plain := legendSVG(legend{})
	for _, absent := range []string{"queued hold", "wedge = repo", "waiting on you", "ci pass", "no runner", "dim = no status"} {
		if strings.Contains(plain, absent) {
			t.Errorf("the key carries %q for a diagram that cannot draw it", absent)
		}
	}
	for _, want := range []string{"hexagon = remote host", "dashed arrow = stacked on", "size = tier", "small dot = role"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the key lacks %q", want)
		}
	}
	if a, b := legendSVG(fullKeyLegend()), legendSVG(legend{}); a == b {
		t.Error("the key does not change with the legend")
	}
	moved := fullKeyLegend()
	moved.weight = 5
	if legendSVG(moved) == legendSVG(fullKeyLegend()) || !strings.Contains(legendSVG(moved), "crew weight 5") {
		t.Error("the strip is not drawn from the legend")
	}
}

func TestLegendSVGEscapesItsText(t *testing.T) {
	pct := 1.0
	svg := legendSVG(legend{engines: []engineReading{{name: `<b>&"x'`, window: "7d", pct: &pct}}})
	if strings.Contains(svg, "<b>") {
		t.Errorf("engine text reached the svg as markup:\n%s", svg)
	}
	parseSVG(t, []byte(svg))
}

func TestLegendSVGRenders(t *testing.T) {
	resvg, err := exec.LookPath("resvg")
	if err != nil {
		t.Skip("resvg not on PATH")
	}
	cmd := exec.Command(resvg, "-", "-c")
	cmd.Stdin = strings.NewReader(legendSVG(fullKeyLegend()))
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("resvg: %v\n%s", err, stderr.String())
	}
	img, err := png.Decode(&out)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() < 300 || b.Dy() < 300 {
		t.Errorf("rendered %v, too small for the full key", b)
	}
}
