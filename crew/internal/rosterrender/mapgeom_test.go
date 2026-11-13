package rosterrender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

// needTools ends the test when the real rasterizer is not here: a skip in the
// nix build sandbox, a failure under CREW_DRIFT_REQUIRED.
func needTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("resvg"); err != nil {
		drift.Missing(t, "resvg", err)
	}
	if os.Getenv("AEYE_D2_FONT_DIR") == "" {
		drift.Missing(t, "AEYE_D2_FONT_DIR", errors.New("not set"))
	}
}

// selectRing is how the selection ring is drawn: an unfilled rect in colSun.
const selectRing = `fill="none" stroke="` + colSun + `"`

// drawnCards reads each worker card's rect off the svg, in the map's pixels:
// the viewBox fitted into a pngW x pngH image and centred, as resvg draws it.
func drawnCards(t *testing.T, svg string, b board, pngW, pngH int) map[string][4]float64 {
	t.Helper()
	root := decodeSVG(t, svg)
	vb := strings.Fields(root.attr("viewBox"))
	if len(vb) != 4 || vb[0] != "0" || vb[1] != "0" {
		t.Fatalf("viewBox = %q", root.attr("viewBox"))
	}
	w, errW := strconv.ParseFloat(vb[2], 64)
	h, errH := strconv.ParseFloat(vb[3], 64)
	if errW != nil || errH != nil {
		t.Fatalf("viewBox = %q", root.attr("viewBox"))
	}
	s := min(float64(pngW)/w, float64(pngH)/h)
	ox, oy := (float64(pngW)-w*s)/2, (float64(pngH)-h*s)/2
	out := map[string][4]float64{}
	for id, g := range cardGroups(root) {
		var li, ci, k int
		if _, err := fmt.Sscanf(id, "card-%d-%d-%d", &li, &ci, &k); err != nil {
			t.Fatalf("card id %q: %v", id, err)
		}
		c := b.lanes[li].cols[ci].cards[k]
		if c.kind != kindPlanet {
			continue
		}
		i := slices.IndexFunc(g.Nodes, func(n xnode) bool { return n.XMLName.Local == "rect" })
		if i < 0 {
			t.Fatalf("card %s has no rect", id)
		}
		r := g.Nodes[i]
		out[c.repoLabel+"|"+c.branch] = [4]float64{
			ox + r.num(t, "x")*s, oy + r.num(t, "y")*s, r.num(t, "width") * s, r.num(t, "height") * s,
		}
	}
	return out
}

// TestMapGeometryIsTheDrawnCards: every printed rect is the card the svg draws
// for that worker, after the same fit into the PNG resvg makes, for a wide and
// a tall image; the order is the board's reading order; holds and queued tasks
// are not selectable and so not printed.
func TestMapGeometryIsTheDrawnCards(t *testing.T) {
	for _, f := range boardFixtures() {
		for _, px := range [][2]int{{1200, 800}, {600, 1200}} {
			t.Run(fmt.Sprintf("%s/%dx%d", f.name, px[0], px[1]), func(t *testing.T) {
				svg, b, g := f.render(float64(px[0]) / float64(px[1]))
				got := mapGeometry(b, g, px[0], px[1])
				drawn := drawnCards(t, svg, b, px[0], px[1])
				if len(got) != len(drawn) || len(got) != len(f.ps) {
					t.Fatalf("printed %d, drawn %d worker cards, want %d", len(got), len(drawn), len(f.ps))
				}
				var order []string
				for _, cg := range g.cards() {
					if c := b.lanes[cg.lane].cols[cg.col].cards[cg.card]; c.kind == kindPlanet {
						order = append(order, c.branch)
					}
				}
				for i, p := range got {
					if p.Branch != order[i] {
						t.Errorf("planet %d is %s, want %s in reading order", i, p.Branch, order[i])
					}
					label := ""
					for _, w := range f.ps {
						if w.branch == p.Branch {
							label = w.repoLabel
						}
					}
					r, ok := drawn[label+"|"+p.Branch]
					if !ok {
						t.Fatalf("no drawn card for %s", p.Branch)
					}
					for _, c := range [][2]float64{
						{p.Cx - p.HW, r[0]}, {p.Cy - p.HH, r[1]}, {2 * p.HW, r[2]}, {2 * p.HH, r[3]},
					} {
						if math.Abs(c[0]-c[1]) > 0.02 {
							t.Errorf("%s printed %+v, drawn %v", p.Branch, p, r)
							break
						}
					}
					if c := p.Cx + p.HW; p.Cx-p.HW < 0 || c > float64(px[0]) || p.Cy-p.HH < 0 || p.Cy+p.HH > float64(px[1]) {
						t.Errorf("%s at %+v leaves the %dx%d image", p.Branch, p, px[0], px[1])
					}
					if want := round2(math.Hypot(p.HW, p.HH)); math.Abs(p.R-want) > 0.02 {
						t.Errorf("%s r %v, want %v", p.Branch, p.R, want)
					}
				}
			})
		}
	}
}

// TestMapNamesTheRepoOnlyWithLanes: with one lane Repo and Home stay empty;
// with several each worker names its repo label and the home bus's are Home,
// which the dash needs to narrow --select-repo.
func TestMapNamesTheRepoOnlyWithLanes(t *testing.T) {
	single := inRepo(worker("feat/1-a", "working"), "a")
	single.home = true
	b, _ := buildBoard([]planet{single}, nil, nil, newSlotState(), legend{})
	for _, p := range mapGeometry(b, layoutBoard(b, 1.5), 1200, 800) {
		if p.Repo != "" || p.Home {
			t.Errorf("single lane: %+v", p)
		}
	}

	x, y := inRepo(worker("feat/1-a", "working"), "x"), inRepo(worker("feat/1-a", "blocked"), "y")
	x.home = true
	b, _ = buildBoard([]planet{x, y}, nil, nil, newSlotState(), legend{})
	got := mapGeometry(b, layoutBoard(b, 1.5), 1200, 800)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for _, p := range got {
		if want := p.Repo == x.repoLabel; p.Repo == "" || p.Home != want {
			t.Errorf("two lanes: %+v", p)
		}
	}
}

// TestMapGeometryMatchesTheRealRender: at each printed card's left edge the
// rasterized PNG shows the card's border colour.
func TestMapGeometryMatchesTheRealRender(t *testing.T) {
	needTools(t)
	f := boardFixtures()[1]
	for _, px := range [][2]int{{1200, 800}, {600, 1200}} {
		svg, b, g := f.render(float64(px[0]) / float64(px[1]))
		data, err := Default().Resvg(t.Context(), []byte(svg), px[0], px[1])
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if bd := img.Bounds(); bd.Dx() != px[0] || bd.Dy() != px[1] {
			t.Fatalf("png is %v, want %dx%d", bd, px[0], px[1])
		}
		byBranch := map[string]boardCard{}
		for _, l := range b.lanes {
			for _, c := range l.cols {
				for _, cd := range c.cards {
					byBranch[cd.branch] = cd
				}
			}
		}
		for _, p := range mapGeometry(b, g, px[0], px[1]) {
			c := byBranch[p.Branch]
			if c.stale || c.class == classDone {
				continue
			}
			want := classColour(c.class, c.failed)
			r, gr, bl, _ := img.At(int(math.Round(p.Cx-p.HW)), int(math.Round(p.Cy))).RGBA()
			wr, _ := strconv.ParseUint(want[1:3], 16, 8)
			wg, _ := strconv.ParseUint(want[3:5], 16, 8)
			wb, _ := strconv.ParseUint(want[5:7], 16, 8)
			if d := max(absDiff(r>>8, wr), absDiff(gr>>8, wg), absDiff(bl>>8, wb)); d > 40 {
				t.Errorf("%dx%d: %s's left edge is #%02x%02x%02x, want %s", px[0], px[1], p.Branch, r>>8, gr>>8, bl>>8, want)
			}
		}
	}
}

func absDiff(a uint32, b uint64) uint64 {
	if uint64(a) > b {
		return uint64(a) - b
	}
	return b - uint64(a)
}

func TestMapGeometryScalesWithTheBox(t *testing.T) {
	_, b, g := boardFixtures()[1].render(1.5)
	small, big := mapGeometry(b, g, 600, 400), mapGeometry(b, g, 1200, 800)
	for i := range small {
		if math.Abs(2*small[i].Cx-big[i].Cx) > 0.02 || math.Abs(2*small[i].HW-big[i].HW) > 0.02 {
			t.Errorf("%s: %+v at 600x400, %+v at 1200x800", small[i].Branch, small[i], big[i])
		}
	}
}

func TestResvgRewritesOnlyTheRoot(t *testing.T) {
	in := `<svg width="100pt" height="50pt" viewBox="0.00 0.00 100.00 50.00" xmlns="x"><rect width="3" height="4"/></svg>`
	want := `<svg width="640px" height="480px" viewBox="0.00 0.00 100.00 50.00" xmlns="x"><rect width="3" height="4"/></svg>`
	if got := string(sizeSVG([]byte(in), 640, 480)); got != want {
		t.Errorf("sizeSVG =\n%s\nwant\n%s", got, want)
	}
}

func TestMapOutputShapeAndReadingOrder(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	var stdout bytes.Buffer
	var sizes [][2]int
	var svgs []string
	l.opts.Stdout = &stdout
	l.opts.Probes.Resvg = func(_ context.Context, svg []byte, w, h int) ([]byte, error) {
		svgs = append(svgs, string(svg))
		sizes = append(sizes, [2]int{w, h})
		return []byte("PNG"), nil
	}
	out := filepath.Join(t.TempDir(), "map.png")
	run := func(extra ...string) string {
		t.Helper()
		stdout.Reset()
		req := Request{Crew: l.crew, Map: true, Px: "300x200", Out: out}
		if len(extra) > 0 {
			req.Select = extra[0]
		}
		if code := Run(context.Background(), req, l.paths, l.opts); code != exitOK {
			t.Fatalf("code %d: %s", code, l.opts.Stderr.(*bytes.Buffer))
		}
		return stdout.String()
	}

	first := run()
	var got struct {
		V       int
		W, H    int
		PNG     string
		Planets []map[string]any
	}
	dec := json.NewDecoder(strings.NewReader(first))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("%v in %q", err, first)
	}
	if !strings.HasSuffix(first, "}\n") || strings.Count(first, "\n") != 1 {
		t.Errorf("stdout is not one JSON line: %q", first)
	}
	if got.V != 1 || got.W != 300 || got.H != 200 || got.PNG != out {
		t.Errorf("header = %+v, want v 1, 300x200, %s", got, out)
	}
	if png, err := os.ReadFile(out); err != nil || string(png) != "PNG" {
		t.Errorf("png = %q, %v", png, err)
	}
	if !slices.Equal(sizes[0][:], []int{300, 200}) {
		t.Errorf("resvg sized %v", sizes[0])
	}
	if len(got.Planets) != 4 {
		t.Fatalf("%d planets, want the 4 workers", len(got.Planets))
	}
	for _, p := range got.Planets {
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"branch", "cx", "cy", "hh", "hw", "name", "r"}) {
			t.Errorf("planet keys = %v", keys)
		}
	}
	if strings.Contains(svgs[0], selectRing) {
		t.Error("no --select, but a ring was drawn")
	}

	// A persisted sequence orders each column, read and never rewritten.
	fr, err := buildFrame(context.Background(), l.opts, l.paths, l.cdir(), l.crew, roleRows(l.rolePanes(), l.paths.Dir, l.crew), false)
	if err != nil {
		t.Fatal(err)
	}
	order := slices.Clone(fr.slots.Order)
	slices.Reverse(order)
	slotsPath := filepath.Join(l.cdir(), fileSlots)
	if err := os.MkdirAll(l.cdir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveSlots(slotsPath, slotState{V: slotsVer, Order: order}, os.Stderr); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(slotsPath)
	if err != nil {
		t.Fatal(err)
	}
	second := run("feat/2-b")
	var branches []string
	for _, p := range decodePlanets(t, second) {
		branches = append(branches, p.Branch)
	}
	// The board reads column by column, each column in the persisted order.
	byColumn := slices.Clone(order)
	slices.SortStableFunc(byColumn, func(x, y slotEntry) int {
		return slices.Index(boardClasses[:], x.Class) - slices.Index(boardClasses[:], y.Class)
	})
	var want []string
	for _, e := range byColumn {
		want = append(want, e.Branch)
	}
	if !slices.Equal(branches, want) {
		t.Errorf("planets in order %v, want the persisted order by column %v", branches, want)
	}
	if after, _ := os.ReadFile(slotsPath); !bytes.Equal(before, after) {
		t.Errorf("map mode rewrote the slots file:\n%s\n%s", before, after)
	}
	if !strings.Contains(svgs[1], selectRing) {
		t.Error("a known --select drew no ring")
	}
	run("feat/9-nobody")
	if strings.Contains(svgs[2], selectRing) {
		t.Error("an unknown --select drew a ring")
	}
	if again := run(); again != run() {
		t.Error("two renders of one roster gave different geometry")
	}
}

func decodePlanets(t *testing.T, line string) []mapPlanet {
	t.Helper()
	var got mapOut
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatal(err)
	}
	return got.Planets
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[path] = info.Mode().String() + " " + strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMapWritesNothingButThePNG: a crew with no dir of its own is read, drawn and
// left as it was found, from another working directory, with no lock or record.
func TestMapWritesNothingButThePNG(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	var stdout bytes.Buffer
	l.opts.Stdout = &stdout
	l.opts.Probes.Resvg = func(context.Context, []byte, int, int) ([]byte, error) { return []byte("PNG"), nil }
	l.opts.Probes.AeyePublish = func(context.Context, string, string, bool) error {
		t.Error("map mode published to aeye")
		return nil
	}
	if _, err := os.Stat(l.cdir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the crew dir exists before the run: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, l.paths.Common)
	out := filepath.Join(t.TempDir(), "m.png")
	code := Run(context.Background(), Request{Crew: l.crew, Map: true, Px: "128x128", Out: out}, l.paths, l.opts)
	if code != exitOK {
		t.Fatalf("code %d: %s", code, l.opts.Stderr.(*bytes.Buffer))
	}
	if after := snapshot(t, l.paths.Common); !maps.Equal(before, after) {
		t.Errorf("the bus dir changed:\nbefore %v\nafter  %v", before, after)
	}
	if now, _ := os.Getwd(); now != wd {
		t.Errorf("map mode moved the working directory to %s", now)
	}
	if _, err := os.Stat(l.cdir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("map mode created the crew dir: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("no png: %v", err)
	}
}

// TestMapFailureNamesTheTool: a rasterizer that cannot run fails the map with
// one line naming it, prints nothing and leaves no PNG.
func TestMapFailureNamesTheTool(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	var stdout bytes.Buffer
	l.opts.Stdout = &stdout
	l.opts.Probes.Resvg = func(context.Context, []byte, int, int) ([]byte, error) {
		return nil, errors.New(`resvg: exec: "resvg": executable file not found in $PATH`)
	}
	out := filepath.Join(t.TempDir(), "m.png")
	code := Run(context.Background(), Request{Crew: l.crew, Map: true, Px: "128x128", Out: out}, l.paths, l.opts)
	stderr := l.opts.Stderr.(*bytes.Buffer).String()
	if want := "crew: roster-render: resvg:"; code != exitFailure || !strings.HasPrefix(stderr, want) || strings.Count(stderr, "\n") != 1 {
		t.Errorf("code %d, stderr %q, want exit 1 and one %q line", code, stderr, want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q on a failure", stdout.String())
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed render left a png: %v", err)
	}
}

func TestMapRefusals(t *testing.T) {
	ok := Request{Crew: "c1", Map: true, Px: "640x480", Out: "/tmp/x.png"}
	with := func(f func(*Request)) Request {
		r := ok
		f(&r)
		return r
	}
	for _, tc := range []struct {
		name string
		req  Request
		msg  string
	}{
		{"once", with(func(r *Request) { r.Once = true }), "crew: roster-render: --map and --once are mutually exclusive"},
		{"detach", with(func(r *Request) { r.Detach = true }), "crew: roster-render: --map and --detach are mutually exclusive"},
		{"pane", with(func(r *Request) { r.Pane = "%3" }), "crew: roster-render: --map and --pane are mutually exclusive"},
		{"no-open", with(func(r *Request) { r.NoOpen = true }), "crew: roster-render: --map and --no-open are mutually exclusive"},
		{"interval", with(func(r *Request) { r.Interval = "5" }), "crew: roster-render: --map and --interval are mutually exclusive"},
		{"quiet", with(func(r *Request) { r.Quiet = "0" }), "crew: roster-render: --map and --quiet are mutually exclusive"},
		{"legend", with(func(r *Request) { r.Legend = "none" }), "crew: roster-render: --map and --legend are mutually exclusive"},
		{"no crew", with(func(r *Request) { r.Crew = "" }), errCrewRequired},
		{"bad crew", with(func(r *Request) { r.Crew = "a/b" }), errCrewID},
		{"no px", with(func(r *Request) { r.Px = "" }), "crew: roster-render: --map requires --px"},
		{"no out", with(func(r *Request) { r.Out = "" }), "crew: roster-render: --map requires --out"},
		{"px one side", with(func(r *Request) { r.Px = "640" }), errMapPx},
		{"px upper x", with(func(r *Request) { r.Px = "640X480" }), errMapPx},
		{"px empty height", with(func(r *Request) { r.Px = "640x" }), errMapPx},
		{"px signed", with(func(r *Request) { r.Px = "-64x480" }), errMapPx},
		{"px words", with(func(r *Request) { r.Px = "axb" }), errMapPx},
		{"px too narrow", with(func(r *Request) { r.Px = "63x480" }), errMapPx},
		{"px too short", with(func(r *Request) { r.Px = "640x63" }), errMapPx},
		{"px too wide", with(func(r *Request) { r.Px = "4097x480" }), errMapPx},
		{"px too tall", with(func(r *Request) { r.Px = "640x4097" }), errMapPx},
		{"px three parts", with(func(r *Request) { r.Px = "64x64x64" }), errMapPx},
		{"px without map", Request{Crew: "c1", Px: "640x480"}, errMapOnly},
		{"select without map", Request{Crew: "c1", Select: "feat/x"}, errMapOnly},
		{"out without map", Request{Crew: "c1", Out: "/tmp/x.png"}, errMapOnly},
		{"select-repo without map", Request{Crew: "c1", SelectRepo: "alpha"}, errMapOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, msg, code := check(tc.req); msg != tc.msg || code != exitUsage {
				t.Errorf("check = %q, %d; want %q, %d", msg, code, tc.msg, exitUsage)
			}
		})
	}
	for _, px := range []string{"64x64", "4096x4096", "640x480"} {
		c, msg, code := check(with(func(r *Request) { r.Px = px }))
		if msg != "" || code != exitOK || !c.mapMode {
			t.Errorf("px %s refused: %q", px, msg)
		}
	}
}
