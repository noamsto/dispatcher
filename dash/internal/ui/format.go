package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
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

// fmt1 ports once/format.go's jq `fmt1`: one decimal place, nil in nil out.
func fmt1(f *float64) *string {
	if f == nil {
		return nil
	}
	t := int64(math.Round(*f * 10))
	i := t / 10
	frac := t - i*10
	if frac < 0 {
		frac = -frac
	}
	s := strconv.FormatInt(i, 10) + "." + strconv.FormatInt(frac, 10)
	return &s
}

func optNumber(f *float64) string {
	if f == nil {
		return ""
	}
	return formatNumber(*f)
}

func optFmt1(f *float64) string {
	s := fmt1(f)
	if s == nil {
		return ""
	}
	return *s
}

// renderAgg ports once/format.go's jq `render_agg`: "—" when unmeasured,
// "(k)" when the aggregate's own denominator differs from the row total,
// "!" for small samples (k<5).
func renderAgg(k, n int, text string) string {
	if k == 0 {
		return "—"
	}
	out := text
	if k != n {
		out += fmt.Sprintf("(%d)", k)
	}
	if k < 5 {
		out += "!"
	}
	return out
}

// isoUTC ports once/format.go's jq `isoUTC`.
func isoUTC(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// cleanText ports once/format.go's jq `clean`: tab/newline/CR become a
// space, other C0/C1 controls and bidi overrides are dropped.
func cleanText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
