package once

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// reltime ports the shared jq `reltime`: "<d>d <h>h" / "<h>h <m>m" / "<m>m".
func reltime(s int64) string {
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

// usd ports the shared jq `usd`: cents-rounded, always two decimals.
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

// fmt1 ports the shared jq `fmt1`: one decimal place, nil in nil out.
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

// formatNumber prints a JSON number the way jq's string interpolation does:
// minimal, no trailing ".0" for a whole value.
func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// renderAgg ports the shared jq `render_agg`: "—" when unmeasured, "(k)"
// when the aggregate's own denominator differs from the row total, "!" for
// small samples.
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

// padRow ports the shared jq `pad_row`: right column never trails with
// spaces, widths are rune counts (as jq's `length` counts codepoints, not
// display width).
func padRow(cells []string, widths []int, left []bool) string {
	end := len(cells) - 1
	parts := make([]string, len(cells))
	for c, cell := range cells {
		p := widths[c] - utf8.RuneCountInString(cell)
		if p < 0 {
			p = 0
		}
		switch {
		case !left[c]:
			parts[c] = strings.Repeat(" ", p) + cell
		case c == end:
			parts[c] = cell
		default:
			parts[c] = cell + strings.Repeat(" ", p)
		}
	}
	return strings.Join(parts, "  ")
}

func tableWidths(rows [][]string) []int {
	ncols := len(rows[0])
	widths := make([]int, ncols)
	for c := 0; c < ncols; c++ {
		max := 0
		for _, row := range rows {
			if n := utf8.RuneCountInString(row[c]); n > max {
				max = n
			}
		}
		widths[c] = max
	}
	return widths
}

// cleanText ports the shared jq `clean`: tab/newline/CR become a space,
// other C0/C1 controls and bidi overrides are dropped.
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

func isoUTC(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}
