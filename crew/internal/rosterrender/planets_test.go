package rosterrender

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const planetNow = 1_800_000_000.0

func planetRow(branch, state string) map[string]any {
	return map[string]any{
		"branch": branch, "name": "lime", "tier": "standard", "state": state,
		"ts": (planetNow - 60) * 1000,
	}
}

func planetModel(rows ...map[string]any) map[string]any {
	m := emptyModel()
	for _, r := range rows {
		m["rows"] = append(m["rows"].([]any), r)
	}
	return m
}

func onlyPlanet(t *testing.T, rows ...map[string]any) planet {
	t.Helper()
	ps, _ := buildPlanets(planetModel(rows...), planetNow)
	if len(ps) != 1 {
		t.Fatalf("planets = %d, want 1", len(ps))
	}
	return ps[0]
}

func TestPlanetLabelIsNameAndIssue(t *testing.T) {
	for branch, want := range map[string]string{
		"feat/1451-roster": "lime\n1451",
		"fix/x-12-y":       "lime\n12",
		"1451-bare":        "lime\n1451",
		"feat/no-number":   "lime",
		"main":             "lime",
	} {
		if got := onlyPlanet(t, planetRow(branch, "working")).label; got != want {
			t.Errorf("%s label = %q, want %q", branch, got, want)
		}
	}
	row := planetRow("feat/7-x", "working")
	row["name"] = ""
	if got := onlyPlanet(t, row).label; got != "?\n7" {
		t.Errorf("empty name label = %q, want %q", got, "?\n7")
	}
}

func TestPlanetTierSizes(t *testing.T) {
	for tier, want := range map[string]float64{
		"deep": 0.9, "standard": 0.75, "trivial": 0.6, "": 0.75, "huge": 0.75,
	} {
		row := planetRow("feat/1-a", "working")
		row["tier"] = tier
		if got := onlyPlanet(t, row).size; got != want {
			t.Errorf("tier %q size = %v, want %v", tier, got, want)
		}
	}
	row := planetRow("feat/1-a", "working")
	delete(row, "tier")
	if got := onlyPlanet(t, row).size; got != 0.75 {
		t.Errorf("missing tier size = %v, want 0.75", got)
	}
}

var safeText = regexp.MustCompile(`\A[A-Za-z0-9._·-]{1,24}\z`)

func TestPlanetHostileTextIsReduced(t *testing.T) {
	row := planetRow("feat/1-a", "working")
	row["name"] = "a\"b\\c\nd$e`f;g<h>" + strings.Repeat("x", 40)
	row["host"] = "mbp\"; system(\"x\")\n"
	p := onlyPlanet(t, row)
	if !safeText.MatchString(p.name) {
		t.Errorf("name = %q is not reduced", p.name)
	}
	if !safeText.MatchString(p.host) {
		t.Errorf("host = %q is not reduced", p.host)
	}
	if strings.ContainsAny(p.label, "\"\\$`;<>") || strings.Count(p.label, "\n") != 1 {
		t.Errorf("label = %q leaks hostile text", p.label)
	}
	if n := utf8.RuneCountInString(p.name); n > 24 {
		t.Errorf("name is %d runes, want <= 24", n)
	}

	row["host"] = "\"\\\n"
	if got := onlyPlanet(t, row).host; got != "?" {
		t.Errorf("all-hostile host = %q, want ?", got)
	}
	row["name"] = "·middle-dot_1.0"
	if got := onlyPlanet(t, row).name; got != "·middle-dot_1.0" {
		t.Errorf("safe name changed to %q", got)
	}
}

func TestPlanetClassMapping(t *testing.T) {
	for _, tc := range []struct {
		state, class string
		failed       bool
	}{
		{"dispatched", classWorking, false},
		{"working", classWorking, false},
		{"blocked", classBlocked, false},
		{"pr_open", classPROpen, false},
		{"done", classDone, false},
		{"reaped", classDone, false},
		{"failed", classDone, true},
		{"exited", classDone, true},
	} {
		p := onlyPlanet(t, planetRow("feat/1-a", tc.state))
		if p.class != tc.class || p.failed != tc.failed {
			t.Errorf("%s -> class %q failed %v, want %q %v", tc.state, p.class, p.failed, tc.class, tc.failed)
		}
	}
	p := onlyPlanet(t, planetRow("feat/1-a", "mystery"))
	if p.class != classWorking || p.failed {
		t.Errorf("unknown state -> class %q failed %v, want a working planet", p.class, p.failed)
	}
	noBranch := planetRow("", "working")
	ps, lg := buildPlanets(planetModel(noBranch, planetRow("feat/2-b", "")), planetNow)
	if len(ps) != 0 || lg != (legend{}) {
		t.Errorf("rows without a branch or a state drew %d planets, legend %+v", len(ps), lg)
	}
}

func TestPlanetDoneFold(t *testing.T) {
	old := planetRow("feat/1-old", "done")
	old["ts"] = (planetNow - 301) * 1000
	young := planetRow("feat/2-young", "done")
	young["ts"] = (planetNow - 299) * 1000
	oldFailed := planetRow("feat/3-oldfail", "failed")
	oldFailed["ts"] = (planetNow - 3600) * 1000
	youngFailed := planetRow("feat/4-youngfail", "exited")
	oldWorking := planetRow("feat/5-old-working", "working")
	oldWorking["ts"] = (planetNow - 3600) * 1000

	ps, lg := buildPlanets(planetModel(old, young, oldFailed, youngFailed, oldWorking), planetNow)
	var got []string
	for _, p := range ps {
		got = append(got, p.branch)
	}
	if strings.Join(got, " ") != "feat/2-young feat/4-youngfail feat/5-old-working" {
		t.Errorf("drawn = %v", got)
	}
	if lg.done != 2 || lg.failed != 2 || lg.working != 1 {
		t.Errorf("legend = %+v, want done 2 failed 2 working 1 (folded ones counted)", lg)
	}
}

func TestPlanetLegendCountsAndHolds(t *testing.T) {
	m := planetModel(
		planetRow("feat/1-a", "working"), planetRow("feat/2-b", "dispatched"),
		planetRow("feat/3-c", "blocked"), planetRow("feat/4-d", "pr_open"),
		planetRow("feat/5-e", "done"), planetRow("feat/6-f", "failed"),
	)
	m["holds"] = []any{map[string]any{"id": "h1"}, map[string]any{"id": "h2"}}
	_, lg := buildPlanets(m, planetNow)
	want := legend{working: 2, blocked: 1, prOpen: 1, done: 1, failed: 1, holds: 2}
	if lg != want {
		t.Fatalf("legend = %+v, want %+v", lg, want)
	}
}

func TestPlanetTimestampAsJSONNumber(t *testing.T) {
	row := planetRow("feat/1-a", "done")
	row["ts"] = json.Number("1")
	ps, lg := buildPlanets(planetModel(row), planetNow)
	if len(ps) != 0 || lg.done != 1 {
		t.Fatalf("json.Number ts not folded: %d planets, %+v", len(ps), lg)
	}
}

func TestPlanetFields(t *testing.T) {
	row := planetRow("feat/9-stack", "working")
	row["base"] = "feat/8-base"
	row["host"] = "mbp"
	p := onlyPlanet(t, row)
	if p.branch != "feat/9-stack" || p.base != "feat/8-base" || p.host != "mbp" || p.issue != "9" {
		t.Fatalf("planet = %+v", p)
	}
	row["base"] = nil
	if got := onlyPlanet(t, row).base; got != "" {
		t.Errorf("null base = %q", got)
	}
}

func TestPlanetMoons(t *testing.T) {
	m := planetModel(
		planetRow("feat/1-a", "blocked"), planetRow("feat/2-b", "pr_open"), planetRow("feat/3-c", "dispatched"),
	)
	m["roles"] = []any{
		map[string]any{"branch": "feat/1-a", "role": "reviewer", "state": "idle", "engine": "claude"},
		map[string]any{"branch": "feat/1-a", "role": "critic", "state": "working", "engine": "pi"},
		map[string]any{"branch": "feat/2-b", "role": "critic", "state": "working", "engine": "pi"},
		map[string]any{"branch": "feat/3-c", "role": "critic", "state": "", "engine": "?"},
		map[string]any{"branch": "feat/gone", "role": "critic", "state": "working", "engine": "pi"},
	}
	ps, _ := buildPlanets(m, planetNow)
	byBranch := map[string]planet{}
	for _, p := range ps {
		byBranch[p.branch] = p
	}
	a := byBranch["feat/1-a"].moons
	if len(a) != 2 || a[0].role != "critic" || !a[0].working || a[1].role != "reviewer" || a[1].working {
		t.Errorf("moons = %+v, want [critic working, reviewer idle]", a)
	}
	if n := len(byBranch["feat/2-b"].moons); n != 0 {
		t.Errorf("pr_open worker has %d moons, want 0", n)
	}
	if c := byBranch["feat/3-c"].moons; len(c) != 1 || c[0].working {
		t.Errorf("dispatched moons = %+v", c)
	}
}
