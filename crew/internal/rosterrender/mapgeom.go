package rosterrender

import "math"

// mapPlanet is one worker card of the map in pixels of the rendered PNG, y
// down. hw and hh are the half extents of its card; r is the circle around it,
// for readers that predate the rectangle.
type mapPlanet struct {
	Branch string `json:"branch"`
	Name   string `json:"name"`
	Repo   string `json:"repo,omitempty"`
	// Home marks a worker from the bus the renderer was run in; set only when
	// the board has more than one lane, as Repo is.
	Home bool    `json:"home,omitempty"`
	Cx   float64 `json:"cx"`
	Cy   float64 `json:"cy"`
	HW   float64 `json:"hw"`
	HH   float64 `json:"hh"`
	R    float64 `json:"r"`
}

// mapGeometry places every worker card of b, laid out by g, in a pngW x pngH
// image of its svg, in board reading order. The viewBox is exactly the canvas,
// which resvg scales to fit the image and centres. Holds and queued tasks have
// no worker to select, so they are left out.
func mapGeometry(b board, g boardGeom, pngW, pngH int) []mapPlanet {
	s := min(float64(pngW)/g.W, float64(pngH)/g.H)
	ox, oy := (float64(pngW)-g.W*s)/2, (float64(pngH)-g.H*s)/2
	out := []mapPlanet{}
	for _, cg := range g.cards() {
		c := b.lanes[cg.lane].cols[cg.col].cards[cg.card]
		if c.kind != kindPlanet {
			continue
		}
		mp := mapPlanet{Branch: c.branch, Name: c.name}
		if len(b.lanes) > 1 {
			mp.Repo, mp.Home = c.repoLabel, c.home
		}
		hw, hh := cg.w/2, cg.h/2
		mp.Cx, mp.Cy = round2(ox+(cg.x+hw)*s), round2(oy+(cg.y+hh)*s)
		mp.HW, mp.HH, mp.R = round2(hw*s), round2(hh*s), round2(math.Hypot(hw, hh)*s)
		out = append(out, mp)
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
