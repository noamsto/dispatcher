package rosterrender

import (
	"bytes"
	"cmp"
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

// cardSamples are the title, engine, model and detail a hand-built row gets,
// picked by its branch so the output does not depend on row order.
var cardSamples = [][4]string{
	{"Render the roster as rounded task cards with a title and a status line", "claude", "claude-opus-4-1", "running the targeted go tests for the card text"},
	{"Fix the flaky retry in the sync watcher", "pi", "deepseek/deepseek-v4.1-flash", "reading crew/internal/rosterrender/layout.go"},
	{"Port the Supercalifragilisticexpialidocious importer to Go and cover its edge cases with table tests", "claude", "claude-sonnet-4-5", "waiting for the plan critic's verdict on the second revision"},
	{"Bump deps", "codex", "gpt-5", "pushed the branch"},
	{"Quote the \"state\" & <tier> words in the legend", "claude", "claude-haiku-4-5", "editing dot.go"},
}

func cardSample(branch string) [4]string {
	var n uint
	for _, r := range branch {
		n = n*31 + uint(r)
	}
	return cardSamples[n%uint(len(cardSamples))]
}

func dotModel(ws []dotWorker, holds int) map[string]any {
	m := emptyModel()
	for _, w := range ws {
		s := cardSample(w.branch)
		row := map[string]any{
			"branch": w.branch, "name": w.name, "tier": w.tier, "state": w.state,
			"title": s[0], "engine": s[1], "model": s[2], "detail": s[3],
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

// repoWorker is a roster row of a multi-repo case with the fields the bus
// model alone does not carry: its repo, CI state and whether it waits on a human.
type repoWorker struct {
	dotWorker
	repo, ci string
	waiting  bool
}

// repoPlanets builds the planets and legend of a hand-built multi-repo roster.
func repoPlanets(ws []repoWorker) ([]planet, legend) {
	var rows []dotWorker
	for _, w := range ws {
		rows = append(rows, w.dotWorker)
	}
	ps, lg := buildPlanets(dotModel(rows, 0), dotNow)
	// buildPlanets keeps the rows in input order.
	for i, w := range ws {
		ps[i].repo, ps[i].repoLabel = "/git/"+w.repo+".git", w.repo
		ps[i].ci, ps[i].waiting = w.ci, w.waiting
	}
	tally(ps, &lg)
	return ps, lg
}

// wedgeRoster is two repos: a waiting planet, a pr_open planet per CI state, a
// stale working planet, a stacked planet, a remote one, and the branch
// feat/1-shared in both repos.
func wedgeRoster() []repoWorker {
	return []repoWorker{
		{dotWorker: dotWorker{branch: "feat/1-shared", name: "sky", tier: "standard", state: "blocked", age: 60}, repo: "alpha", waiting: true},
		{dotWorker: dotWorker{branch: "feat/2-pass", name: "rose", tier: "trivial", state: "pr_open", age: 90}, repo: "alpha", ci: "pass"},
		{dotWorker: dotWorker{branch: "feat/3-run", name: "opal", tier: "standard", state: "pr_open", age: 90}, repo: "alpha", ci: "pending"},
		{dotWorker: dotWorker{branch: "feat/4-fail", name: "coral", tier: "standard", state: "pr_open", age: 90}, repo: "alpha", ci: "fail"},
		{dotWorker: dotWorker{branch: "feat/5-none", name: "mint", tier: "deep", state: "pr_open", age: 90}, repo: "alpha", ci: "none"},
		{dotWorker: dotWorker{branch: "feat/6-old", name: "dune", tier: "standard", state: "working", age: 3600}, repo: "alpha"},
		{dotWorker: dotWorker{branch: "feat/1-shared", name: "lime", tier: "deep", state: "working", age: 30}, repo: "beta"},
		{dotWorker: dotWorker{branch: "feat/7-stack", name: "teal", tier: "standard", state: "working", age: 20, base: "feat/1-shared", roles: roleOne}, repo: "beta"},
		{dotWorker: dotWorker{branch: "feat/8-remote", name: "olive", tier: "standard", state: "working", age: 10, host: "mbp"}, repo: "beta"},
		{dotWorker: dotWorker{branch: "feat/9-block", name: "plum", tier: "standard", state: "blocked", age: 25}, repo: "beta"},
	}
}

// wedge3Roster is three repos of 3, 3 and 2 workers, narrow enough for a ring to
// skip a wedge: a stacked pair, a remote worker, CI badges, a waiting worker and
// role moons.
func wedge3Roster() []repoWorker {
	return []repoWorker{
		{dotWorker: dotWorker{branch: "feat/11-api", name: "sky", tier: "deep", state: "blocked", age: 60, roles: roleOne}, repo: "alpha", waiting: true},
		{dotWorker: dotWorker{branch: "feat/12-docs", name: "rose", tier: "trivial", state: "pr_open", age: 90}, repo: "alpha", ci: "pass"},
		{dotWorker: dotWorker{branch: "feat/13-cli", name: "opal", tier: "standard", state: "working", age: 20, base: "feat/11-api", roles: roleBoth}, repo: "alpha"},
		{dotWorker: dotWorker{branch: "feat/21-ui", name: "lime", tier: "standard", state: "working", age: 30, roles: roleOne}, repo: "beta"},
		{dotWorker: dotWorker{branch: "feat/22-db", name: "coral", tier: "standard", state: "pr_open", age: 90}, repo: "beta", ci: "fail"},
		{dotWorker: dotWorker{branch: "feat/23-remote", name: "olive", tier: "deep", state: "working", age: 10, host: "mbp"}, repo: "beta", ci: "pending"},
		{dotWorker: dotWorker{branch: "feat/31-ops", name: "mint", tier: "trivial", state: "working", age: 15}, repo: "gamma", ci: "none"},
		{dotWorker: dotWorker{branch: "feat/32-old", name: "dune", tier: "standard", state: "working", age: 3600}, repo: "gamma"},
	}
}

func emitRepos(ws []repoWorker) (string, scene, legend) {
	ps, lg := repoPlanets(ws)
	_, sc := arrange(newSlotState(), ps)
	return renderDOT(sc, lg, legendFull), sc, lg
}

type dotCase struct {
	name    string
	workers []dotWorker
	holds   int
	repos   []repoWorker // a multi-repo case, in place of workers
}

func (c dotCase) emit() (string, scene, legend) {
	if c.repos != nil {
		return emitRepos(c.repos)
	}
	return emit(dotModel(c.workers, c.holds))
}

var dotCases = []dotCase{
	{"0-workers", nil, 0, nil},
	{"1-workers", []dotWorker{{branch: "feat/1-solo", name: "lime", tier: "standard", state: "working", age: 3, roles: roleOne}}, 0, nil},
	{"2-workers", []dotWorker{
		{branch: "feat/1-solo", name: "lime", tier: "standard", state: "working", age: 3, roles: roleOne},
		{branch: "feat/2-duo", name: "sky", tier: "deep", state: "blocked", age: 5},
	}, 0, nil},
	{"8-workers", eightWorkers(), 0, nil},
	{"16-workers", sixteenWorkers(), 2, nil},
	{"wedges", nil, 0, wedgeRoster()},
	{"wedges3", nil, 0, wedge3Roster()},
}

func emit(m map[string]any) (string, scene, legend) {
	ps, lg := buildPlanets(m, dotNow)
	_, sc := arrange(newSlotState(), ps)
	placeHolds(&sc, buildHolds(m))
	return renderDOT(sc, lg, legendFull), sc, lg
}

func TestDotGoldens(t *testing.T) {
	for _, c := range dotCases {
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := c.emit()
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
			out, sc, _ := c.emit()
			if !strings.Contains(out, "working ") || !strings.Contains(out, "border weight = tier") {
				t.Errorf("no legend in\n%s", out)
			}
			for _, p := range sc.planets {
				if p.name == "" || !strings.Contains(out, htmlText(cardTextOf(p.planet).rows[0])) {
					t.Errorf("planet %s is unlabelled", p.branch)
				}
			}
			if len(sc.planets) == 0 {
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

// keyIDs maps each drawn planet to its node id: ids follow (repo, branch) order.
func keyIDs(sc scene) map[planetKey]string {
	keys := make([]planetKey, len(sc.planets))
	for i, p := range sc.planets {
		keys[i] = p.key()
	}
	slices.SortFunc(keys, func(x, y planetKey) int {
		return cmp.Or(strings.Compare(x.repo, y.repo), strings.Compare(x.branch, y.branch))
	})
	ids := map[planetKey]string{}
	for i, k := range keys {
		ids[k] = "p" + strconv.Itoa(i+1)
	}
	return ids
}

// idOf maps each drawn planet's branch to its node id; a branch drawn in two
// repos has two ids, so only keyIDs tells those apart.
func idOf(sc scene) map[string]string {
	ids := map[string]string{}
	for k, id := range keyIDs(sc) {
		ids[k.branch] = id
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
		if !strings.Contains(line, "shape=box") || !strings.Contains(line, `rounded,filled`) || strings.Contains(line, "shape=hexagon") {
			t.Errorf("%s is not a rounded card: %s", p.branch, line)
		}
		badge := regexp.MustCompile(`(?m)^  ` + ids[p.branch] + `h \[`).MatchString(out)
		if badge != (p.host != "") {
			t.Errorf("%s hexagon badge = %v, host %q", p.branch, badge, p.host)
		}
		if badge {
			hexes++
			x, y := posOf(t, nodeLine(t, out, ids[p.branch]+"h"))
			if !strings.Contains(nodeLine(t, out, ids[p.branch]+"h"), "shape=hexagon") ||
				math.Abs(x-(p.x-cardW*72/2)) > 0.011 || math.Abs(y-(p.y+cardH*72/2)) > 0.011 {
				t.Errorf("%s host badge at (%v, %v), want the card's upper left corner", p.branch, x, y)
			}
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

func TestDotCardsNeverOverlap(t *testing.T) {
	const gap = 4.0
	for _, c := range dotCases {
		t.Run(c.name, func(t *testing.T) {
			_, sc, _ := c.emit()
			for i, p := range sc.planets {
				hw, hh := cardHalf(cardPen(p.tier))
				near := math.Hypot(max(math.Abs(p.x)-hw, 0), max(math.Abs(p.y)-hh, 0))
				if near < sunSize*72/2+planetPenwidth {
					t.Errorf("%s overlaps the dispatcher: %.1f away", p.branch, near)
				}
				for _, q := range sc.planets[i+1:] {
					qw, qh := cardHalf(cardPen(q.tier))
					if math.Abs(p.x-q.x) < hw+qw+gap && math.Abs(p.y-q.y) < hh+qh+gap {
						t.Errorf("%s (%.0f, %.0f) and %s (%.0f, %.0f) overlap", p.branch, p.x, p.y, q.branch, q.x, q.y)
					}
				}
			}
		})
	}
}

func TestDotCardCarriesItsText(t *testing.T) {
	ws := []dotWorker{
		{branch: "feat/1-long", name: "lime", tier: "deep", state: "working", age: 5},
		{branch: "feat/3-quote", name: "sky", tier: "standard", state: "pr_open", age: 5},
	}
	m := dotModel(ws, 0)
	rows := m["rows"].([]any)
	rows[0].(map[string]any)["title"] = "Port the Supercalifragilisticexpialidocious importer to Go and cover its edge cases with table tests"
	rows[0].(map[string]any)["engine"], rows[0].(map[string]any)["model"] = "claude", "claude-opus-4-1"
	rows[0].(map[string]any)["detail"] = "waiting for the plan critic's verdict on the second revision of the plan"
	rows[1].(map[string]any)["title"] = `Quote the "state" & <tier> words`
	rows[1].(map[string]any)["detail"] = ""
	out, sc, _ := emit(m)
	ids := keyIDs(sc)

	long := nodeLine(t, out, ids[placedBy(t, sc, "", "feat/1-long").key()])
	for _, want := range []string{
		`align="left">lime · #1</td><td align="left"><font color="` + colWorking + `">&#160;&#160;working</font></td>`,
		`<td align="left">Port the</td>`,
		`<td align="left">Supercalifragilisticexpialidoci…</td>`,
		`<td align="left"><font color="` + colLegend + `">deep·claude·opus</font></td>`,
		`<td align="left"><font color="` + colDim + `">waiting for the plan critic's…</font></td>`,
		`fixedsize="true" width="` + strconv.FormatFloat(cardInner, 'f', 1, 64) + `" height="` + strconv.FormatFloat(cardTableH, 'f', 1, 64) + `"`,
	} {
		if !strings.Contains(long, want) {
			t.Errorf("long card lacks %s:\n%s", want, long)
		}
	}
	if n := strings.Count(long, `<tr><td align="left">`); n != cardRows {
		t.Errorf("%d outer rows, want %d", n, cardRows)
	}

	quote := nodeLine(t, out, ids[placedBy(t, sc, "", "feat/3-quote").key()])
	for _, want := range []string{
		`<td align="left">Quote the &quot;state&quot; &amp; &lt;tier&gt;</td>`,
		`<td align="left">words</td>`,
		`<td align="left"><font color="` + colDim + `">&#160;</font></td>`,
		`<font color="` + colPROpen + `">&#160;&#160;pr open</font>`,
	} {
		if !strings.Contains(quote, want) {
			t.Errorf("quoted card lacks %s:\n%s", want, quote)
		}
	}
	if strings.Contains(quote, `"state"`) || strings.Contains(quote, "<tier>") {
		t.Errorf("title text reached the label unescaped:\n%s", quote)
	}
}

func TestDotUsesAeyesFont(t *testing.T) {
	out, _, _ := emit(dotModel(eightWorkers(), 0))
	if strings.Contains(out, "sans-serif") || strings.Count(out, `fontname="Source Sans 3"`) != 2 {
		t.Errorf("fonts are not aeye's Source Sans 3:\n%s", out)
	}
}

func TestDotLegend(t *testing.T) {
	out, _, _ := emit(dotModel(sixteenWorkers(), 2))
	_, label, ok := strings.Cut(out, "\n  label=<")
	if !ok {
		t.Fatal("no HTML legend label")
	}
	for _, want := range []string{
		`<font color="#a6e3a1">●</font>`, `<font color="#f38ba8">●</font>`, `<font color="#89b4fa">●</font>`,
		`<font color="#45475a">`, `<font color="#7f4a5a">`, "working 8", "blocked 2", "pr open 2", "done 3", "failed 1", "holds 2",
		"border weight = tier", "small dot = role", "stacked on", "hexagon = remote host", "→",
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
	_, legendHTML, _ := strings.Cut(out, "\n  label=<")
	if strings.Contains(legendHTML, "ZQ") || strings.Count(legendHTML, "<table") != 1 || strings.Count(legendHTML, "</table>") != 1 {
		t.Errorf("legend is not the fixed table: %s", legendHTML)
	}
}

func TestDotIDsDoNotDeriveFromUserText(t *testing.T) {
	wedges := wedgeRoster()
	for i := range wedges {
		wedges[i].repo = hostileRepo + wedges[i].repo
		wedges[i].branch = hostileRepo + wedges[i].branch
	}
	plain, _, _ := emit(dotModel(eightWorkers(), 0))
	multi, _, _ := emitRepos(wedges)
	stmt := regexp.MustCompile(`^  (\S+)(?: -> (\S+))? \[`)
	id := regexp.MustCompile(`^(p\d+([mwc]\d*|[hr])?|h\d+|c\d+|w\d+(s[01]|a\d+)?|sel\d*|dispatcher|node|edge)$`)
	for _, out := range []string{plain, multi} {
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
}

func neatoPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("neato")
	if err != nil {
		t.Skip("neato not on PATH")
	}
	return p
}

// neatoMeasuresABoundedFont reports whether Graphviz measures the diagram font
// with one of the two faces the card width table is calibrated against: Source
// Sans 3, or Noto Sans, which Pango picks when Source Sans 3 is missing. The
// trailing "3" in the family name is read as a size, so Pango never resolves to
// Source Sans 3 by name and the match is on the face it actually loads.
func neatoMeasuresABoundedFont(neato string) bool {
	cmd := exec.Command(neato, "-n", "-v", "-Tsvg")
	cmd.Stdin = strings.NewReader(`digraph g { a [pos="0,0!" fontname="` + diagramFont + `" label="x"]; }`)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if cmd.Run() != nil {
		return false
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if _, after, ok := strings.Cut(line, "resolved to:"); ok && (strings.Contains(after, "Noto Sans") || strings.Contains(after, diagramFont)) {
			return true
		}
	}
	return false
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

var cardTextXRe = regexp.MustCompile(`(?s)<title>p\d+</title>\s*<path[^>]* d="M(-?[0-9.]+),[^"]*"[^>]*/>\s*<text[^>]* x="(-?[0-9.]+)"`)

// TestCardTextStartsAtTheSamePlaceOnEveryCard renders every fixture with the
// real neato: the text must start the same distance from each card's outline,
// whatever Graphviz measures. The table is calibrated against Source Sans 3 and
// Noto Sans, so when neato measures with one of them it must not warn that a
// label is too small; with any other fallback (e.g. the nix sandbox) the table
// overflows by design and only the position check applies.
func TestCardTextStartsAtTheSamePlaceOnEveryCard(t *testing.T) {
	neato := neatoPath(t)
	checkWarnings := neatoMeasuresABoundedFont(neato)
	want, seen := 0.0, false
	for _, c := range dotCases {
		dot, _, _ := c.emit()
		cmd := exec.Command(neato, "-n", "-Tsvg")
		cmd.Stdin = strings.NewReader(dot)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: neato: %v\n%s", c.name, err, stderr.String())
		}
		if checkWarnings {
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.Contains(line, "Warning") {
					t.Errorf("%s: neato warned: %s", c.name, line)
				}
			}
		}
		for _, m := range cardTextXRe.FindAllSubmatch(stdout.Bytes(), -1) {
			edge, _ := strconv.ParseFloat(string(m[1]), 64)
			x, _ := strconv.ParseFloat(string(m[2]), 64)
			if !seen {
				want, seen = x-edge, true
			}
			if math.Abs(x-edge-want) > 0.02 {
				t.Errorf("%s: text starts %.2f from the outline, want %.2f", c.name, x-edge, want)
			}
		}
	}
	if !seen {
		t.Fatal("no card text found in the svg")
	}
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

func TestFinishSVGDotsOnlyFlowSpokes(t *testing.T) {
	const in = `<svg viewBox="0 0 10 10">
<g id="edge1" class="edge aeye&#45;flow">
<title>dispatcher&#45;&gt;p1</title>
<path fill="none" stroke="#585b70" stroke-width="1.5" stroke-dasharray="5,2" d="M1,1C2,2 3,3 4,4"/>
</g>
<g id="edge2" class="edge">
<title>dispatcher&#45;&gt;p2</title>
<path fill="none" stroke="#585b70" stroke-width="1.5" d="M1,1C2,2 3,3 4,4"/>
</g>
</svg>`
	got := string(finishSVG([]byte(in), nil))
	flow := `<path fill="none" stroke="#585b70" stroke-width="1.5" d="M1,1C2,2 3,3 4,4" stroke-dasharray="0.01 12" stroke-linecap="round"/>`
	if !strings.Contains(got, flow) || strings.Contains(got, "5,2") {
		t.Errorf("the flow spoke's path was not dotted:\n%s", got)
	}
	if n := strings.Count(got, "stroke-dasharray"); n != 1 {
		t.Errorf("%d dasharrays, want only the flow spoke's:\n%s", n, got)
	}
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
	got := string(finishSVG([]byte(in), nil))
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
		got := string(finishSVG([]byte(`<svg viewBox="-10.50 20 300 400.25"><g/></svg>`), nil))
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
			if got := string(finishSVG([]byte(in), nil)); got != in {
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

	svg := finishSVG(neatoSVG(t, neato, dot), sc.wedges)
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
	_, wedged, _ := emitRepos(wedgeRoster())
	for _, c := range []struct {
		golden string
		wedges []wedge
	}{{"16-workers", nil}, {"wedges", wedged.wedges}} {
		t.Run(c.golden, func(t *testing.T) {
			dot, err := os.ReadFile(filepath.Join("testdata", c.golden+".dot"))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			in := filepath.Join(dir, c.golden+".svg")
			if err := os.WriteFile(in, finishSVG(neatoSVG(t, neato, string(dot)), c.wedges), 0o644); err != nil {
				t.Fatal(err)
			}
			// The carousel's resvg: one font directory, no system fonts, no generic families.
			cmd := exec.Command(resvg, "--skip-system-fonts", "--use-fonts-dir", fonts, "--font-family", diagramFont,
				"--serif-family", diagramFont, "--sans-serif-family", diagramFont, in, filepath.Join(dir, c.golden+".png"))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("resvg: %v\n%s", err, stderr.String())
			}
			if s := strings.TrimSpace(stderr.String()); s != "" {
				t.Errorf("resvg warned on the finished svg:\n%s", s)
			}
		})
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

// hostileRepo is a repo name full of DOT and HTML syntax.
const hostileRepo = "ZQ\"x\\y\nw$<b>]"

var posRe = regexp.MustCompile(`pos="(-?[0-9.]+),(-?[0-9.]+)!"`)

func posOf(t *testing.T, line string) (x, y float64) {
	t.Helper()
	m := posRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no pos in %s", line)
	}
	x, _ = strconv.ParseFloat(m[1], 64)
	y, _ = strconv.ParseFloat(m[2], 64)
	return x, y
}

func placedBy(t *testing.T, sc scene, repo, branch string) placed {
	t.Helper()
	for _, p := range sc.planets {
		if p.repoLabel == repo && p.branch == branch {
			return p
		}
	}
	t.Fatalf("no planet %s in repo %s", branch, repo)
	return placed{}
}

// degrees is the clockwise angle from 12 o'clock of a point, in [0, 360).
func degrees(x, y float64) float64 { return math.Mod(math.Atan2(x, y)*180/math.Pi+360, 360) }

func TestDotWedges(t *testing.T) {
	out, sc, _ := emitRepos(wedgeRoster())
	if len(sc.wedges) != 2 {
		t.Fatalf("%d wedges, want 2", len(sc.wedges))
	}
	if n := len(regexp.MustCompile(`(?m)^  w\d+ \[`).FindAllString(out, -1)); n != len(sc.wedges) {
		t.Errorf("%d wedge label nodes, want %d", n, len(sc.wedges))
	}
	lastSpoke := strings.LastIndex(out, "dispatcher -> p")
	firstMoon := regexp.MustCompile(`(?m)^  p\d+m1 \[`).FindStringIndex(out)
	if firstMoon == nil {
		t.Fatal("the roster has no moon")
	}
	for i, w := range sc.wedges {
		id := "w" + strconv.Itoa(i+1)
		label := nodeLine(t, out, id)
		for _, want := range []string{"label=" + dotq(w.label), "shape=plaintext", "fixedsize=false", `fontcolor="` + colLegend + `"`,
			"pos=" + dotq(pt(w.x)+","+pt(w.y)+"!")} {
			if !strings.Contains(label, want) {
				t.Errorf("%s lacks %s: %s", id, want, label)
			}
		}

		s0, s1 := nodeLine(t, out, id+"s0"), nodeLine(t, out, id+"s1")
		x0, y0 := posOf(t, s0)
		x1, y1 := posOf(t, s1)
		if math.Abs(math.Hypot(x0, y0)-spokeInner) > 0.02 || math.Abs(math.Hypot(x1, y1)-w.rim) > 0.02 ||
			math.Abs(degrees(x0, y0)-w.start) > 0.02 || math.Abs(degrees(x1, y1)-w.start) > 0.02 {
			t.Errorf("%s spoke runs (%v, %v) to (%v, %v), want radius %v to %v at %v degrees", id, x0, y0, x1, y1, spokeInner, w.rim, w.start)
		}
		for _, line := range []string{s0, s1} {
			if !strings.Contains(line, "style=invis") {
				t.Errorf("a spoke end is drawn: %s", line)
			}
		}
		edge := nodeLine(t, out, id+"s0 -> "+id+"s1")
		for _, want := range []string{id + "s0 -> " + id + "s1", `color="` + colDim + staleAlpha + `"`, "penwidth=0.75"} {
			if !strings.Contains(edge, want) {
				t.Errorf("spoke edge lacks %s: %s", want, edge)
			}
		}
		if strings.Contains(edge, "dashed") {
			t.Errorf("a wedge divider is dashed: %s", edge)
		}

		points := len(regexp.MustCompile(`(?m)^  `+id+`a\d+ \[`).FindAllString(out, -1))
		if want := 1 + max(1, int(math.Ceil((w.span-2*arcInset)/arcStep))); points != want {
			t.Errorf("%s rim has %d points, want %d", id, points, want)
		}
		for k := range points {
			x, y := posOf(t, nodeLine(t, out, id+"a"+strconv.Itoa(k)))
			if math.Abs(math.Hypot(x, y)-w.rim) > 0.02 {
				t.Errorf("%s rim point %d is %v from the dispatcher, want %v", id, k, math.Hypot(x, y), w.rim)
			}
			if k == 0 || k == points-1 {
				want := w.start + arcInset
				if k > 0 {
					want = w.start + w.span - arcInset
				}
				if math.Abs(math.Mod(degrees(x, y)-want+540, 360)-180) > 0.05 {
					t.Errorf("%s rim point %d is at %v degrees, want %v", id, k, degrees(x, y), want)
				}
			}
			if k == 0 {
				continue
			}
			link := id + "a" + strconv.Itoa(k-1) + " -> " + id + "a" + strconv.Itoa(k) + " ["
			if !strings.Contains(out, link) {
				t.Errorf("no rim edge %s", link)
			}
		}
		for line := range strings.SplitSeq(out, "\n") {
			if strings.Contains(line, id+"a") && strings.Contains(line, "->") &&
				(strings.Contains(line, "dashed") || !strings.Contains(line, `color="`+colDim+staleAlpha+`"`) || !strings.Contains(line, "penwidth=0.75")) {
				t.Errorf("rim edge is not a thin, dim, solid line: %s", line)
			}
		}

		if at := strings.Index(out, "  "+id+"s0 ["); at < lastSpoke || at > firstMoon[0] {
			t.Errorf("%s is emitted outside the span between the spokes and the moons", id)
		}
	}
}

func TestDotWedgesAbsentForOneRepo(t *testing.T) {
	out, sc, _ := emit(dotModel(eightWorkers(), 0))
	if len(sc.wedges) != 0 || regexp.MustCompile(`(?m)^  w\d`).MatchString(out) {
		t.Errorf("a single repo drew wedges:\n%s", out)
	}
}

func TestDotSameBranchInTwoRepos(t *testing.T) {
	ws := wedgeRoster()
	ws[1].base = "feat/7-stack" // a branch of the other repo only
	out, sc, _ := emitRepos(ws)
	ids := keyIDs(sc)
	a, b := placedBy(t, sc, "alpha", "feat/1-shared"), placedBy(t, sc, "beta", "feat/1-shared")
	if ids[a.key()] == ids[b.key()] || len(ids) != len(sc.planets) {
		t.Fatalf("ids %v do not tell the planets of one branch apart", ids)
	}
	for _, p := range []placed{a, b} {
		line := nodeLine(t, out, ids[p.key()])
		if !strings.Contains(line, htmlText(cardTextOf(p.planet).rows[0])) {
			t.Errorf("%s of %s has the wrong node: %s", p.branch, p.repoLabel, line)
		}
	}
	if a.name == b.name {
		t.Fatalf("both planets are named %q, so the check above proves nothing", a.name)
	}

	stack := placedBy(t, sc, "beta", "feat/7-stack")
	if want := ids[stack.key()] + " -> " + ids[b.key()] + " ["; !strings.Contains(out, want) {
		t.Errorf("the stacked planet is not drawn on its own repo's branch (%s)", want)
	}
	if strings.Contains(out, ids[stack.key()]+" -> "+ids[a.key()]+" [") {
		t.Error("the stacked planet is drawn on the other repo's branch")
	}
	if n := strings.Count(out, "arrowhead=normal"); n != 1 {
		t.Errorf("%d stacked edges, want 1 (a base in another repo is no base)", n)
	}
}

func TestDotWaitingRing(t *testing.T) {
	out, sc, _ := emitRepos(wedgeRoster())
	ids := keyIDs(sc)
	var got []string
	for _, m := range regexp.MustCompile(`(?m)^  (p\d+)w \[`).FindAllStringSubmatch(out, -1) {
		got = append(got, m[1])
	}
	var want []string
	for _, p := range sc.planets {
		if p.waiting {
			want = append(want, ids[p.key()])
		}
	}
	if len(want) != 1 || !slices.Equal(got, want) {
		t.Fatalf("rings on %v, want only the waiting planets %v", got, want)
	}
	p := placedBy(t, sc, "alpha", "feat/1-shared")
	line := nodeLine(t, out, ids[p.key()]+"w")
	wd, ht := strconv.FormatFloat(cardW+2*waitGap/72, 'g', -1, 64), strconv.FormatFloat(cardH+2*waitGap/72, 'g', -1, 64)
	for _, w := range []string{`label=""`, "shape=box", `style="rounded,dotted"`, `color="` + colWaiting + `"`, "penwidth=2.5", "width=" + wd, "height=" + ht,
		"pos=" + dotq(pt(p.x)+","+pt(p.y)+"!")} {
		if !strings.Contains(line, w) {
			t.Errorf("ring lacks %s: %s", w, line)
		}
	}
	if strings.Contains(line, "fill") || strings.Contains(line, "class") {
		t.Errorf("the ring is filled or animated: %s", line)
	}
	if strings.Index(out, "  "+ids[p.key()]+"w [") < strings.Index(out, "  "+ids[p.key()]+" [") {
		t.Error("the ring is declared before its planet")
	}
}

func TestDotCIBadge(t *testing.T) {
	out, sc, _ := emitRepos(wedgeRoster())
	ids := keyIDs(sc)
	for _, tc := range []struct {
		branch string
		want   []string
	}{
		{"feat/2-pass", []string{`label=""`, `fillcolor="` + colWorking + `"`, "penwidth=0"}},
		{"feat/3-run", []string{`label=""`, `fillcolor="` + colBG + `"`, `color="` + colSun + `"`, "penwidth=2"}},
		{"feat/4-fail", []string{`label="!"`, `fillcolor="` + colBlocked + `"`, "penwidth=0", `fontcolor="` + colBG + `"`, "fontsize=9"}},
		{"feat/5-none", []string{`label=""`, `fillcolor="` + colBG + `"`, `color="` + colDim + `"`, "penwidth=1", `style="filled,dashed"`}},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			p := placedBy(t, sc, "alpha", tc.branch)
			line := nodeLine(t, out, ids[p.key()]+"c")
			for _, w := range append(tc.want, "width=0.2", "height=0.2") {
				if !strings.Contains(line, w) {
					t.Errorf("badge lacks %s: %s", w, line)
				}
			}
			x, y := posOf(t, line)
			dx, dy := cardW*72/2, cardH*72/2
			if math.Abs(x-(p.x+dx)) > 0.011 || math.Abs(y-(p.y+dy)) > 0.011 {
				t.Errorf("badge at (%v, %v), want the card's upper right corner (%v, %v)", x, y, p.x+dx, p.y+dy)
			}
			if strings.Index(out, "  "+ids[p.key()]+"c [") < strings.Index(out, "  "+ids[p.key()]+" [") {
				t.Error("the badge is declared before its planet")
			}
		})
	}
	if n := len(regexp.MustCompile(`(?m)^  p\d+c \[`).FindAllString(out, -1)); n != 4 {
		t.Errorf("%d badges, want one per CI state and none for ci %q", n, "")
	}

	ws := wedgeRoster()
	ws[1].ci = "bogus"
	bogus, _, _ := emitRepos(ws)
	if n := len(regexp.MustCompile(`(?m)^  p\d+c \[`).FindAllString(bogus, -1)); n != 3 {
		t.Errorf("an unknown CI state drew a badge: %d badges, want 3", n)
	}
}

func TestDotStalePlanet(t *testing.T) {
	out, sc, _ := emitRepos(wedgeRoster())
	ids := keyIDs(sc)
	old := placedBy(t, sc, "alpha", "feat/6-old")
	if !old.stale {
		t.Fatal("the roster's old working planet is not stale")
	}
	line := nodeLine(t, out, ids[old.key()])
	for _, want := range []string{`color="` + colWorking + staleAlpha + `"`, `style="rounded,filled,dashed"`, `fontcolor="` + colDim + `"`} {
		if !strings.Contains(line, want) {
			t.Errorf("stale planet lacks %s: %s", want, line)
		}
	}
	live := nodeLine(t, out, ids[placedBy(t, sc, "beta", "feat/1-shared").key()])
	if strings.Contains(live, staleAlpha+`"`) || strings.Contains(live, "dashed") {
		t.Errorf("a live planet is drawn dim: %s", live)
	}
	flow, pulse, _ := animated(out)
	if slices.Contains(flow, ids[old.key()]) || slices.Contains(pulse, ids[old.key()]) {
		t.Errorf("the stale planet is animated: flow %v pulse %v", flow, pulse)
	}
	if len(flow) == 0 {
		t.Error("no live planet flows, so the check above proves nothing")
	}
}

func TestAnimatedSetsSkipStalePlanets(t *testing.T) {
	stale := func(branch, class string, ts float64) placed {
		return placed{planet: planet{branch: branch, class: class, stale: true, ts: ts, moons: []moon{{working: true}}}}
	}
	live := func(branch, class string, ts float64) placed {
		return placed{planet: planet{branch: branch, class: class, ts: ts}}
	}
	ps := []placed{stale("s-work", classWorking, 900), stale("s-block", classBlocked, 800), live("w", classWorking, 1), live("b", classBlocked, 1)}
	flow, pulse, glow := animatedSets(ps)
	if len(flow) != 1 || !flow[planetKey{branch: "w"}] || len(pulse) != 1 || !pulse[planetKey{branch: "b"}] {
		t.Errorf("flow %v pulse %v, want only the live planets", flow, pulse)
	}
	if len(glow) != 2 {
		t.Errorf("glow %v: a stale planet's working moons keep their glow", glow)
	}

	// The budget is spent on live planets: stale ones do not take a slot.
	var crowd []placed
	for i := range maxAnimated {
		crowd = append(crowd, live(fmt.Sprintf("w%d", i), classWorking, 1), stale(fmt.Sprintf("s%d", i), classWorking, 100))
	}
	if flow, _, _ := animatedSets(crowd); len(flow) != maxAnimated || flow[planetKey{branch: "s0"}] {
		t.Errorf("flow %v, want the %d live planets", flow, maxAnimated)
	}

	t.Run("the same branch in two repos is two planets", func(t *testing.T) {
		a := placed{planet: planet{repo: "a", branch: "x", class: classWorking, ts: 1}}
		b := placed{planet: planet{repo: "b", branch: "x", class: classWorking, ts: 1}}
		if flow, _, _ := animatedSets([]placed{a, b}); len(flow) != 2 {
			t.Errorf("flow %v, want both planets", flow)
		}
	})
}

func TestDotMarkerLegend(t *testing.T) {
	rows := func(lg legend) int { return strings.Count(legendHTML(lg, legendFull), "<tr>") }
	base := legend{working: 3, blocked: 1, prOpen: 2, done: 1, holds: 1, repos: 1}
	if got := rows(base); got != 5 {
		t.Errorf("a legend without markers has %d rows, want 5", got)
	}
	if legendHeight(base, legendFull) != legendH+legendRowH || legendHeight(legend{}, legendFull) != legendH+legendRowH {
		t.Errorf("legendHeight = %v, want %v", legendHeight(base, legendFull), legendH+legendRowH)
	}

	extra := func(lg legend) string {
		t.Helper()
		_, row, ok := strings.Cut(legendHTML(lg, legendFull), "stacked on</td></tr>\n    <tr><td>")
		if !ok {
			t.Fatalf("no marker row in %+v", lg)
		}
		row, _, _ = strings.Cut(row, "</td></tr>")
		return row
	}
	const gap = "&nbsp;&nbsp;&nbsp;&nbsp;"
	for _, tc := range []struct {
		name string
		lg   legend
		want []string
		not  []string
	}{
		{"repos", legend{repos: 2}, []string{"wedge = repo"}, []string{"waiting", "ci ", "dim ="}},
		{"waiting", legend{waiting: 2, repos: 1}, []string{`<font color="` + colWaiting + `">●</font> waiting on you 2`}, []string{"wedge", "ci ", "dim =", "ring"}},
		{"ci", legend{ci: 1}, []string{
			`<font color="` + colWorking + `">●</font> ci pass`, `<font color="` + colSun + `">●</font> ci running`,
			`<font color="` + colBlocked + `">●</font> ci fail`, `<font color="` + colDim + `">●</font> no runner`,
		}, []string{"wedge", "waiting", "dim ="}},
		{"stale", legend{stale: 1}, []string{"dim = no status 45m+"}, []string{"wedge", "waiting", "ci "}},
		{"all", legend{repos: 3, waiting: 1, ci: 2, stale: 1}, []string{"wedge = repo" + gap + `<font color="` + colWaiting, "ci pass", "no runner" + gap + "dim = no status"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rows(tc.lg); got != 6 {
				t.Errorf("%d rows, want 6", got)
			}
			row := extra(tc.lg)
			for _, w := range tc.want {
				if !strings.Contains(row, w) {
					t.Errorf("row lacks %q: %s", w, row)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(row, n) {
					t.Errorf("row names %q: %s", n, row)
				}
			}
			if legendHeight(tc.lg, legendFull) != legendH+2*legendRowH {
				t.Errorf("legendHeight = %v, want %v", legendHeight(tc.lg, legendFull), legendH+2*legendRowH)
			}
		})
	}
	if want := fmt.Sprintf("%dm+", int(staleStatus.Minutes())); !strings.Contains(extra(legend{stale: 1}), want) {
		t.Errorf("the stale key does not state the %s threshold", want)
	}
}

// TestDotCanvasPaysTheMarkerRow: the corners widen by half the marker row's
// height, so the picture stays square with the legend's extra row under it.
func TestDotCanvasPaysTheMarkerRow(t *testing.T) {
	out, sc, lg := emitRepos(wedgeRoster())
	if legendHeight(lg, legendFull) <= legendH+legendRowH {
		t.Fatal("the wedges roster has no marker row")
	}
	x, _ := posOf(t, nodeLine(t, out, "c2"))
	if want := sc.extent + canvasPad + legendHeight(lg, legendFull)/2; math.Abs(x-want) > 0.01 {
		t.Errorf("canvas half width = %v, want %v", x, want)
	}
}

func crowd(repo string, n int) []repoWorker {
	var ws []repoWorker
	for i := range n {
		ws = append(ws, repoWorker{dotWorker: dotWorker{branch: fmt.Sprintf("feat/%d-w", i+1), name: "n", tier: "standard", state: "working", age: 10}, repo: repo})
	}
	return ws
}

func TestWedgeLabelsFitTheCorners(t *testing.T) {
	long := []repoWorker(nil)
	long = append(long, crowd(strings.Repeat("a", 24), 4)...)
	long = append(long, crowd(strings.Repeat("b", 24), 4)...)
	for name, ws := range map[string][]repoWorker{"the wedges roster": wedgeRoster(), "24-character names": long} {
		t.Run(name, func(t *testing.T) {
			ps, _ := repoPlanets(ws)
			_, sc := arrange(newSlotState(), ps)
			halfX, halfY := mapHalf(sc)
			for _, w := range sc.wedges {
				if reach := math.Abs(w.x) + labelWidth(w.label)/2; reach > halfX {
					t.Errorf("%q reaches x=%v, past the corner %v", w.label, reach, halfX)
				}
				if reach := math.Abs(w.y) + hostLabelHalf; reach > halfY {
					t.Errorf("%q reaches y=%v, past the corner %v", w.label, reach, halfY)
				}
			}
			if name == "24-character names" {
				if halfX <= sc.extent+canvasPad {
					t.Errorf("the labels do not widen the canvas (%v <= %v)", halfX, sc.extent+canvasPad)
				}
			}
		})
	}
}

func TestMapHalfHoldsAWedgeLabel(t *testing.T) {
	sc := scene{extent: 300, wedges: []wedge{{label: strings.Repeat("r", 24), x: -280, y: 10}}}
	halfX, halfY := mapHalf(sc)
	if want := 280 + (24*hostCharWidth+hostMargin)/2; halfX != want {
		t.Errorf("halfX = %v, want %v", halfX, want)
	}
	if want := 300 + canvasPad; halfY != want {
		t.Errorf("halfY = %v, want %v", halfY, want)
	}
}

func TestDotHostileRepoLabelCannotBreakOut(t *testing.T) {
	ws := wedgeRoster()
	plain, _, _ := emitRepos(ws)
	for i := range ws {
		if ws[i].repo == "alpha" {
			ws[i].repo = hostileRepo
		}
	}
	out, sc, _ := emitRepos(ws)
	for line := range strings.SplitSeq(out, "\n") {
		if !balanced(line) {
			t.Errorf("unbalanced quotes: %s", line)
		}
	}
	if got, want := strings.Count(out, "\n"), strings.Count(plain, "\n"); got != want {
		t.Errorf("a repo name changed the statement count: %d lines, want %d", got, want)
	}
	hostile := placedBy(t, sc, hostileRepo, "feat/2-pass")
	if hostile.repoLabel != hostileRepo {
		t.Fatalf("the label was rewritten to %q", hostile.repoLabel)
	}
	if !strings.Contains(out, "label="+dotq(hostileRepo)) {
		t.Errorf("the wedge label is not the quoted repo name in\n%s", out)
	}
	_, legend, _ := strings.Cut(out, "\n  label=<")
	if strings.Contains(legend, "ZQ") || strings.Count(legend, "<table") != 1 || strings.Count(legend, "</table>") != 1 {
		t.Errorf("legend is not the fixed table: %s", legend)
	}
}

func TestRenderedWedges(t *testing.T) {
	neato := neatoPath(t)
	golden, err := os.ReadFile(filepath.Join("testdata", "wedges.dot"))
	if err != nil {
		t.Fatal(err)
	}
	text := map[string]bool{}
	parseSVG(t, neatoSVG(t, neato, string(golden))).walk(func(n svgNode) {
		if n.XMLName.Local == "text" {
			text[strings.TrimSpace(n.Text)] = true
		}
	})
	for _, label := range []string{"alpha", "beta"} {
		if !text[label] {
			t.Errorf("the rendered golden has no text %q (has %v)", label, text)
		}
	}

	// The map has no legend, so its viewBox is the corners plus the graph pad.
	_, sc, _ := emitRepos(wedgeRoster())
	svg := neatoSVG(t, neato, buildDOT(sc, nil, legendNone, "", ""))
	vbW, vbH, ok := viewBoxSize(svg)
	if !ok {
		t.Fatal("svg has no viewBox")
	}
	halfX, halfY := mapHalf(sc)
	padX, padY := (vbW-2*halfX)/2, (vbH-2*halfY)/2
	const wantPad = 0.2 * 72
	if math.Abs(padX-wantPad) > 0.6 || math.Abs(padY-wantPad) > 0.6 {
		t.Errorf("viewBox %vx%v around corners %vx%v leaves pads %v, %v, want %v: something is drawn outside the corners",
			vbW, vbH, 2*halfX, 2*halfY, padX, padY, wantPad)
	}
}

// TestDotCountsLegendIsOneRow: counts mode keeps only the state counts, and the
// canvas pays for exactly what each mode draws, so the picture stays square.
func TestDotCountsLegendIsOneRow(t *testing.T) {
	budget := 6
	lg := legend{working: 3, blocked: 1, prOpen: 2, done: 1, failed: 1, holds: 1, repos: 3, waiting: 1, ci: 2, stale: 1,
		weight: 4, budget: &budget, engines: []engineReading{{name: "claude"}}}
	html := legendHTML(lg, legendCounts)
	if got := strings.Count(html, "<tr>"); got != 1 {
		t.Errorf("the counts legend has %d rows, want 1:\n%s", got, html)
	}
	for _, want := range []string{"working 3", "blocked 1", "pr open 2", "done 1", "failed 1", "holds 1",
		`<font color="` + colWorking + `">●</font> working 3`, `<font color="` + colHolds + `">●</font> holds 1`} {
		if !strings.Contains(html, want) {
			t.Errorf("the counts legend lacks %q:\n%s", want, html)
		}
	}
	for _, key := range []string{"border weight = tier", "small dot", "hexagon", "stacked on", "queued hold", "wedge", "waiting",
		"ci ", "no runner", "dim =", "crew weight", "fleet budget"} {
		if strings.Contains(html, key) {
			t.Errorf("the counts legend carries the key %q:\n%s", key, html)
		}
	}
	if got := legendHeight(lg, legendCounts); got != legendCountsH || got >= legendHeight(legend{}, legendFull) {
		t.Errorf("counts height = %v, want %v, under the full legend's", got, legendCountsH)
	}
	if got := legendHeight(lg, legendNone); got != 0 {
		t.Errorf("no legend still pays %v", got)
	}

	_, sc := arrange(newSlotState(), []planet{wp("a", classWorking)})
	for _, mode := range []legendMode{legendFull, legendCounts, legendNone} {
		out := renderDOT(sc, lg, mode)
		if want := mode != legendNone; strings.Contains(out, "\n  label=<") != want {
			t.Errorf("%s: graph label drawn = %v, want %v", mode, !want, want)
		}
		if mode == legendCounts && strings.Count(strings.SplitN(out, "\n  label=<", 2)[1], "<tr>") != 1 {
			t.Errorf("the counts diagram has more than one legend row:\n%s", out)
		}
		x, y := posOf(t, nodeLine(t, out, "c2"))
		if wantX, wantY := sc.extent+canvasPad+legendHeight(lg, mode)/2, sc.extent+canvasPad; math.Abs(x-wantX) > 0.01 || math.Abs(y-wantY) > 0.01 {
			t.Errorf("%s: corner at %v,%v, want %v,%v", mode, x, y, wantX, wantY)
		}
	}
}

// TestRenderedLegendModesStaySquare: with the real neato, the legend a mode draws
// fits the height the canvas pre-paid for it, so nothing is clipped and the
// picture is square in every mode.
func TestRenderedLegendModesStaySquare(t *testing.T) {
	neato := neatoPath(t)
	_, sc, lg := emitRepos(wedgeRoster())
	for _, mode := range []legendMode{legendFull, legendCounts, legendNone} {
		svg := neatoSVG(t, neato, renderDOT(sc, lg, mode))
		w, h, ok := viewBoxSize(svg)
		if !ok {
			t.Fatalf("%s: svg has no viewBox", mode)
		}
		if math.Abs(w-h) > 0.05*math.Max(w, h) {
			t.Errorf("%s: viewBox %vx%v is not square", mode, w, h)
		}
	}
}

// graphGroup is the svg's graph group, whose children are everything Graphviz
// drew in front of the background rect.
func graphGroup(t *testing.T, root svgNode) svgNode {
	t.Helper()
	for _, n := range root.Nodes {
		if n.XMLName.Local == "g" && n.attr("class") == "graph" {
			return n
		}
	}
	t.Fatal("svg has no graph group")
	return svgNode{}
}

// svgSun is the dispatcher's centre in the graph group's coordinates, where
// layout (x, y) lands at (cx+x, cy-y).
func svgSun(t *testing.T, root svgNode) (cx, cy float64) {
	t.Helper()
	found := false
	root.walk(func(n svgNode) {
		if n.XMLName.Local != "g" || n.title() != "dispatcher" {
			return
		}
		e, _ := n.child("ellipse")
		cx, _ = strconv.ParseFloat(e.attr("cx"), 64)
		cy, _ = strconv.ParseFloat(e.attr("cy"), 64)
		found = true
	})
	if !found {
		t.Fatal("svg has no dispatcher")
	}
	return cx, cy
}

// spokeGroups maps a planet's node id to the group of its dispatcher spoke.
func spokeGroups(root svgNode) map[string]svgNode {
	spokes := map[string]svgNode{}
	root.walk(func(n svgNode) {
		if id, ok := strings.CutPrefix(n.title(), "dispatcher->"); ok && n.XMLName.Local == "g" {
			spokes[id] = n
		}
	})
	return spokes
}

// TestFlowSpokesRestDashed: the viewer rasterizes the un-animated svg while it
// pans, so the working planets' spokes carry a dotted dash (0.01 12 with round
// caps) themselves, and a spoke that is not animated stays solid.
func TestFlowSpokesRestDashed(t *testing.T) {
	out, sc, _ := emit(dotModel(sixteenWorkers(), 0))
	ids := idOf(sc)
	working := map[string]bool{}
	for _, p := range sc.planets {
		working[ids[p.branch]] = p.class == classWorking
	}
	flows := 0
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "dispatcher" || f[1] != "->" {
			continue
		}
		flow := strings.Contains(line, `class="aeye-flow"`)
		if flow {
			flows++
		}
		if strings.Contains(line, "style=") {
			t.Errorf("a spoke carries a Graphviz style, which finishSVG's dash replaces: %s", line)
		}
		if flow != working[f[2]] {
			t.Errorf("spoke of %s animated = %v, working = %v", f[2], flow, working[f[2]])
		}
	}
	if flows == 0 {
		t.Fatal("no spoke flows, so the check above proves nothing")
	}

	neato := neatoPath(t)
	root := parseSVG(t, finishSVG(neatoSVG(t, neato, out), sc.wedges))
	dashed, solid := 0, 0
	for id, g := range spokeGroups(root) {
		path, ok := g.child("path")
		if !ok {
			t.Fatalf("spoke %s has no path", id)
		}
		flow := strings.Contains(g.attr("class"), "aeye-flow")
		hasDash := path.attr("stroke-dasharray") == "0.01 12" && path.attr("stroke-linecap") == "round"
		if hasDash != flow {
			t.Errorf("spoke %s: dasharray %q, linecap %q, animated = %v", id, path.attr("stroke-dasharray"), path.attr("stroke-linecap"), flow)
		}
		if !flow && (path.attr("stroke-dasharray") != "" || path.attr("stroke-linecap") != "") {
			t.Errorf("spoke %s is not animated but is dashed or capped", id)
		}
		if hasDash {
			dashed++
		} else {
			solid++
		}
	}
	if dashed != flows || solid == 0 {
		t.Errorf("%d dashed and %d solid spokes in the svg, want %d dashed and some solid", dashed, solid, flows)
	}
}

// TestRenderedSpokesLieBeneathAndEndAtThePlanet: with the real neato, every
// edge is emitted before every node, so no spoke draws over a planet or a label,
// and each spoke stops where its planet's ring ends.
func TestRenderedSpokesLieBeneathAndEndAtThePlanet(t *testing.T) {
	neato := neatoPath(t)
	rosterOf := map[string]func() (string, scene){
		"one repo": func() (string, scene) { out, sc, _ := emit(dotModel(sixteenWorkers(), 2)); return out, sc },
		"wedges":   func() (string, scene) { out, sc, _ := emitRepos(wedgeRoster()); return out, sc },
		"the map": func() (string, scene) {
			_, sc, _ := emitRepos(wedgeRoster())
			return buildDOT(sc, nil, legendNone, "", ""), sc
		},
	}
	for name, build := range rosterOf {
		t.Run(name, func(t *testing.T) {
			dot, sc := build()
			root := parseSVG(t, finishSVG(neatoSVG(t, neato, dot), sc.wedges))
			lastEdge, firstNode := -1, -1
			for i, n := range graphGroup(t, root).Nodes {
				switch {
				case n.XMLName.Local != "g":
				case n.attr("class") == "edge":
					lastEdge = i
				case n.attr("class") == "node" && firstNode < 0:
					firstNode = i
				}
			}
			if lastEdge < 0 || firstNode < 0 || lastEdge > firstNode {
				t.Errorf("the last edge is child %d and the first node child %d of the graph group", lastEdge, firstNode)
			}

			cx, cy := svgSun(t, root)
			spokes := spokeGroups(root)
			ids := keyIDs(sc)
			for _, p := range sc.planets {
				g, ok := spokes[ids[p.key()]]
				if !ok {
					t.Errorf("no spoke to %s", p.branch)
					continue
				}
				path, _ := g.child("path")
				nums := pathNumRe.FindAllString(path.attr("d"), -1)
				if len(nums) < 4 {
					t.Errorf("spoke to %s has path %q", p.branch, path.attr("d"))
					continue
				}
				ex, _ := strconv.ParseFloat(nums[len(nums)-2], 64)
				ey, _ := strconv.ParseFloat(nums[len(nums)-1], 64)
				dx, dy := math.Abs(ex-(cx+p.x)), math.Abs(ey-(cy-p.y))
				hw, hh := cardW*72/2, cardH*72/2
				pen := cardPen(p.tier)
				// The border is drawn over the spoke's end, so the end may lie
				// anywhere in the border's stroke.
				inside := dx <= hw+pen/2+1 && dy <= hh+pen/2+1
				onEdge := dx >= hw-pen/2-1 || dy >= hh-pen/2-1
				if !inside || !onEdge {
					t.Errorf("spoke to %s ends (%.1f, %.1f) from the card's centre, want its border (half extents %.1f x %.1f)", p.branch, dx, dy, hw, hh)
				}
			}
		})
	}
}

// TestFinishSVGHalosFreeText: a text outside a node shape (the legend, a host,
// anchor or wedge label) is outlined in the background colour behind its fill,
// once; a text inside a planet, badge or moon is left plain.
func TestFinishSVGHalosFreeText(t *testing.T) {
	in := `<svg viewBox="0 0 10 10"><text x="1" y="2" fill="#fff">a</text>` +
		`<g id="node1" class="node"><title>h1</title><text xml:space="preserve" x="3">b</text></g>` +
		`<g id="node2" class="node"><title>p1</title><text x="5">p</text></g>` +
		`<g id="node3" class="node"><title>p1c</title><text x="6">!</text></g>` +
		`<g id="node7" class="node"><title>p1h</title><text x="9">h</text></g>` +
		`<g id="node5" class="node"><title>w2</title><text x="7">d</text></g>` +
		`<g id="node6" class="node"><title>a1</title><text x="8">e</text></g>` +
		`<g id="node4" class="node"><title>w1</title><text x="4" stroke="` + colBG + `" paint-order="stroke">c</text></g></svg>`
	got := string(finishSVG([]byte(in), nil))
	halo := ` stroke="` + colBG + `" stroke-width="3" stroke-linejoin="round" paint-order="stroke"`
	for _, want := range []string{`<text x="1" y="2" fill="#fff"` + halo + `>a</text>`, `<text xml:space="preserve" x="3"` + halo + `>b</text>`, `<text x="5">p</text>`, `<text x="6">!</text>`, `<text x="9">h</text>`,
		`<text x="7"` + halo + `>d</text>`, `<text x="8"` + halo + `>e</text>`} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s in\n%s", want, got)
		}
	}
	if n := strings.Count(got, "paint-order="); n != 5 {
		t.Errorf("%d paint-order attributes, want 5 (an outlined text is left as it is):\n%s", n, got)
	}

	neato := neatoPath(t)
	dot, sc, _ := emitRepos(wedgeRoster())
	haloed, plain := 0, 0
	seen := map[string]bool{}
	var walk func(n svgNode, inShape bool)
	walk = func(n svgNode, inShape bool) {
		if strings.HasPrefix(n.attr("id"), "node") {
			inShape = !freeLabelRe.MatchString(n.title())
		}
		if n.XMLName.Local == "text" {
			has := n.attr("stroke") == colBG && n.attr("stroke-width") == "3" && n.attr("stroke-linejoin") == "round" && n.attr("paint-order") == "stroke"
			switch {
			case inShape && has:
				t.Errorf("text %q inside a node shape has a halo", n.Text)
			case !inShape && !has:
				t.Errorf("free text %q has no halo: %+v", n.Text, n.Attrs)
			}
			if has {
				haloed++
			} else {
				plain++
			}
			// Known texts, so the check does not share freeLabelRe with the code.
			switch strings.TrimSpace(n.Text) {
			case "alpha", "beta", "mbp":
				if !has {
					t.Errorf("label %q has no halo", n.Text)
				}
				seen[strings.TrimSpace(n.Text)] = true
			case "sky · #1", "olive · #8", "!":
				if has {
					t.Errorf("node text %q has a halo", n.Text)
				}
				seen[strings.TrimSpace(n.Text)] = true
			}
		}
		for _, c := range n.Nodes {
			walk(c, inShape)
		}
	}
	walk(parseSVG(t, finishSVG(neatoSVG(t, neato, dot), sc.wedges)), false)
	if haloed == 0 || plain == 0 {
		t.Errorf("%d haloed and %d plain texts, want both", haloed, plain)
	}
	for _, want := range []string{"alpha", "beta", "mbp", "sky · #1", "olive · #8", "!"} {
		if !seen[want] {
			t.Errorf("no text %q in the rendered svg", want)
		}
	}
}

// wedgeFills are the graph group's direct <path> children before its first edge
// or node: where injectWedgeFills puts the fills.
func wedgeFills(t *testing.T, root svgNode) []svgNode {
	t.Helper()
	var fills []svgNode
	for _, n := range graphGroup(t, root).Nodes {
		if n.XMLName.Local == "g" && (n.attr("class") == "edge" || n.attr("class") == "node") {
			break
		}
		if n.XMLName.Local == "path" {
			fills = append(fills, n)
		}
	}
	return fills
}

func TestRenderedWedgeFills(t *testing.T) {
	neato := neatoPath(t)
	_, sc, lg := emitRepos(wedgeRoster())
	for name, dot := range map[string]string{
		"full legend":  renderDOT(sc, lg, legendFull),
		"counts":       renderDOT(sc, lg, legendCounts),
		"no legend":    renderDOT(sc, lg, legendNone),
		"the map's":    buildDOT(sc, nil, legendNone, "", ""),
		"selected one": buildDOT(sc, nil, legendNone, "feat/1-shared", ""),
	} {
		t.Run(name, func(t *testing.T) {
			svg := finishSVG(neatoSVG(t, neato, dot), sc.wedges)
			root := parseSVG(t, svg)
			fills := wedgeFills(t, root)
			if len(fills) != len(sc.wedges) {
				t.Fatalf("%d fills before the first edge or node, want one per wedge (%d)", len(fills), len(sc.wedges))
			}
			cx, cy := svgSun(t, root)
			hues := map[string]bool{}
			for i, f := range fills {
				w := sc.wedges[i]
				op, _ := strconv.ParseFloat(f.attr("fill-opacity"), 64)
				if op < 0.08 || op > 0.12 || f.attr("stroke") != "none" || f.attr("fill") != wedgeHues[i%len(wedgeHues)] {
					t.Errorf("fill %d is %+v, want a %s wash of 0.08-0.12 opacity with no stroke", i, f.Attrs, wedgeHues[i%len(wedgeHues)])
				}
				hues[f.attr("fill")] = true

				// M centre L arc-start A rim,rim 0 large sweep arc-end Z
				nums := pathNumRe.FindAllString(f.attr("d"), -1)
				if len(nums) != 11 {
					t.Fatalf("fill %d path %q", i, f.attr("d"))
				}
				v := make([]float64, len(nums))
				for k, s := range nums {
					v[k], _ = strconv.ParseFloat(s, 64)
				}
				if math.Abs(v[0]-cx) > 0.01 || math.Abs(v[1]-cy) > 0.01 {
					t.Errorf("fill %d starts at %v,%v, want the dispatcher %v,%v", i, v[0], v[1], cx, cy)
				}
				for _, end := range []struct{ x, y, deg float64 }{{v[2], v[3], w.start}, {v[9], v[10], w.start + w.span}} {
					if r := math.Hypot(end.x-cx, end.y-cy); math.Abs(r-w.rim) > 0.02 {
						t.Errorf("fill %d reaches %v from the dispatcher, want the rim %v", i, r, w.rim)
					}
					if d := math.Mod(degrees(end.x-cx, cy-end.y)-end.deg+540, 360) - 180; math.Abs(d) > 0.05 {
						t.Errorf("fill %d has an edge %v degrees off its wedge's", i, d)
					}
				}
				if large := w.span > 180; (v[7] == 1) != large || v[8] != 1 {
					t.Errorf("fill %d arc flags %v %v for a span of %v", i, v[7], v[8], w.span)
				}
			}
			if len(sc.wedges) <= len(wedgeHues) && len(hues) != len(sc.wedges) {
				t.Errorf("%d wedges share %d hues", len(sc.wedges), len(hues))
			}

			// Each planet sits inside its repo's drawn sector, so the fill is in the planets' coordinates.
			planets := map[string]svgNode{}
			root.walk(func(n svgNode) {
				if n.XMLName.Local == "g" && n.attr("class") == "node" {
					planets[n.title()] = n
				}
			})
			ids := keyIDs(sc)
			for _, p := range sc.planets {
				e, ok := planets[ids[p.key()]].child("ellipse")
				if !ok {
					e, _ = planets[ids[p.key()]].child("polygon")
				}
				if e.XMLName.Local != "ellipse" {
					continue
				}
				x, _ := strconv.ParseFloat(e.attr("cx"), 64)
				y, _ := strconv.ParseFloat(e.attr("cy"), 64)
				at := degrees(x-cx, cy-y)
				for _, w := range sc.wedges {
					if w.repo == p.repo && (at < w.start || at > w.start+w.span) {
						t.Errorf("planet %s of %s is drawn at %v degrees, outside its wedge [%v, %v]", p.branch, w.label, at, w.start, w.start+w.span)
					}
				}
			}
		})
	}

	t.Run("a single repo has none", func(t *testing.T) {
		dot, one, _ := emit(dotModel(eightWorkers(), 0))
		root := parseSVG(t, finishSVG(neatoSVG(t, neato, dot), one.wedges))
		if fills := wedgeFills(t, root); len(fills) != 0 {
			t.Errorf("%d wedge fills for a single repo", len(fills))
		}
		if n := strings.Count(string(finishSVG(neatoSVG(t, neato, dot), one.wedges)), "fill-opacity"); n != 0 {
			t.Errorf("%d fill-opacity attributes in a single-repo svg", n)
		}
	})
}

func TestInjectWedgeFillsLeavesForeignSVGAlone(t *testing.T) {
	w := []wedge{{start: 0, span: 90, rim: 100}}
	for name, in := range map[string]string{
		"no wedges":     `<svg viewBox="0 0 1 1"><g id="graph0"><g id="node1"><title>dispatcher</title><ellipse cx="1" cy="-1"/></g></g></svg>`,
		"no dispatcher": `<svg viewBox="0 0 1 1"><g id="graph0"><g id="node1"><title>p1</title><ellipse cx="1" cy="-1"/></g></g></svg>`,
		"no shapes":     `<svg viewBox="0 0 1 1"><g id="graph0"><title>dispatcher</title></g></svg>`,
	} {
		wedges := w
		if name == "no wedges" {
			wedges = nil
		}
		if got := string(injectWedgeFills([]byte(in), wedges)); got != in {
			t.Errorf("%s: changed to %s", name, got)
		}
	}
}

// TestWedgeLabelBoxClearsTheRim: whatever the bisector's angle, the whole box of
// a wedge's name, not only its centre, stands wedgeLabelGap outside the arc.
func TestWedgeLabelBoxClearsTheRim(t *testing.T) {
	const rim = 320.0
	clearance := func(rim, deg float64, label string) float64 {
		x, y := polar(wedgeLabelRadius(rim, deg, label), deg)
		hw, hh := labelWidth(label)/2, wedgeLabelHalfH
		// The distance from the dispatcher to the nearest point of the box.
		return math.Hypot(max(x-hw, min(0, x+hw)), max(y-hh, min(0, y+hh))) - rim
	}
	for _, label := range []string{"a", "alpha", strings.Repeat("r", 24)} {
		for _, deg := range []float64{0, 20, 45, 70, 90, 110, 135, 160, 180, 225, 270, 300, 340} {
			if got := clearance(rim, deg, label); got < wedgeLabelGap-1e-6 {
				t.Errorf("%q at %v degrees clears the rim by %v, want %v", label, deg, got, float64(wedgeLabelGap))
			}
		}
	}
	// Wide labels at 3 and 9 o'clock stand off by their width, not by one fixed gap.
	long := strings.Repeat("r", 24)
	if side, top := wedgeLabelRadius(rim, 90, long), wedgeLabelRadius(rim, 0, long); side-top < labelWidth(long)/2-wedgeLabelHalfH-1e-6 {
		t.Errorf("a wide name stands %v out at 3 o'clock and %v at 12", side, top)
	}

	ps := append(repoUniform("a", 6), repoUniform("b", 3)...)
	ps = append(ps, repoUniform(strings.Repeat("c", 24), 5)...)
	_, sc := arrange(newSlotState(), ps)
	for _, w := range sc.wedges {
		if got := clearance(w.rim, w.start+w.span/2, w.label); got < wedgeLabelGap-1e-6 {
			t.Errorf("wedge %q: its box clears the rim by %v, want %v", w.label, got, float64(wedgeLabelGap))
		}
		if !near(math.Hypot(w.x, w.y), wedgeLabelRadius(w.rim, w.start+w.span/2, w.label)) {
			t.Errorf("wedge %q is not where wedgeLabelRadius puts it", w.label)
		}
	}
}

func TestDotCardPinsStatePosition(t *testing.T) {
	long := strings.Repeat("W", 24)
	tests := []struct {
		name, state, nick string
		dim, real         bool
	}{
		{"working", "working", "lime", false, true},
		{"blocked", "blocked", "lime", false, true},
		{"pr open", "pr_open", "lime", false, true},
		{"done", "done", "lime", true, true},
		{"reaped", "reaped", "lime", true, true},
		{"failed", "failed", "lime", true, true},
		{"exited", "exited", "lime", true, true},
		{"dispatched", "dispatched", "lime", false, true},
		{"full budget head", "dispatched", long, false, true},
		{"full budget head, short state", "done", long, false, true},
		{"dimmed card", "working", "lime", true, true},
		{"hostile state", long, long, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := cardOf(t, func(r map[string]any) { r["state"], r["name"] = tt.state, tt.nick })
			c := cardTextOf(p)
			w := int(headWidth(c.state))
			label := cardLabel(placed{planet: p}, colWorking, tt.dim)
			want := `<td fixedsize="true" width="` + strconv.Itoa(w) + `" height="` + strconv.Itoa(cardRowH) + `" align="left">`
			if !strings.Contains(label, want) {
				t.Errorf("row 1 lacks %s:\n%s", want, label)
			}
			if n := strings.Count(label, "</td></tr>"); n != cardRows+1 {
				t.Errorf("%d closed rows, want %d (five outer plus the inner head row)", n, cardRows+1)
			}
			if strings.Count(label, `<tr><td align="left">`) != cardRows {
				t.Errorf("want %d outer rows:\n%s", cardRows, label)
			}
			if clamped := float64(w) == math.Ceil(textWidth(ellipsis)); !clamped && textWidth(c.rows[0]) > float64(w) {
				t.Errorf("head width %.1f > cell %d", textWidth(c.rows[0]), w)
			}
			if tt.real {
				if got := float64(w) + textWidth("  "+c.state) + 2*cardCellPad; got > cardInner {
					t.Errorf("head cell + state + padding = %.1f > %.1f", got, cardInner)
				}
			}
		})
	}
}
