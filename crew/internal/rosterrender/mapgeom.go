package rosterrender

import (
	"math"
	"strconv"
)

// mapPlanet is one planet of the map in pixels of the rendered PNG, y down. r is
// the outer edge of its ring, so a remote host's hexagon is covered by its
// circumscribed circle, a few pixels more than its drawn area.
type mapPlanet struct {
	Branch string `json:"branch"`
	Name   string `json:"name"`
	Repo   string `json:"repo,omitempty"`
	// Home marks a planet from the bus the renderer was run in; set only when
	// wedges exist, as Repo is.
	Home bool    `json:"home,omitempty"`
	Cx   float64 `json:"cx"`
	Cy   float64 `json:"cy"`
	R    float64 `json:"r"`
}

func viewBoxSize(svg []byte) (w, h float64, ok bool) {
	m := svgRootRe.FindSubmatch(svg)
	if m == nil {
		return 0, 0, false
	}
	w, errW := strconv.ParseFloat(string(m[3]), 64)
	h, errH := strconv.ParseFloat(string(m[4]), 64)
	return w, h, errW == nil && errH == nil && w > 0 && h > 0
}

// mapGeometry places every planet of sc in a pngW x pngH image of the svg. The
// svg's bounding box is the four corners, so a layout point lands at a constant
// offset in the viewBox, which resvg scales to fit the image and centres.
func mapGeometry(sc scene, halfX, halfY, vbW, vbH float64, pngW, pngH int) []mapPlanet {
	s := min(float64(pngW)/vbW, float64(pngH)/vbH)
	ox, oy := (float64(pngW)-vbW*s)/2, (float64(pngH)-vbH*s)/2
	padX, padY := (vbW-2*halfX)/2, (vbH-2*halfY)/2
	out := make([]mapPlanet, 0, len(sc.planets))
	for _, p := range sc.planets {
		repo, home := p.repoLabel, p.home
		if len(sc.wedges) == 0 {
			repo, home = "", false
		}
		out = append(out, mapPlanet{
			Branch: p.branch,
			Name:   p.name,
			Repo:   repo,
			Home:   home,
			Cx:     round2(ox + (padX+halfX+p.x)*s),
			Cy:     round2(oy + (padY+halfY-p.y)*s),
			R:      round2(hitRadius(p) * s),
		})
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
