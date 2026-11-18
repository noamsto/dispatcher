package ui

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

type theme int

const (
	themeDark theme = iota
	themeLight
)

// palette is every themed colour keyed by role. mochaPalette and lattePalette
// below are the only hex literals in the dash.
type palette struct {
	text, dim, borderOff, selBg, pillFg              string
	teal, purple, amber, red, green, blue            string
	stWorking, stBlocked, stPROpen, stDone, stFailed string
	gaugeOK, gaugeWarn, gaugeDanger                  string
}

var mochaPalette = palette{
	text: "#cdd6f4", dim: "#7f849c", borderOff: "#585b70", selBg: "#2f3350", pillFg: "#11111b",
	teal: "#94e2d5", purple: "#b4befe", amber: "#f9e2af", red: "#f38ba8", green: "#a6e3a1", blue: "#89b4fa",
	stWorking: "#a6e3a1", stBlocked: "#f38ba8", stPROpen: "#89b4fa", stDone: "#6c7086", stFailed: "#d36a82",
	gaugeOK: "#2ecc71", gaugeWarn: "#e6b800", gaugeDanger: "#e63939",
}

// lattePalette uses the official Latte hue per role, darkened (same hue,
// lower lightness) where the official tone misses its contrast floor against
// the Latte base #eff1f5.
var lattePalette = palette{
	text: "#4c4f69", dim: "#6c6f85", borderOff: "#7c7f93", selBg: "#ccd0da", pillFg: "#eff1f5",
	teal: "#0f7a6e", purple: "#5b5bd6", amber: "#9a5a00", red: "#c0203f", green: "#2a7d1c", blue: "#1250c8",
	stWorking: "#2a7d1c", stBlocked: "#c0203f", stPROpen: "#1250c8", stDone: "#6c6f85", stFailed: "#a8324e",
	gaugeOK: "#2a7d1c", gaugeWarn: "#9a6a00", gaugeDanger: "#c01c28",
}

// latteBase is the light terminal background codenames are darkened against.
const latteBase = "#eff1f5"

// lightActive tells tmuxColorStyle which shades to use; applyPalette sets it.
var lightActive bool

func paletteOf(t theme) palette {
	if t == themeLight {
		return lattePalette
	}
	return mochaPalette
}

// applyPalette points the package colour vars at t's palette and returns a
// func restoring the previous theme.
func applyPalette(t theme) (restore func()) {
	prev := themeDark
	if lightActive {
		prev = themeLight
	}
	p := paletteOf(t)
	colText, colDim, colBorderOff = lipgloss.Color(p.text), lipgloss.Color(p.dim), lipgloss.Color(p.borderOff)
	colSelBg, colPillFg = lipgloss.Color(p.selBg), lipgloss.Color(p.pillFg)
	colTeal, colPurple, colAmber = lipgloss.Color(p.teal), lipgloss.Color(p.purple), lipgloss.Color(p.amber)
	colRed, colGreen, colBlue = lipgloss.Color(p.red), lipgloss.Color(p.green), lipgloss.Color(p.blue)
	stateWorkingColor, stateBlockedColor = lipgloss.Color(p.stWorking), lipgloss.Color(p.stBlocked)
	statePROpenColor, stateDoneColor = lipgloss.Color(p.stPROpen), lipgloss.Color(p.stDone)
	stateFailedColor = lipgloss.Color(p.stFailed)
	gaugeNormalColor, gaugeWarningColor, gaugeDangerColor = p.gaugeOK, p.gaugeWarn, p.gaugeDanger
	lightActive = t == themeLight
	return func() {
		if prev != t {
			applyPalette(prev)
		}
	}
}

// resolveTheme picks the startup theme: CREW_DASH_THEME, then tmux's
// @catppuccin_flavor (latte = light, any other value = dark), then the
// terminal background, then dark. tmuxFlavor returns "" when unset.
func resolveTheme(getenv func(string) string, tmuxFlavor func() string, hasDark func() bool) theme {
	switch strings.ToLower(getenv("CREW_DASH_THEME")) {
	case "light":
		return themeLight
	case "dark":
		return themeDark
	}
	if f := strings.TrimSpace(tmuxFlavor()); f != "" {
		if f == "latte" {
			return themeLight
		}
		return themeDark
	}
	if !hasDark() {
		return themeLight
	}
	return themeDark
}

// tmuxFlavorFunc reads @catppuccin_flavor through the runner; "" when not in
// tmux, unset or failing.
func tmuxFlavorFunc(ctx context.Context, r data.Runner, inTmux bool) func() string {
	return func() string {
		if !inTmux || r == nil {
			return ""
		}
		out, _, err := r.Run(ctx, "tmux", "show-option", "-gqv", "@catppuccin_flavor")
		if err != nil {
			return ""
		}
		return string(bytes.TrimSpace(out))
	}
}

// themeFromCSI parses a DEC 2031 report: CSI ? 997 ; 1 n is dark, 2 is light.
func themeFromCSI(b []byte) (theme, bool) {
	switch string(b) {
	case "\x1b[?997;1n":
		return themeDark, true
	case "\x1b[?997;2n":
		return themeLight, true
	}
	return themeDark, false
}

// themeReport extracts a theme report from msg. bubbletea v1 surfaces an
// unrecognised CSI as the unexported unknownCSISequenceMsg ([]byte), so it is
// matched by type name through reflection.
func themeReport(msg any) (theme, bool) {
	t := reflect.TypeOf(msg)
	if t == nil || t.PkgPath() != "github.com/charmbracelet/bubbletea" ||
		t.Name() != "unknownCSISequenceMsg" || t.Kind() != reflect.Slice || t.Elem().Kind() != reflect.Uint8 {
		return themeDark, false
	}
	return themeFromCSI(reflect.ValueOf(msg).Bytes())
}

func hexRGB(h string) (r, g, b float64) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(h, "#"), 16, 32)
	return float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)
}

func relLuminance(r, g, b float64) float64 {
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrastRGB(r1, g1, b1, r2, g2, b2 float64) float64 {
	a, b := relLuminance(r1, g1, b1), relLuminance(r2, g2, b2)
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

// xtermRGB is the RGB of a 256-colour index in the 16-255 range.
func xtermRGB(n int) (r, g, b float64) {
	if n >= 232 {
		v := float64(8 + 10*(n-232))
		return v, v, v
	}
	n -= 16
	lv := func(i int) float64 {
		if i == 0 {
			return 0
		}
		return float64(55 + 40*i)
	}
	return lv(n / 36), lv(n / 6 % 6), lv(n % 6)
}

func rgbToHSL(r, g, b float64) (h, s, l float64) {
	r, g, b = r/255, g/255, b/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (mx + mn) / 2
	if mx == mn {
		return 0, 0, l
	}
	d := mx - mn
	if l > 0.5 {
		s = d / (2 - mx - mn)
	} else {
		s = d / (mx + mn)
	}
	switch mx {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h / 6, s, l
}

func hslToRGB(h, s, l float64) (r, g, b float64) {
	if s == 0 {
		return l * 255, l * 255, l * 255
	}
	q := l + s - l*s
	if l < 0.5 {
		q = l * (1 + s)
	}
	p := 2*l - q
	hue := func(t float64) float64 {
		if t < 0 {
			t++
		}
		if t > 1 {
			t--
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 0.5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return hue(h+1.0/3) * 255, hue(h) * 255, hue(h-1.0/3) * 255
}

// darkenForLight maps a tmux colour index to a hex colour readable on the
// Latte base: same hue and saturation, lightness lowered until the contrast
// is at least 4.5:1. Indices 0-15 follow the terminal palette and pass
// through unchanged.
func darkenForLight(n int) string {
	if n < 16 || n > 255 {
		return strconv.Itoa(n)
	}
	br, bg, bb := hexRGB(latteBase)
	r, g, b := xtermRGB(n)
	h, s, l := rgbToHSL(r, g, b)
	for contrastRGB(r, g, b, br, bg, bb) < 4.5 && l > 0 {
		l -= 0.005
		r, g, b = hslToRGB(h, s, math.Max(l, 0))
		r, g, b = math.Round(r), math.Round(g), math.Round(b)
	}
	return fmt.Sprintf("#%02x%02x%02x", int(r), int(g), int(b))
}

func init() { applyPalette(themeDark) }
