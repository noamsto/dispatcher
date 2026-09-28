package ui

import (
	"fmt"
	"math"
	"strconv"
)

// format.go ports the small set of the once renderer's shared jq-derived
// formatters (dash/internal/once/format.go) that the interactive views also
// need, so "3d 12h" / "+40" / "$12.34" read identically in both renderers.

// reltime renders a duration in seconds as "<d>d <h>h" / "<h>h <m>m" / "<m>m".
func reltime(s int64) string {
	if s < 0 {
		s = 0
	}
	d := s / 86400
	h := (s % 86400) / 3600
	m := (s % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// formatNumber prints a float the way jq's string interpolation does:
// minimal, no trailing ".0" for a whole value.
func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// usd renders a dollar amount cents-rounded, always two decimals.
func usd(f float64) string {
	c := int64(math.Round(f * 100))
	whole := c / 100
	frac := c % 100
	if frac < 0 {
		frac = -frac
	}
	fracStr := strconv.FormatInt(frac, 10)
	if len(fracStr) == 1 {
		fracStr = "0" + fracStr
	}
	return strconv.FormatInt(whole, 10) + "." + fracStr
}

func pace(aheadPts *int64) string {
	if aheadPts == nil {
		return "—"
	}
	if *aheadPts > 0 {
		return fmt.Sprintf("+%d", *aheadPts)
	}
	return strconv.FormatInt(*aheadPts, 10)
}

func f64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
