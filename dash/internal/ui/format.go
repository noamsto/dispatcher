package ui

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/invisible"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
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

func strOf(p *string) string {
	if p == nil {
		return ""
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

// cleanText ports once/format.go's jq `clean`: tab/newline/CR become a space,
// other C0/C1 controls are dropped, and invisible format characters go through
// internal/invisible (emoji-safe; models read what terminals hide).
func cleanText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return invisible.Drop(b.String())
}

const controlNotice = "(command contains control bytes; see crew dash --json --view overview)"

// hasControl reports whether a's command or any argv word holds a byte that
// cleanText would drop or rewrite, so showing it could differ from what runs.
func hasControl(a vm.Action) bool {
	return slices.ContainsFunc(append([]string{a.Command}, a.Argv...), func(s string) bool {
		return cleanText(s) != s
	})
}

// shownCommand is a's command verbatim, or a fixed notice when it holds
// control bytes: a cleaned copy could be pasted and run as something else.
func shownCommand(a vm.Action) string {
	if hasControl(a) {
		return controlNotice
	}
	return a.Command
}

// cleanView returns a deep copy of v with every string, string pointer and
// string map key or value passed through cleanText(ansi.Strip(s)), so text is
// clean before anything measures or truncates it. Fields tagged dash:"raw"
// keep their bytes. Pointers, slices and maps are copied, never written
// through, since the view model shares them with its inputs.
func cleanView[T any](v T) T {
	cleanValue(reflect.ValueOf(&v).Elem())
	return v
}

func cleanValue(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(cleanText(ansi.Strip(v.String())))
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(v.Elem())
		cleanValue(c.Elem())
		v.Set(c)
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() && f.Tag.Get("dash") != "raw" {
				cleanValue(v.Field(i))
			}
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(c, v)
		for i := range c.Len() {
			cleanValue(c.Index(i))
		}
		v.Set(c)
	case reflect.Map:
		if v.IsNil() {
			return
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			k.Set(it.Key())
			e.Set(it.Value())
			cleanValue(k)
			cleanValue(e)
			c.SetMapIndex(k, e)
		}
		v.Set(c)
	}
}
