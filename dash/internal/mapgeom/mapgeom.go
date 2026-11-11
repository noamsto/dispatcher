// Package mapgeom decodes the planet geometry that `crew roster-render --map`
// prints and maps terminal cells onto it, so a click on the kitty-placed
// roster image selects a worker. It is pure: no terminal, no bubbletea.
package mapgeom

import (
	"encoding/json"
	"fmt"
	"math"
)

const (
	// Version is the only geometry schema this package decodes.
	Version = 1

	// MaxPlanets matches the crew renderer's own cap.
	MaxPlanets = 64

	// MaxCells is the most cells per side a kitty placeholder block can
	// address (its row/column diacritics table); kitty.MaxCells is the same
	// number.
	MaxCells = 297

	// minPx and maxPx bound each side of the image the renderer accepts via
	// --px.
	minPx = 64
	maxPx = 4096
)

// Planet is one drawn worker. CX, CY and R are in PNG pixels with the origin
// at the image's top-left.
type Planet struct {
	Branch string `json:"branch"`
	Name   string `json:"name"`
	// Repo is the repo's label, empty when the crew spans one repo.
	Repo string `json:"repo,omitempty"`
	// Home marks a planet from the bus the renderer ran in, the only one whose
	// worker the roster lists. Set only when the crew spans several repos.
	Home bool    `json:"home,omitempty"`
	CX   float64 `json:"cx"`
	CY   float64 `json:"cy"`
	R    float64 `json:"r"`
	// HW and HH are the half width and half height of a card-shaped worker.
	// Both are set or both are zero; older renderers send neither, and R still
	// bounds the card so it stays a valid circle for them.
	HW float64 `json:"hw,omitempty"`
	HH float64 `json:"hh,omitempty"`
}

// PlanetKey identifies a planet: a branch alone is ambiguous across repos.
type PlanetKey struct{ Repo, Branch string }

func (p Planet) Key() PlanetKey { return PlanetKey{Repo: p.Repo, Branch: p.Branch} }

// Geometry is the decoded `--map` output. Planets are in ring/slot order,
// which is the keyboard order.
type Geometry struct {
	V       int      `json:"v"`
	W       int      `json:"w"`
	H       int      `json:"h"`
	PNG     string   `json:"png"`
	Planets []Planet `json:"planets"`
}

// Parse decodes and validates the JSON `crew roster-render --map` prints.
func Parse(data []byte) (Geometry, error) {
	var g Geometry
	if err := json.Unmarshal(data, &g); err != nil {
		return Geometry{}, fmt.Errorf("map geometry: %w", err)
	}
	if g.V != Version {
		return Geometry{}, fmt.Errorf("map geometry: unsupported version %d", g.V)
	}
	if g.W <= 0 || g.H <= 0 {
		return Geometry{}, fmt.Errorf("map geometry: bad image size %dx%d", g.W, g.H)
	}
	if len(g.Planets) > MaxPlanets {
		return Geometry{}, fmt.Errorf("map geometry: %d planets, max %d", len(g.Planets), MaxPlanets)
	}
	// Two repos of one crew may each have a worker on the same branch name.
	seen := make(map[[2]string]bool, len(g.Planets))
	for i, p := range g.Planets {
		if p.Branch == "" {
			return Geometry{}, fmt.Errorf("map geometry: planet %d has no branch", i)
		}
		if seen[[2]string{p.Repo, p.Branch}] {
			return Geometry{}, fmt.Errorf("map geometry: duplicate branch %q", p.Branch)
		}
		seen[[2]string{p.Repo, p.Branch}] = true
		if !finite(p.CX, p.CY, p.R) || p.R <= 0 {
			return Geometry{}, fmt.Errorf("map geometry: planet %q has a bad circle", p.Branch)
		}
		if !finite(p.HW, p.HH) || p.HW < 0 || p.HH < 0 || (p.HW > 0) != (p.HH > 0) {
			return Geometry{}, fmt.Errorf("map geometry: planet %q has a bad card", p.Branch)
		}
		if p.CX < 0 || p.CX > float64(g.W) || p.CY < 0 || p.CY > float64(g.H) {
			return Geometry{}, fmt.Errorf("map geometry: planet %q is outside the image", p.Branch)
		}
	}
	return g, nil
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// Placement is the cols x rows block of cells the image occupies, built by
// Fit. The renderer is asked for PxW x PxH pixels, so one cell is CellW x
// CellH image pixels.
type Placement struct {
	Cols, Rows   int
	CellW, CellH int
}

// Fit sizes the image block to a box of cells: each side is capped at
// MaxCells (and at what keeps the pixel box within the renderer's 4096-pixel
// limit, so cells are dropped rather than the image rescaled) and is at least
// one cell. A box smaller than 64 pixels on a side is below the renderer's
// minimum, which then returns a larger image; Hit rescales for that.
func Fit(boxCols, boxRows, cellW, cellH int) Placement {
	return Placement{
		Cols:  max(1, min(boxCols, MaxCells, maxPx/cellW)),
		Rows:  max(1, min(boxRows, MaxCells, maxPx/cellH)),
		CellW: cellW,
		CellH: cellH,
	}
}

// PxW is the image width to request from the renderer (--px).
func (p Placement) PxW() int { return max(minPx, p.Cols*p.CellW) }

// PxH is the image height to request from the renderer (--px).
func (p Placement) PxH() int { return max(minPx, p.Rows*p.CellH) }

// Hit returns the index of the planet whose shape the cell at (cellX, cellY)
// touches, counted from the image block's top-left, or -1 for gaps, margins
// and cells outside the block. A card is tested by its rectangle and any other
// planet by its circle. When shapes overlap in the cell, the planet whose
// centre is nearest the cell rect wins; a cell that contains a centre always
// picks that planet.
func Hit(g Geometry, p Placement, cellX, cellY int) int {
	if cellX < 0 || cellY < 0 || cellX >= p.Cols || cellY >= p.Rows {
		return -1
	}
	// The renderer may return an image other than Cols*CellW x Rows*CellH
	// (see Fit); kitty stretches it over the block, so map cells to image
	// pixels by fraction.
	sx := float64(g.W) / float64(p.Cols*p.CellW)
	sy := float64(g.H) / float64(p.Rows*p.CellH)
	x0 := float64(cellX*p.CellW) * sx
	x1 := float64((cellX+1)*p.CellW) * sx
	y0 := float64(cellY*p.CellH) * sy
	y1 := float64((cellY+1)*p.CellH) * sy

	best, bestD := -1, math.Inf(1)
	for i, pl := range g.Planets {
		dx := pl.CX - clamp(pl.CX, x0, x1)
		dy := pl.CY - clamp(pl.CY, y0, y1)
		d := math.Hypot(dx, dy)
		if d >= bestD || !pl.touches(x0, x1, y0, y1, d) {
			continue
		}
		best, bestD = i, d
	}
	return best
}

// touches reports whether the cell rect [x0,x1]x[y0,y1] reaches the planet's
// shape; d is the distance from the planet's centre to that rect.
func (p Planet) touches(x0, x1, y0, y1, d float64) bool {
	if p.HW > 0 && p.HH > 0 {
		return x1 >= p.CX-p.HW && x0 <= p.CX+p.HW && y1 >= p.CY-p.HH && y0 <= p.CY+p.HH
	}
	return d <= p.R
}

func clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }
