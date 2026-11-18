package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func trueColor(t *testing.T) {
	t.Helper()
	t.Cleanup(applyPalette(themeDark))
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })
}

func TestResolveTheme(t *testing.T) {
	flavor := func(s string) func() string { return func() string { return s } }
	dark := func(b bool) func() bool { return func() bool { return b } }
	cases := []struct {
		name   string
		env    map[string]string
		flavor string
		dark   bool
		want   theme
	}{
		{"override light beats tmux dark", map[string]string{"CREW_DASH_THEME": "light"}, "mocha", true, themeLight},
		{"override dark beats tmux latte", map[string]string{"CREW_DASH_THEME": "dark"}, "latte", false, themeDark},
		{"override case-insensitive", map[string]string{"CREW_DASH_THEME": "LIGHT"}, "", true, themeLight},
		{"tmux latte beats dark terminal", nil, "latte", true, themeLight},
		{"tmux mocha beats light terminal", nil, "mocha", false, themeDark},
		{"tmux other value is dark", nil, "frappe", false, themeDark},
		{"terminal light", nil, "", false, themeLight},
		{"terminal dark", nil, "", true, themeDark},
		{"bogus override falls through", map[string]string{"CREW_DASH_THEME": "x"}, "latte", true, themeLight},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveTheme(env(c.env), flavor(c.flavor), dark(c.dark)); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

type flavorRunner struct {
	out  string
	err  error
	args []string
}

func (r *flavorRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	r.args = append([]string{name}, args...)
	return []byte(r.out), nil, r.err
}

func (r *flavorRunner) RunIn(ctx context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
	return r.Run(ctx, name, args...)
}

func TestTmuxFlavorFunc(t *testing.T) {
	r := &flavorRunner{out: "latte\n"}
	if got := tmuxFlavorFunc(context.Background(), r, true)(); got != "latte" {
		t.Fatalf("got %q", got)
	}
	if strings.Join(r.args, " ") != "tmux show-option -gqv @catppuccin_flavor" {
		t.Fatalf("args %v", r.args)
	}
	r2 := &flavorRunner{out: "latte"}
	if got := tmuxFlavorFunc(context.Background(), r2, false)(); got != "" || r2.args != nil {
		t.Fatalf("outside tmux must not run tmux: %q %v", got, r2.args)
	}
	r3 := &flavorRunner{out: "latte", err: errors.New("boom")}
	if got := tmuxFlavorFunc(context.Background(), r3, true)(); got != "" {
		t.Fatalf("error must fall through, got %q", got)
	}
}

func TestThemeFromCSI(t *testing.T) {
	for _, c := range []struct {
		in   string
		want theme
		ok   bool
	}{
		{"\x1b[?997;1n", themeDark, true},
		{"\x1b[?997;2n", themeLight, true},
		{"\x1b[?997;3n", themeDark, false},
		{"", themeDark, false},
	} {
		got, ok := themeFromCSI([]byte(c.in))
		if got != c.want || ok != c.ok {
			t.Errorf("%q = %v,%v", c.in, got, ok)
		}
	}
	if _, ok := themeReport("\x1b[?997;2n"); ok {
		t.Fatal("plain string must not match")
	}
	if _, ok := themeReport(nil); ok {
		t.Fatal("nil must not match")
	}
	if _, ok := themeReport([]byte("\x1b[?997;2n")); ok {
		t.Fatal("foreign []byte must not match")
	}
}

const (
	latteSGR = "38;2;108;111;133" // #6c6f85 dim
	mochaSGR = "38;2;127;131;156" // #7f849c dim
)

func TestThemeReportSwapsPalette(t *testing.T) {
	trueColor(t)
	m := sized(newTestModel(t), 120, 40)
	m.active = overviewTab
	if out := m.View(); !strings.Contains(out, mochaSGR) {
		t.Fatalf("default view lacks Mocha text colour")
	}
	// Feed the real bubbletea message type, produced by its own parser.
	nm, _ := m.Update(unknownCSIMsg(t, "\x1b[?997;2n"))
	out := nm.(Model).View()
	if !strings.Contains(out, latteSGR) || strings.Contains(out, mochaSGR) {
		t.Fatalf("light report did not switch the view to Latte")
	}
	nm, _ = nm.(Model).Update(unknownCSIMsg(t, "\x1b[?997;1n"))
	out = nm.(Model).View()
	if !strings.Contains(out, mochaSGR) || strings.Contains(out, latteSGR) {
		t.Fatalf("dark report did not switch the view back to Mocha")
	}
}

// unknownCSIMsg obtains bubbletea's unexported unknownCSISequenceMsg for seq
// by running a real program over it.
func unknownCSIMsg(t *testing.T, seq string) tea.Msg {
	t.Helper()
	cap := &captureModel{}
	p := tea.NewProgram(cap, tea.WithInput(&pipeOnce{data: seq}), tea.WithOutput(io.Discard))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if cap.got == nil {
		t.Fatal("no message captured")
	}
	return cap.got
}

// pipeOnce yields data once then blocks until closed, like a quiet terminal.
type pipeOnce struct {
	data string
	done bool
	ch   chan struct{}
}

func (p *pipeOnce) Read(b []byte) (int, error) {
	if !p.done {
		p.done = true
		return copy(b, p.data), nil
	}
	if p.ch == nil {
		p.ch = make(chan struct{})
	}
	<-p.ch
	return 0, io.EOF
}

type captureModel struct{ got tea.Msg }

func (c *captureModel) Init() tea.Cmd { return nil }
func (c *captureModel) View() string  { return "" }
func (c *captureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := themeReport(msg); ok {
		c.got = msg
		return c, tea.Quit
	}
	return c, nil
}

// quitOnReport wraps the real Model and quits once it handled a theme report.
type quitOnReport struct{ Model }

func (q quitOnReport) Init() tea.Cmd { return nil }
func (q quitOnReport) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, isReport := themeReport(msg)
	nm, cmd := q.Model.Update(msg)
	q.Model = nm.(Model)
	if isReport {
		return q, tea.Quit
	}
	return q, cmd
}

func TestThemeReportThroughRealProgram(t *testing.T) {
	trueColor(t)
	for _, c := range []struct {
		seq, sgr string
		light    bool
	}{
		{"\x1b[?997;2n", latteSGR, true},
		{"\x1b[?997;1n", mochaSGR, false},
	} {
		if c.light {
			applyPalette(themeDark)
		} else {
			applyPalette(themeLight)
		}
		m := sized(newTestModel(t), 120, 40)
		m.active = overviewTab
		p := tea.NewProgram(quitOnReport{m}, tea.WithInput(&pipeOnce{data: c.seq}), tea.WithOutput(io.Discard))
		final, err := p.Run()
		if err != nil {
			t.Fatal(err)
		}
		if lightActive != c.light {
			t.Fatalf("%q: lightActive = %v", c.seq, lightActive)
		}
		if out := final.(quitOnReport).View(); !strings.Contains(out, c.sgr) {
			t.Fatalf("%q: view lacks %s", c.seq, c.sgr)
		}
	}
}

func TestNoColorRendersNoSGR(t *testing.T) {
	t.Cleanup(applyPalette(themeLight))
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })
	m := sized(newTestModel(t), 120, 40)
	m.active = overviewTab
	nm, _ := m.Update(unknownCSIMsg(t, "\x1b[?997;1n"))
	if out := nm.(Model).View(); strings.Contains(out, "\x1b[") && strings.Contains(out, "38;") {
		t.Fatalf("ascii profile emitted colour SGR")
	}
}

func TestMochaHexMatchesOriginalValues(t *testing.T) {
	trueColor(t)
	if string(colText) != "#cdd6f4" || string(stateFailedColor) != "#d36a82" || gaugeNormalColor != "#2ecc71" {
		t.Fatal("dark theme must keep today's Mocha values")
	}
}

type pair struct {
	name   string
	fg, bg string
	floor  float64
}

func lightPairs() []pair {
	p := lattePalette
	const base = latteBase
	var ps []pair
	add := func(name, fg, bg string, floor float64) {
		ps = append(ps, pair{name, fg, bg, floor})
	}
	add("text/base", p.text, base, 4.5)
	add("text/selBg", p.text, p.selBg, 4.5)
	for _, c := range []struct{ n, fg string }{
		{"dim", p.dim}, {"borderOff", p.borderOff},
		{"teal", p.teal}, {"purple", p.purple}, {"amber", p.amber}, {"red", p.red}, {"green", p.green}, {"blue", p.blue},
		{"stWorking", p.stWorking}, {"stBlocked", p.stBlocked}, {"stPROpen", p.stPROpen}, {"stDone", p.stDone}, {"stFailed", p.stFailed},
		{"gaugeOK", p.gaugeOK}, {"gaugeWarn", p.gaugeWarn}, {"gaugeDanger", p.gaugeDanger},
	} {
		add(c.n+"/base", c.fg, base, 3)
		if c.n != "borderOff" { // borders never draw on the selected row's fill
			add(c.n+"/selBg", c.fg, p.selBg, 3)
		}
	}
	for _, c := range []struct{ n, bg string }{
		{"teal", p.teal}, {"purple", p.purple}, {"amber", p.amber}, {"red", p.red}, {"green", p.green}, {"blue", p.blue},
	} {
		add("pillFg/"+c.n, p.pillFg, c.bg, 4.5)
	}
	return ps
}

func ratio(fg, bg string) float64 {
	r1, g1, b1 := hexRGB(fg)
	r2, g2, b2 := hexRGB(bg)
	return contrastRGB(r1, g1, b1, r2, g2, b2)
}

func TestLatteContrastFloors(t *testing.T) {
	for _, c := range lightPairs() {
		got := ratio(c.fg, c.bg)
		if got < c.floor {
			t.Errorf("%s: %s on %s = %.2f, want >= %.1f", c.name, c.fg, c.bg, got, c.floor)
		}
		if os.Getenv("CREW_DASH_CONTRAST_TABLE") != "" {
			fmt.Printf("%s\t%.2f\n", c.name, got)
		}
	}
}

func TestDarkenForLight(t *testing.T) {
	for n := 0; n < 16; n++ {
		if got := darkenForLight(n); got != fmt.Sprint(n) {
			t.Errorf("index %d = %q, want unchanged", n, got)
		}
	}
	for n := 16; n <= 255; n++ {
		got := darkenForLight(n)
		if r := ratio(got, latteBase); r < 4.5 {
			t.Errorf("index %d -> %s contrast %.2f < 4.5", n, got, r)
		}
	}
	// A light pastel keeps its hue: 217 (#ffafaf-ish pink) stays red-dominant.
	r, g, b := hexRGB(darkenForLight(217))
	if !(r > g && r > b) {
		t.Errorf("217 lost its hue: %v %v %v", r, g, b)
	}
	// Already-dark indices are not changed.
	r0, g0, b0 := xtermRGB(52)
	if got := darkenForLight(52); got != fmt.Sprintf("#%02x%02x%02x", int(r0), int(g0), int(b0)) {
		t.Errorf("dark index 52 changed to %s", got)
	}
}

func TestTmuxColorStyleLight(t *testing.T) {
	trueColor(t)
	c := "colour217"
	w := vm.WorkerRow{Tmux: &c}
	dark := tmuxColorStyle(w).Render("x")
	applyPalette(themeLight)
	light := tmuxColorStyle(w).Render("x")
	if dark == light {
		t.Fatal("light mode must darken the codename colour")
	}
	if !strings.Contains(light, "38;2;") {
		t.Fatalf("light codename not truecolor: %q", light)
	}
}

func TestOnlyPaletteHoldsHexLiterals(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "theme.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), `Color("#`) {
			t.Errorf("%s has a hex colour literal outside theme.go", f)
		}
	}
}

// TestCaptureRenders writes TrueColor Overview and Board frames under both
// palettes to $CREW_DASH_CAPTURE_DIR; skipped when unset.
func TestCaptureRenders(t *testing.T) {
	dir := os.Getenv("CREW_DASH_CAPTURE_DIR")
	if dir == "" {
		t.Skip("CREW_DASH_CAPTURE_DIR not set")
	}
	trueColor(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, th := range []struct {
		name string
		t    theme
	}{{"dark", themeDark}, {"light", themeLight}} {
		applyPalette(th.t)
		for _, tab := range []struct {
			name string
			idx  int
		}{{"overview", overviewTab}, {"board", boardTab}} {
			m := sized(testModel(boardSnapshot()), 140, 45)
			m.active = tab.idx
			p := filepath.Join(dir, tab.name+"-"+th.name+".ansi")
			if err := os.WriteFile(p, []byte(m.View()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPinnedThemeIgnoresReports(t *testing.T) {
	trueColor(t)
	applyPalette(themeLight)
	m := sized(newTestModel(t), 120, 40)
	m.active = overviewTab
	m.themePinned = true
	nm, _ := m.Update(unknownCSIMsg(t, "\x1b[?997;1n"))
	if out := nm.(Model).View(); !strings.Contains(out, latteSGR) {
		t.Fatalf("pinned view lacks %s", latteSGR)
	}
}

func TestThemePinned(t *testing.T) {
	for v, want := range map[string]bool{"light": true, "DARK": true, "": false, "auto": false} {
		if got := themePinned(env(map[string]string{"CREW_DASH_THEME": v})); got != want {
			t.Errorf("%q: got %v", v, got)
		}
	}
}
