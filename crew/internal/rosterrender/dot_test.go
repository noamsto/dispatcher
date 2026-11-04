package rosterrender

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the testdata goldens")

const dotNow = 1_800_000_000.0

// dotWorker is one hand-built roster row; age is seconds since its last event.
type dotWorker struct {
	branch, name, tier, state, base, host string
	age                                   float64
	roles                                 [][2]string // role, state
}

func dotModel(ws []dotWorker, holds int) map[string]any {
	m := emptyModel()
	for _, w := range ws {
		row := map[string]any{
			"branch": w.branch, "name": w.name, "tier": w.tier, "state": w.state,
			"ts": (dotNow - w.age) * 1000,
		}
		if w.base != "" {
			row["base"] = w.base
		}
		if w.host != "" {
			row["host"] = w.host
		}
		m["rows"] = append(m["rows"].([]any), row)
		for _, r := range w.roles {
			m["roles"] = append(m["roles"].([]any), map[string]any{"branch": w.branch, "role": r[0], "state": r[1]})
		}
	}
	for range holds {
		m["holds"] = append(m["holds"].([]any), map[string]any{"window": "five_hour"})
	}
	return m
}

var (
	roleBoth = [][2]string{{"critic", "working"}, {"reviewer", "idle"}}
	roleOne  = [][2]string{{"critic", "working"}}
)

func eightWorkers() []dotWorker {
	return []dotWorker{
		{branch: "feat/101-alpha", name: "lime", tier: "deep", state: "working", age: 30, roles: roleBoth},
		{branch: "feat/102-beta", name: "sky", tier: "standard", state: "blocked", age: 60, roles: roleOne},
		{branch: "feat/103-gamma", name: "rose", tier: "trivial", state: "pr_open", age: 90},
		{branch: "feat/104-delta", name: "teal", tier: "standard", state: "working", age: 20, base: "feat/101-alpha"},
		{branch: "feat/105-eps", name: "olive", tier: "standard", state: "dispatched", age: 10, host: "mbp"},
		{branch: "feat/106-zeta", name: "slate", tier: "trivial", state: "done", age: 120},
		{branch: "feat/107-eta", name: "amber", tier: "standard", state: "done", age: 900},
		{branch: "feat/108-theta", name: "plum", tier: "deep", state: "working", age: 5},
	}
}

func sixteenWorkers() []dotWorker {
	return append(eightWorkers(), []dotWorker{
		{branch: "feat/109-iota", name: "coral", tier: "standard", state: "failed", age: 100},
		{branch: "fix/110-kappa", name: "mint", tier: "deep", state: "blocked", age: 40, roles: roleOne},
		{branch: "feat/111-lambda", name: "fern", tier: "standard", state: "working", age: 15, roles: roleBoth},
		{branch: "feat/112-mu", name: "dune", tier: "trivial", state: "working", age: 25},
		{branch: "feat/113-nu", name: "opal", tier: "standard", state: "pr_open", age: 70},
		{branch: "feat/114-xi", name: "iris", tier: "standard", state: "working", age: 12, host: "mbp"},
		{branch: "feat/115-omi", name: "jade", tier: "standard", state: "working", age: 8, host: "buildbox"},
		{branch: "feat/116-pi", name: "ruby", tier: "standard", state: "done", age: 1200},
	}...)
}

type dotCase struct {
	name    string
	workers []dotWorker
	holds   int
}

var dotCases = []dotCase{
	{"0-workers", nil, 0},
	{"1-workers", []dotWorker{{branch: "feat/1-solo", name: "lime", tier: "standard", state: "working", age: 3, roles: roleOne}}, 0},
	{"2-workers", []dotWorker{
		{branch: "feat/1-solo", name: "lime", tier: "standard", state: "working", age: 3, roles: roleOne},
		{branch: "feat/2-duo", name: "sky", tier: "deep", state: "blocked", age: 5},
	}, 0},
	{"8-workers", eightWorkers(), 0},
	{"16-workers", sixteenWorkers(), 2},
}

func emit(m map[string]any) (string, scene, legend) {
	ps, lg := buildPlanets(m, dotNow)
	_, sc := arrange(newSlotState(), ps)
	return renderDOT(sc, lg), sc, lg
}

func TestDotGoldens(t *testing.T) {
	for _, c := range dotCases {
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := emit(dotModel(c.workers, c.holds))
			path := filepath.Join("testdata", c.name+".dot")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if got != string(want) {
				t.Errorf("%s differs from the golden (run with -update to accept)", path)
			}
		})
	}
}

// TestDotEveryRenderIsLabelledAndFitted: every planet carries its codename, every
// picture has the legend, and the invisible corners that size the canvas are
// centred on the dispatcher and hug the content.
func TestDotEveryRenderIsLabelledAndFitted(t *testing.T) {
	for _, c := range dotCases {
		t.Run(c.name, func(t *testing.T) {
			out, sc, _ := emit(dotModel(c.workers, c.holds))
			if !strings.Contains(out, "working ") || !strings.Contains(out, "size = tier") {
				t.Errorf("no legend in\n%s", out)
			}
			for _, p := range sc.planets {
				if p.name == "" || !strings.Contains(out, "label="+dotq(p.label)) {
					t.Errorf("planet %s is unlabelled", p.branch)
				}
			}
			if len(c.workers) == 0 {
				return
			}
			var xs, ys []float64
			for _, id := range []string{"c1", "c2", "c3", "c4"} {
				m := regexp.MustCompile(`pos="(-?[0-9.]+),(-?[0-9.]+)!"`).FindStringSubmatch(nodeLine(t, out, id))
				x, _ := strconv.ParseFloat(m[1], 64)
				y, _ := strconv.ParseFloat(m[2], 64)
				xs, ys = append(xs, x), append(ys, y)
			}
			if xs[0] != -xs[1] || xs[2] != -xs[3] || ys[0] != -ys[2] || ys[1] != -ys[3] {
				t.Errorf("canvas not centred on the dispatcher: x %v y %v", xs, ys)
			}
			if margin := ys[0] - sc.extent; margin < 0 || margin > 2*canvasPad {
				t.Errorf("margin around the outermost planet = %v", margin)
			}
		})
	}
}

func TestDotDeterministicAndInputOrderFree(t *testing.T) {
	m := dotModel(sixteenWorkers(), 1)
	a, _, _ := emit(m)
	b, _, _ := emit(m)
	if a != b {
		t.Error("rendering twice gave different text")
	}
	ws := sixteenWorkers()
	slices.Reverse(ws)
	c, _, _ := emit(dotModel(ws, 1))
	if a != c {
		t.Error("input row order changed the output")
	}
}

// idOf maps each drawn planet's branch to its node id: ids follow branch order.
func idOf(sc scene) map[string]string {
	names := branches(sc)
	slices.Sort(names)
	ids := map[string]string{}
	for i, b := range names {
		ids[b] = "p" + strconv.Itoa(i+1)
	}
	return ids
}

func TestDotAeyeClasses(t *testing.T) {
	out, sc, _ := emit(dotModel(sixteenWorkers(), 0))
	ids := idOf(sc)
	class := map[string]string{}
	working := map[string]bool{}
	for _, p := range sc.planets {
		class[ids[p.branch]] = p.class
		for k, m := range p.moons {
			working[ids[p.branch]+"m"+strconv.Itoa(k+1)] = m.working
		}
	}
	var pulse, flow, glow int
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "aeye-pulse"):
			pulse++
			id := strings.Fields(line)[0]
			if class[id] != classBlocked || strings.Contains(line, "->") {
				t.Errorf("aeye-pulse outside a blocked planet node: %s", line)
			}
		case strings.Contains(line, "aeye-flow"):
			flow++
			f := strings.Fields(line)
			if len(f) < 3 || f[0] != "dispatcher" || f[1] != "->" || class[f[2]] != classWorking {
				t.Errorf("aeye-flow outside a working planet's spoke: %s", line)
			}
		case strings.Contains(line, "aeye-glow"):
			glow++
			if id := strings.Fields(line)[0]; !working[id] {
				t.Errorf("aeye-glow outside a working moon: %s", line)
			}
		}
	}
	var wantPulse, wantFlow, wantGlow int
	for _, p := range sc.planets {
		switch p.class {
		case classBlocked:
			wantPulse++
		case classWorking:
			wantFlow++
		}
		for _, m := range p.moons {
			if m.working {
				wantGlow++
			}
		}
	}
	if pulse != wantPulse || flow != wantFlow || glow != wantGlow || pulse == 0 || flow == 0 || glow == 0 {
		t.Errorf("pulse/flow/glow = %d/%d/%d, want %d/%d/%d (all nonzero)", pulse, flow, glow, wantPulse, wantFlow, wantGlow)
	}
}

// animated lists the node ids carrying each aeye class in a DOT text: a flow
// spoke by the planet it feeds.
func animated(out string) (flow, pulse, glow []string) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case strings.Contains(line, "aeye-flow"):
			flow = append(flow, f[2])
		case strings.Contains(line, "aeye-pulse"):
			pulse = append(pulse, f[0])
		case strings.Contains(line, "aeye-glow"):
			glow = append(glow, f[0])
		}
	}
	slices.Sort(flow)
	slices.Sort(pulse)
	slices.Sort(glow)
	return
}

// TestAnimationBudget: motion marks what is live and recent, so each class caps
// at maxAnimated elements, newest first, ties by branch; glow counts moons.
func TestAnimationBudget(t *testing.T) {
	crowd := func(state string, n int, age func(i int) float64, roles [][2]string) []dotWorker {
		var ws []dotWorker
		for i := range n {
			ws = append(ws, dotWorker{branch: fmt.Sprintf("feat/%02d-w", i), name: "n", tier: "standard", state: state, age: age(i), roles: roles})
		}
		slices.Reverse(ws)
		return ws
	}
	// ids of the workers at the given indexes of a crowd, sorted as the output is.
	want := func(sc scene, idx ...int) []string {
		ids := idOf(sc)
		var out []string
		for _, i := range idx {
			out = append(out, ids[fmt.Sprintf("feat/%02d-w", i)])
		}
		slices.Sort(out)
		return out
	}
	check := func(t *testing.T, what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}

	t.Run("flow keeps the eight most recent working planets, ties by branch", func(t *testing.T) {
		// workers 0-5 are newest in order; 6-11 tie, so the cut falls on 6 and 7.
		out, sc, _ := emit(dotModel(crowd("working", 12, func(i int) float64 { return float64(10 * min(i+1, 7)) }, nil), 0))
		flow, _, _ := animated(out)
		check(t, "flow", flow, want(sc, 0, 1, 2, 3, 4, 5, 6, 7))
	})
	t.Run("pulse keeps the eight most recent blocked planets", func(t *testing.T) {
		out, sc, _ := emit(dotModel(crowd("blocked", 10, func(i int) float64 { return float64(10 * (10 - i)) }, nil), 0))
		_, pulse, _ := animated(out)
		check(t, "pulse", pulse, want(sc, 2, 3, 4, 5, 6, 7, 8, 9))
	})
	t.Run("glow counts moons, walking planets newest first", func(t *testing.T) {
		three := [][2]string{{"a", "working"}, {"b", "working"}, {"c", "working"}}
		out, sc, _ := emit(dotModel(crowd("working", 5, func(i int) float64 { return float64(10 * (i + 1)) }, three), 0))
		_, _, glow := animated(out)
		ids := idOf(sc)
		var wantGlow []string
		for i, moons := range []int{3, 3, 2} {
			for k := range moons {
				wantGlow = append(wantGlow, fmt.Sprintf("%sm%d", ids[fmt.Sprintf("feat/%02d-w", i)], k+1))
			}
		}
		slices.Sort(wantGlow)
		check(t, "glow", glow, wantGlow)
	})
	t.Run("a roster at the cap is unchanged", func(t *testing.T) {
		out, sc, _ := emit(dotModel(crowd("working", maxAnimated, func(i int) float64 { return float64(i + 1) }, roleOne), 0))
		flow, _, glow := animated(out)
		check(t, "flow", flow, want(sc, 0, 1, 2, 3, 4, 5, 6, 7))
		if len(glow) != maxAnimated {
			t.Errorf("glow = %v, want one per planet", glow)
		}
	})
}

func TestDotShapesAndStacks(t *testing.T) {
	out, sc, lg := emit(dotModel(sixteenWorkers(), 2))
	ids := idOf(sc)
	hexes := 0
	for _, p := range sc.planets {
		line := nodeLine(t, out, ids[p.branch])
		if got := strings.Contains(line, "shape=hexagon"); got != (p.host != "") {
			t.Errorf("%s hexagon = %v, host %q", p.branch, got, p.host)
		}
		if p.host != "" {
			hexes++
		}
	}
	if hexes != 3 {
		t.Errorf("hexagons = %d, want 3", hexes)
	}
	stack := ids["feat/104-delta"] + " -> " + ids["feat/101-alpha"]
	if !strings.Contains(out, stack+" [") || !strings.Contains(out, "style=dashed") {
		t.Errorf("no dashed stacked edge %q in\n%s", stack, out)
	}
	if lg.done != 3 || lg.failed != 1 || lg.holds != 2 {
		t.Errorf("legend = %+v", lg)
	}
	for _, host := range []string{`label="mbp"`, `label="buildbox"`} {
		if !strings.Contains(out, host) {
			t.Errorf("no host label %s", host)
		}
	}
}

func nodeLine(t *testing.T, out, id string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  "+id+" [") {
			return line
		}
	}
	t.Fatalf("no node line for %s", id)
	return ""
}

func TestDotUsesAeyesFont(t *testing.T) {
	out, _, _ := emit(dotModel(eightWorkers(), 0))
	if strings.Contains(out, "sans-serif") || strings.Count(out, `fontname="Source Sans 3"`) != 2 {
		t.Errorf("fonts are not aeye's Source Sans 3:\n%s", out)
	}
}

func TestDotLegend(t *testing.T) {
	out, _, _ := emit(dotModel(sixteenWorkers(), 2))
	_, label, ok := strings.Cut(out, "label=<")
	if !ok {
		t.Fatal("no HTML legend label")
	}
	for _, want := range []string{
		`<font color="#a6e3a1">●</font>`, `<font color="#f38ba8">●</font>`, `<font color="#89b4fa">●</font>`,
		`<font color="#45475a">`, `<font color="#7f4a5a">`, "working 8", "blocked 2", "pr open 2", "done 3", "failed 1", "holds 2",
		"size = tier", "small dot = role", "stacked on", "hexagon = remote host", "→",
	} {
		if !strings.Contains(label, want) {
			t.Errorf("legend lacks %q", want)
		}
	}
	for _, tofu := range []string{"⬡", "⇢"} {
		if strings.Contains(label, tofu) {
			t.Errorf("legend holds %q, which aeye's font does not draw", tofu)
		}
	}
	zero, _, _ := emit(dotModel(nil, 0))
	for _, no := range []string{"failed", "holds"} {
		if strings.Contains(zero, no) {
			t.Errorf("empty roster legend names %q", no)
		}
	}
	for _, want := range []string{"working 0", "blocked 0", "pr open 0", "done 0"} {
		if !strings.Contains(zero, want) {
			t.Errorf("empty roster legend lacks %q", want)
		}
	}
}

func TestDotqEscapes(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:    `"plain"`,
		`a"b`:      `"a\"b"`,
		`a\b`:      `"a\\b"`,
		"a\nb":     `"a\nb"`,
		`\"`:       `"\\\""`,
		"x$<y>{z}": `"x$<y>{z}"`,
		"":         `""`,
	} {
		if got := dotq(in); got != want {
			t.Errorf("dotq(%q) = %s, want %s", in, got, want)
		}
	}
}

// balanced reports whether a line closes every quoted string it opens.
func balanced(line string) bool {
	open := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if open {
				i++
			}
		case '"':
			open = !open
		}
	}
	return !open
}

func TestDotHostileTextCannotBreakOut(t *testing.T) {
	hostile := "ZQ\"x\\y\nw$<b>"
	ws := eightWorkers()
	ws[0].name = hostile
	ws[4].host = hostile
	ws[1].branch = "feat/102-\"q\\<u>$"
	ws[3].base = hostile
	out, _, _ := emit(dotModel(ws, 1))
	for _, line := range strings.Split(out, "\n") {
		if !balanced(line) {
			t.Errorf("unbalanced quotes: %s", line)
		}
	}
	for _, bad := range []string{"$", "<b>", "<u>", "ZQ\"", `\y`} {
		if strings.Contains(out, bad) {
			t.Errorf("output carries %q", bad)
		}
	}
	_, legendHTML, _ := strings.Cut(out, "label=<")
	if strings.Contains(legendHTML, "ZQ") || strings.Count(legendHTML, "<table") != 1 || strings.Count(legendHTML, "</table>") != 1 {
		t.Errorf("legend is not the fixed table: %s", legendHTML)
	}
}

func TestDotIDsDoNotDeriveFromUserText(t *testing.T) {
	out, _, _ := emit(dotModel(eightWorkers(), 0))
	stmt := regexp.MustCompile(`^  (\S+)(?: -> (\S+))? \[`)
	id := regexp.MustCompile(`^(p\d+(m\d+)?|h\d+|c\d+|dispatcher|node|edge)$`)
	for _, line := range strings.Split(out, "\n") {
		m := stmt.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, name := range m[1:] {
			if name != "" && !id.MatchString(name) {
				t.Errorf("id %q is not a generated one: %s", name, line)
			}
		}
	}
}

func neatoPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("neato")
	if err != nil {
		t.Skip("neato not on PATH")
	}
	return p
}

func neatoSVG(t *testing.T, neato, dot string) []byte {
	t.Helper()
	cmd := exec.Command(neato, "-n", "-Tsvg")
	cmd.Stdin = strings.NewReader(dot)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("neato: %v\n%s", err, stderr.String())
	}
	return stdout.Bytes()
}

var viewBoxRe = regexp.MustCompile(`viewBox="([-0-9. ]+)"`)

// svgNode is one element of a parsed svg, as little as the class tests read.
type svgNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []svgNode  `xml:",any"`
	Text    string     `xml:",chardata"`
}

func (n svgNode) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (n svgNode) child(tag string) (svgNode, bool) {
	for _, c := range n.Nodes {
		if c.XMLName.Local == tag {
			return c, true
		}
	}
	return svgNode{}, false
}

func (n svgNode) title() string {
	c, _ := n.child("title")
	return c.Text
}

func (n svgNode) walk(f func(svgNode)) {
	f(n)
	for _, c := range n.Nodes {
		c.walk(f)
	}
}

func parseSVG(t *testing.T, svg []byte) svgNode {
	t.Helper()
	var root svgNode
	if err := xml.Unmarshal(svg, &root); err != nil {
		t.Fatalf("svg does not parse: %v", err)
	}
	return root
}

func TestFinishSVG(t *testing.T) {
	const in = `<svg width="546pt" height="561pt"
 viewBox="0.00 0.00 546.00 561.00" xmlns="http://www.w3.org/2000/svg">
<g id="edge1" class="edge aeye&#45;flow">
<title>dispatcher&#45;&gt;p1</title>
</g>
<text font-family="Source Sans 3" font-size="13.00">a</text>
<text font-family="Source Sans 3" font-size="12.00">b</text>
</svg>
`
	got := string(finishSVG([]byte(in)))
	if !strings.Contains(got, `class="edge aeye-flow"`) || strings.Contains(got, "aeye&#45;") {
		t.Errorf("the class hook is not plain text:\n%s", got)
	}
	if !strings.Contains(got, "<title>dispatcher&#45;&gt;p1</title>") {
		t.Errorf("a hyphen outside the class was rewritten:\n%s", got)
	}
	if n := strings.Count(got, `font-family="'Source Sans 3'"`); n != 2 || strings.Contains(got, `font-family="Source Sans 3"`) {
		t.Errorf("%d font families quoted, want both:\n%s", n, got)
	}
	const rect = `<rect x="0.00" y="0.00" width="546.00" height="561.00" fill="#1e1e2e"/>`
	open := regexp.MustCompile(`<svg[^>]*>`).FindStringIndex(got)
	if open == nil || !strings.HasPrefix(got[open[1]:], rect) {
		t.Errorf("the first child of <svg> is not %s:\n%s", rect, got)
	}
	if strings.Contains(got, "%") {
		t.Errorf("the background is not in absolute units:\n%s", got)
	}

	t.Run("a viewBox origin is echoed", func(t *testing.T) {
		got := string(finishSVG([]byte(`<svg viewBox="-10.50 20 300 400.25"><g/></svg>`)))
		if want := `<svg viewBox="-10.50 20 300 400.25"><rect x="-10.50" y="20" width="300" height="400.25" fill="#1e1e2e"/><g/></svg>`; got != want {
			t.Errorf("got %s\nwant %s", got, want)
		}
	})
	for name, in := range map[string]string{
		"no svg root":    "digraph roster {\n  dispatcher;\n}\n",
		"no viewBox":     "<svg>\n  p1 [class=\"aeye&#45;flow\"];\n</svg>\n",
		"empty document": "",
	} {
		t.Run(name+" is returned as it came", func(t *testing.T) {
			if got := string(finishSVG([]byte(in))); got != in {
				t.Errorf("got %q, want %q", got, in)
			}
		})
	}
}

var (
	pathNumRe = regexp.MustCompile(`-?[0-9.]+`)
	// hookRe is aeye's frame injector's hook detection (animate.go).
	hookRe = regexp.MustCompile(`class="(?:[^"]*\s)?aeye-(?:flow|pulse|glow)(?:\s|")`)
)

func TestRenderedSVGClasses(t *testing.T) {
	neato := neatoPath(t)
	dot, sc, _ := emit(dotModel(sixteenWorkers(), 2))
	ids := idOf(sc)
	want := map[string][]string{}
	for _, p := range sc.planets {
		id := ids[p.branch]
		switch {
		case p.class == classWorking:
			want["aeye-flow"] = append(want["aeye-flow"], "dispatcher->"+id)
		case p.class == classBlocked && !p.failed:
			want["aeye-pulse"] = append(want["aeye-pulse"], id)
		}
		for k, m := range p.moons {
			if m.working {
				want["aeye-glow"] = append(want["aeye-glow"], id+"m"+strconv.Itoa(k+1))
			}
		}
	}
	hooks := 0
	for _, class := range []string{"aeye-flow", "aeye-pulse", "aeye-glow"} {
		if len(want[class]) == 0 {
			t.Fatalf("the roster has no %s element", class)
		}
		slices.Sort(want[class])
		hooks += len(want[class])
	}

	svg := finishSVG(neatoSVG(t, neato, dot))
	root := parseSVG(t, svg)
	got := map[string][]string{}
	var flows []svgNode
	var sun svgNode
	root.walk(func(n svgNode) {
		if n.XMLName.Local == "g" && n.title() == "dispatcher" {
			sun, _ = n.child("ellipse")
		}
		for _, class := range strings.Fields(n.attr("class")) {
			if !strings.HasPrefix(class, "aeye-") {
				continue
			}
			if n.XMLName.Local != "g" {
				t.Errorf("%s on <%s>, want <g>", class, n.XMLName.Local)
				continue
			}
			got[class] = append(got[class], n.title())
			if class == "aeye-flow" {
				flows = append(flows, n)
			}
		}
	})
	for class := range got {
		if _, ok := want[class]; !ok {
			t.Errorf("unexpected class %s on %v", class, got[class])
		}
	}
	for class, titles := range want {
		slices.Sort(got[class])
		if !slices.Equal(got[class], titles) {
			t.Errorf("%s elements = %v, want %v", class, got[class], titles)
		}
	}

	cx, _ := strconv.ParseFloat(sun.attr("cx"), 64)
	cy, _ := strconv.ParseFloat(sun.attr("cy"), 64)
	for _, g := range flows {
		path, ok := g.child("path")
		nums := pathNumRe.FindAllString(path.attr("d"), -1)
		if !ok || len(nums) < 4 {
			t.Errorf("%s has no path with a start and an end", g.title())
			continue
		}
		at := func(i int) float64 {
			x, _ := strconv.ParseFloat(nums[i], 64)
			y, _ := strconv.ParseFloat(nums[i+1], 64)
			return math.Hypot(x-cx, y-cy)
		}
		if start, end := at(0), at(len(nums)-2); start >= end {
			t.Errorf("%s runs towards the dispatcher: starts %.1f away, ends %.1f", g.title(), start, end)
		}
	}

	if n := len(hookRe.FindAll(svg, -1)); n != hooks {
		t.Errorf("aeye's hook regex matches %d times, want once per hook element (%d)", n, hooks)
	}

	vb := viewBoxRe.FindSubmatch(svg)
	if vb == nil {
		t.Fatal("svg has no viewBox")
	}
	f := strings.Fields(string(vb[1]))
	first := root.Nodes[0]
	if first.XMLName.Local != "rect" || first.attr("fill") != colBG ||
		first.attr("x") != f[0] || first.attr("y") != f[1] || first.attr("width") != f[2] || first.attr("height") != f[3] {
		t.Errorf("the first child of <svg> is %+v, want a %s rect over viewBox %v", first.Attrs, colBG, f)
	}
	// aeye zooms by rewriting the root viewBox to the crop window.
	x, _ := strconv.ParseFloat(f[0], 64)
	w, _ := strconv.ParseFloat(f[2], 64)
	crop := fmt.Sprintf(`viewBox="%.2f %s %.2f %s"`, x+0.75*w, f[1], w/4, f[3])
	zoomed := parseSVG(t, viewBoxRe.ReplaceAll(svg, []byte(crop)))
	rx, _ := strconv.ParseFloat(zoomed.Nodes[0].attr("x"), 64)
	rw, _ := strconv.ParseFloat(zoomed.Nodes[0].attr("width"), 64)
	if rx > x+0.75*w || rx+rw < x+w {
		t.Errorf("the background rect [%v, %v] does not cover the cropped viewBox %s", rx, rx+rw, crop)
	}
}

func TestRenderedSVGText(t *testing.T) {
	neato := neatoPath(t)
	resvg, err := exec.LookPath("resvg")
	if err != nil {
		t.Skip("resvg not on PATH")
	}
	fonts := os.Getenv("AEYE_D2_FONT_DIR")
	if fonts == "" {
		t.Skip("AEYE_D2_FONT_DIR not set")
	}
	dot, err := os.ReadFile(filepath.Join("testdata", "16-workers.dot"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "16.svg")
	if err := os.WriteFile(in, finishSVG(neatoSVG(t, neato, string(dot))), 0o644); err != nil {
		t.Fatal(err)
	}
	// The carousel's resvg: one font directory, no system fonts, no generic families.
	cmd := exec.Command(resvg, "--skip-system-fonts", "--use-fonts-dir", fonts, "--font-family", diagramFont,
		"--serif-family", diagramFont, "--sans-serif-family", diagramFont, in, filepath.Join(dir, "16.png"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("resvg: %v\n%s", err, stderr.String())
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		t.Errorf("resvg warned on the finished svg:\n%s", s)
	}
}

func TestRenderedSVG(t *testing.T) {
	neato := neatoPath(t)
	for _, c := range dotCases {
		t.Run(c.name, func(t *testing.T) {
			dot, err := os.ReadFile(filepath.Join("testdata", c.name+".dot"))
			if err != nil {
				t.Fatal(err)
			}
			svg := bytes.ReplaceAll(neatoSVG(t, neato, string(dot)), []byte("&#45;"), []byte("-"))
			m := viewBoxRe.FindSubmatch(svg)
			if m == nil {
				t.Fatal("svg has no viewBox")
			}
			f := strings.Fields(string(m[1]))
			w, _ := strconv.ParseFloat(f[2], 64)
			h, _ := strconv.ParseFloat(f[3], 64)
			if math.Abs(w-h) > 0.05*math.Max(w, h) {
				t.Errorf("viewBox %vx%v is not square", w, h)
			}
			if strings.HasPrefix(c.name, "0-") || strings.HasPrefix(c.name, "1-") {
				return
			}
			for _, class := range []string{"aeye-pulse", "aeye-flow", "aeye-glow"} {
				if !regexp.MustCompile(`<g id="[^"]*" class="[^"]*` + class).Match(svg) {
					t.Errorf("no <g> carries %s", class)
				}
			}
			if c.name == "16-workers" {
				if err := os.WriteFile(filepath.Join(t.TempDir(), "16.svg"), svg, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
