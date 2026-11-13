package rosterrender

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// crewWorker is a fully described worker for the SVG fixtures.
func crewWorker(branch, name, state string, ts float64) planet {
	p := worker(branch, state)
	p.name, p.issue, p.ts = name, issueOf(branch), ts
	p.tier, p.engine, p.model = "standard", "claude", "opus"
	p.title = "Roster diagram: state board that fills the pane"
	p.detail = "editing boardsvg.go"
	return p
}

type boardFixture struct {
	name   string
	ps     []planet
	holds  []queuedHold
	queued []queuedTask
	sel    selection
}

// boardFixtures mirror the acceptance crews: (a) one working worker, (b) six
// workers over every state with a stacked PR, a hold, a queued task, roles,
// rounds, a waiting, a remote and a stale worker, (c) two repos.
func boardFixtures() []boardFixture {
	solo := crewWorker("feat/1558-board", "ember", "working", 100)

	lead := crewWorker("feat/1580-lead", "ash", "working", 90)
	lead.moons = []moon{{role: "reviewer", working: true}, {role: "plan-critic"}}
	lead.rounds = 2
	stale := crewWorker("feat/1581-stale", "cinder", "working", 10)
	stale.stale = true
	wait := crewWorker("feat/1582-wait", "flint", "blocked", 80)
	wait.waiting, wait.host = true, "strata"
	wait.detail = "permission: git push"
	base := crewWorker("feat/1590-base", "slate", "pr_open", 70)
	base.prURL, base.ci = "https://github.com/o/r/pull/1601", "pass"
	stacked := crewWorker("feat/1591-stacked", "quartz", "pr_open", 60)
	stacked.prURL, stacked.ci, stacked.base = "https://github.com/o/r/pull/1602", "pending", base.branch
	done := crewWorker("feat/1570-done", "basalt", "done", 50)

	x1 := inRepo(crewWorker("feat/12-x", "ember", "working", 40), "x")
	x2 := inRepo(crewWorker("feat/13-x", "ash", "pr_open", 30), "x")
	x2.prURL, x2.ci = "https://github.com/o/x/pull/20", "fail"
	y1 := inRepo(crewWorker("feat/7-y", "flint", "blocked", 20), "y")
	y2 := inRepo(crewWorker("feat/8-y", "slate", "working", 10), "y")

	return []boardFixture{
		{name: "a", ps: []planet{solo}},
		{
			name:   "b",
			ps:     []planet{lead, stale, wait, base, stacked, done},
			holds:  []queuedHold{{id: "h1", label: "1600", engine: "claude", window: "5h"}},
			queued: []queuedTask{{id: "q1", label: "1610", repo: "r", after: []string{"1590"}}},
			sel:    selection{branch: base.branch},
		},
		{
			name:   "c",
			ps:     []planet{x1, x2, y1, y2},
			queued: []queuedTask{{id: "q2", label: "9", repo: "y", repoLabel: "y-label"}},
		},
	}
}

func (f boardFixture) render(aspect float64) (string, board, boardGeom) {
	pct1, pct2 := 88.0, 21.0
	lg := legend{done: 3, failed: 1, weight: 4, engines: []engineReading{
		{name: "claude", window: "7d", pct: &pct1}, {name: "codex"}, {name: "pi", window: "5h", pct: &pct2},
	}}
	b, _ := buildBoard(f.ps, f.holds, f.queued, newSlotState(), lg)
	g := layoutBoard(b, aspect)
	return renderBoardSVG(b, g, headerInfo{crew: "crew 1792528057-3929", strip: stripText(lg)}, f.sel), b, g
}

// xnode is a generic SVG element.
type xnode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xnode    `xml:",any"`
	Text    string     `xml:",chardata"`
}

func (n xnode) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (n xnode) num(t *testing.T, name string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(n.attr(name), 64)
	if err != nil {
		t.Fatalf("<%s %s=%q>: %v", n.XMLName.Local, name, n.attr(name), err)
	}
	return v
}

func (n xnode) walk(f func(xnode)) {
	f(n)
	for _, c := range n.Nodes {
		c.walk(f)
	}
}

func decodeSVG(t *testing.T, svg string) xnode {
	t.Helper()
	var root xnode
	if err := xml.Unmarshal([]byte(svg), &root); err != nil {
		t.Fatalf("svg is not well-formed XML: %v\n%s", err, svg)
	}
	return root
}

// cardGroups maps each card group's id to the group.
func cardGroups(root xnode) map[string]xnode {
	out := map[string]xnode{}
	root.walk(func(n xnode) {
		if id := n.attr("id"); n.XMLName.Local == "g" && strings.HasPrefix(id, "card-") {
			out[id] = n
		}
	})
	return out
}

func TestBoardSVGGoldens(t *testing.T) {
	for _, f := range boardFixtures() {
		t.Run(f.name, func(t *testing.T) {
			got, _, _ := f.render(1.6)
			path := filepath.Join("testdata", "board-"+f.name+".svg")
			if *update {
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

func TestBoardSVGWellFormedWithEveryCard(t *testing.T) {
	for _, f := range boardFixtures() {
		for _, aspect := range []float64{0.6, 1.6, 2.4} {
			t.Run(fmt.Sprintf("%s/%g", f.name, aspect), func(t *testing.T) {
				svg, _, g := f.render(aspect)
				root := decodeSVG(t, svg)
				if want := "0 0 " + fmtNum(g.W) + " " + fmtNum(g.H); root.attr("viewBox") != want {
					t.Errorf("viewBox = %q, want %q", root.attr("viewBox"), want)
				}
				groups := cardGroups(root)
				for _, c := range g.cards() {
					if _, ok := groups[fmt.Sprintf("card-%d-%d-%d", c.lane, c.col, c.card)]; !ok {
						t.Errorf("card %d/%d/%d not drawn", c.lane, c.col, c.card)
					}
				}
				if len(groups) != len(g.cards()) {
					t.Errorf("%d card groups for %d cards", len(groups), len(g.cards()))
				}
				if strings.Contains(strings.ToLower(svg), "legend") {
					t.Error("the board draws a legend")
				}
			})
		}
	}
}

func TestBoardSVGCarriesEverySignal(t *testing.T) {
	svg, _, _ := boardFixtures()[1].render(1.6)
	for _, want := range []string{
		`stroke="` + colWaiting + `"`, // waiting ring
		`fill="` + colWorking + `"/>`, // ci pass badge
		`stroke="` + colSun + `"`,     // ci pending badge and the selection ring
		">r2</text>",                  // review rounds
		"<polygon",                    // remote host hexagon
		">strata</text>",              // remote host chip
		">reviewer</text>",            // role chip
		"on #1601",                    // stacked marker
		">after #1601</text>",         // queued task marker
		">claude · 5h</text>",         // quota hold
		">QUEUED 2</text>", ">WORKING 2</text>", ">BLOCKED 1</text>", ">PR OPEN 2</text>", ">DONE 1</text>",
		"+3 done earlier",
		`class="aeye-flow"`, `class="aeye-pulse"`, `class="aeye-glow"`,
		`opacity="0.55"`, // stale and done dimmed
		`font-family="'Source Sans 3'"`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("fixture b lacks %s", want)
		}
	}
}

func TestBoardSVGSelection(t *testing.T) {
	ring := `stroke="` + colSun + `" stroke-width="` + fmtNum(selectPen) + `"`
	f := boardFixtures()[2]
	cases := []struct {
		sel  selection
		want int
	}{
		{selection{}, 0},
		{selection{branch: "feat/12-x"}, 1},
		{selection{branch: "feat/12-x", repo: "x-label"}, 1},
		{selection{branch: "feat/12-x", repo: "y-label"}, 0},
	}
	for _, tc := range cases {
		f.sel = tc.sel
		svg, _, _ := f.render(1.6)
		if got := strings.Count(svg, ring); got != tc.want {
			t.Errorf("select %+v: %d rings, want %d", tc.sel, got, tc.want)
		}
	}
}

func TestBoardSVGAnimationCapped(t *testing.T) {
	var ps []planet
	for i := range 12 {
		w := crewWorker("feat/"+strconv.Itoa(100+i)+"-w", "w", "working", float64(i))
		w.moons = []moon{{role: "reviewer", working: true}}
		ps = append(ps, w, crewWorker("feat/"+strconv.Itoa(200+i)+"-b", "b", "blocked", float64(i)))
	}
	svg, _, _ := boardFixture{ps: ps}.render(1.6)
	for _, hook := range []string{"aeye-flow", "aeye-pulse", "aeye-glow"} {
		if n := strings.Count(svg, `class="`+hook+`"`); n != maxAnimated {
			t.Errorf("%s drawn %d times, want %d", hook, n, maxAnimated)
		}
	}
}

// hostileFixture is every free-text field at its longest, full of markup.
func hostileFixture(n int) boardFixture {
	long := strings.Repeat(`<b>&"'W</b> `, 20)
	var ps []planet
	for i := range n {
		p := crewWorker("feat/"+strconv.Itoa(300+i)+"-h", "WWWWWWWWWWWWWWWWWWWWWWWW", []string{"working", "blocked", "pr_open", "needs_human_review"}[i%4], float64(i))
		p.title, p.detail = long, long
		p.issue = "WWWWWWWWWWWWWWWWWWWWWWWW"
		p.host = "WWWWWWWWWWWWWWWWWWWWWWWW"
		p.tier, p.engine, p.model = "WWWWWWWWWWWW", "WWWWWWWWWWWWWWWWWWWWWWWW", "WWWWWWWWWWWWWWWWWWWWWWWW"
		p.ci, p.rounds = "fail", 12
		p.moons = []moon{{role: "WWWWWWWWWWWW", working: true}, {role: "reviewer"}, {role: "plan-critic"}, {role: "WWWWWWWWWWWW"}}
		ps = append(ps, p)
	}
	return boardFixture{
		ps:     ps,
		holds:  []queuedHold{{id: "h", label: "WWWWWWWWWWWWWWWWWWWWWWWW", engine: "WWWWWWWWWWWWWWWWWWWWWWWW", window: "WWWWWWWWWWWWWWWWWWWWWWWW"}},
		queued: []queuedTask{{id: "q", label: "WWWWWWWWWWWWWWWWWWWWWWWW", repo: "r", after: []string{"300", "301"}}},
	}
}

func TestBoardSVGEscapesHostileText(t *testing.T) {
	svg, _, _ := hostileFixture(3).render(1.6)
	decodeSVG(t, svg)
	if strings.Contains(svg, "<b>") || strings.Contains(svg, `"'W`) {
		t.Error("free text reached the svg unescaped")
	}
	if !strings.Contains(svg, "&lt;b&gt;&amp;&quot;&apos;W") {
		t.Error("escaped free text missing")
	}
}

// TestBoardSVGTextStaysInsideItsCard checks every text of a card against the
// card's box with the same conservative width estimate the emitter cuts by.
func TestBoardSVGTextStaysInsideItsCard(t *testing.T) {
	const slack = 0.5
	for _, n := range []int{1, 6, 12} {
		for _, aspect := range []float64{0.6, 1.6, 2.4} {
			t.Run(fmt.Sprintf("%d/%g", n, aspect), func(t *testing.T) {
				svg, _, _ := hostileFixture(n).render(aspect)
				for id, grp := range cardGroups(decodeSVG(t, svg)) {
					box := grp.Nodes[0]
					bx, by, bw, bh := box.num(t, "x"), box.num(t, "y"), box.num(t, "width"), box.num(t, "height")
					grp.walk(func(e xnode) {
						if e.XMLName.Local != "text" {
							return
						}
						x, y, w := e.num(t, "x"), e.num(t, "y"), textWidthAt(e.Text, e.num(t, "font-size"))
						lo, hi := x, x+w
						switch e.attr("text-anchor") {
						case "end":
							lo, hi = x-w, x
						case "middle":
							lo, hi = x-w/2, x+w/2
						}
						if lo < bx-slack || hi > bx+bw+slack || y < by || y > by+bh {
							t.Errorf("%s: %q spans x %.1f..%.1f at y %.1f, card %.1f..%.1f × %.1f..%.1f",
								id, e.Text, lo, hi, y, bx, bx+bw, by, by+bh)
						}
					})
				}
			})
		}
	}
}

func TestBoardSVGRendersWithResvg(t *testing.T) {
	resvg, err := exec.LookPath("resvg")
	if err != nil {
		t.Skip("resvg not on PATH")
	}
	fonts := os.Getenv("AEYE_D2_FONT_DIR")
	if fonts == "" {
		t.Skip("AEYE_D2_FONT_DIR not set")
	}
	for _, f := range boardFixtures() {
		t.Run(f.name, func(t *testing.T) {
			svg, _, _ := f.render(1.6)
			dir := t.TempDir()
			in, out := filepath.Join(dir, "board.svg"), filepath.Join(dir, "board.png")
			if err := os.WriteFile(in, []byte(svg), 0o644); err != nil {
				t.Fatal(err)
			}
			// The carousel's resvg: one font directory, no system fonts, no generic families.
			cmd := exec.Command(resvg, "--skip-system-fonts", "--use-fonts-dir", fonts, "--font-family", diagramFont,
				"--serif-family", diagramFont, "--sans-serif-family", diagramFont, in, out)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("resvg: %v\n%s", err, stderr.String())
			}
			if s := strings.TrimSpace(stderr.String()); s != "" {
				t.Errorf("resvg warned:\n%s", s)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if b := img.Bounds(); b.Dx() == 0 || b.Dy() == 0 {
				t.Errorf("png is %v", b)
			}
		})
	}
}

// TestBoardSVGIdentityAndTierLinesWhole: at the card widths fixture b produces,
// every card's `codename · #issue` and tier·engine·model text is drawn whole.
func TestBoardSVGIdentityAndTierLinesWhole(t *testing.T) {
	f := boardFixtures()[1]
	for i := range f.ps {
		f.ps[i].issue = "1234"
		f.ps[i].model = "claude-sonnet-5-5"
	}
	for _, aspect := range []float64{0.8, 1.2, 1.5, 1.6, 2.4} {
		svg, b, g := f.render(aspect)
		root := decodeSVG(t, svg)
		var texts []string
		root.walk(func(n xnode) {
			if n.XMLName.Local == "text" {
				texts = append(texts, n.Text)
			}
		})
		for _, l := range b.lanes {
			for _, col := range l.cols {
				for _, c := range col.cards {
					if c.kind != kindPlanet {
						continue
					}
					for _, want := range []string{headOf(c), metaText(c.planet)} {
						if !slices.Contains(texts, want) {
							t.Errorf("aspect %g (canvas %g wide): %q is not drawn whole", aspect, g.W, want)
						}
					}
				}
			}
		}
	}
}

func TestBoardSVGHeaderCarriesTheBudgetStrip(t *testing.T) {
	svg, _, _ := boardFixtures()[1].render(1.6)
	if !strings.Contains(svg, ">claude 7d 88% · pi 5h 21% · crew 4/6</text>") && !strings.Contains(svg, ">claude 7d 88% · pi 5h 21%</text>") {
		t.Errorf("the header lacks the budget strip:\n%s", svg[:600])
	}
}
