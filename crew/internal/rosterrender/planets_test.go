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
	row["name"] = "dash-dot_1.0"
	if got := onlyPlanet(t, row).name; got != "dash-dot_1.0" {
		t.Errorf("safe name changed to %q", got)
	}
}

func TestPlanetCodenameDropsFoldSuffix(t *testing.T) {
	for _, tc := range []struct{ name, branch, label string }{
		{"orchid·1450", "feat/1450-x", "orchid\n1450"},
		{"fern", "feat/111-x", "fern\n111"},
		{"blush·1278", "feat/no-digits", "blush\n1278"},
		{"sky·ABC-12", "feat/no-digits", "sky\nABC-12"},
		{"sky·feat/odd", "feat/no-digits", "sky"},
		{"sky·", "feat/no-digits", "sky"},
		{"·1450", "feat/1450-x", "?\n1450"},
		{"a\"b$·c;d·9", "feat/no-digits", "ab"},
		{"lime·7·8", "feat/5-x", "lime\n5"},
	} {
		row := planetRow(tc.branch, "working")
		row["name"] = tc.name
		p := onlyPlanet(t, row)
		if p.label != tc.label {
			t.Errorf("name %q branch %q: label = %q, want %q", tc.name, tc.branch, p.label, tc.label)
		}
		if strings.Contains(p.name, "·") {
			t.Errorf("name %q kept the suffix: %q", tc.name, p.name)
		}
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

func TestPlanetWaiting(t *testing.T) {
	for _, tc := range []struct {
		name, state, source, detail string
		want                        bool
	}{
		{"permission", "blocked", "", "permission: Bash(rm -rf x)", true},
		{"acceptance", "blocked", "worker", "acceptance: waive the gate?", true},
		{"plain blocked", "blocked", "", "waiting on CI", false},
		{"no detail", "blocked", "", "", false},
		{"watchdog permission", "blocked", "watchdog", "permission: Bash(ls)", false},
		{"watchdog acceptance", "blocked", "watchdog", "acceptance: x", false},
		{"pr_open permission", "pr_open", "", "permission: Bash(ls)", false},
		{"working permission", "working", "", "permission: Bash(ls)", false},
		{"failed permission", "failed", "", "permission: Bash(ls)", false},
	} {
		row := planetRow("feat/1-a", tc.state)
		row["detail"] = tc.detail
		if tc.source != "" {
			row["source"] = tc.source
		}
		if got := onlyPlanet(t, row).waiting; got != tc.want {
			t.Errorf("%s: waiting = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPlanetStale(t *testing.T) {
	const limit = 45 * 60
	for _, tc := range []struct {
		name, state string
		age         float64 // seconds since the row's last activity
		want        bool
	}{
		{"working at limit", "working", limit, false},
		{"working over", "working", limit + 1, true},
		{"blocked at limit", "blocked", limit, false},
		{"blocked over", "blocked", limit + 1, true},
		{"pr_open never", "pr_open", 86400, false},
		{"done never", "done", 120, false},
		{"failed never", "failed", 120, false},
	} {
		row := planetRow("feat/1-a", tc.state)
		row["ts"] = (planetNow - tc.age) * 1000
		if got := onlyPlanet(t, row).stale; got != tc.want {
			t.Errorf("%s: stale = %v, want %v", tc.name, got, tc.want)
		}
	}

	unknown := planetRow("feat/1-a", "working")
	delete(unknown, "ts")
	if onlyPlanet(t, unknown).stale {
		t.Error("a row without a ts is stale")
	}

	waiting := planetRow("feat/1-a", "blocked")
	waiting["ts"] = (planetNow - 3*limit) * 1000
	waiting["detail"] = "permission: Bash(ls)"
	if p := onlyPlanet(t, waiting); !p.waiting || p.stale {
		t.Errorf("old waiting planet: waiting %v stale %v, want waiting and not stale", p.waiting, p.stale)
	}
}

func TestPlanetPRURLOnlyForPROpen(t *testing.T) {
	const url = "https://github.com/o/r/pull/7"
	for state, want := range map[string]string{
		"pr_open": url, "working": "", "blocked": "", "done": "", "failed": "",
	} {
		row := planetRow("feat/1-a", state)
		row["pr_url"] = url
		if got := onlyPlanet(t, row).prURL; got != want {
			t.Errorf("%s prURL = %q, want %q", state, got, want)
		}
	}
	row := planetRow("feat/1-a", "pr_open")
	row["pr_url"] = 7
	if got := onlyPlanet(t, row).prURL; got != "" {
		t.Errorf("non-string pr_url = %q, want empty", got)
	}
}

func TestPlanetTally(t *testing.T) {
	ps := []planet{
		{repo: "/a", waiting: true, ci: "pass"},
		{repo: "/a", stale: true, ci: "none"},
		{repo: "/b", stale: true},
		{},
	}
	lg := legend{waiting: 9, stale: 9, ci: 9, repos: 9, working: 4}
	tally(ps, &lg)
	if lg.waiting != 1 || lg.stale != 2 || lg.ci != 2 || lg.repos != 2 || lg.working != 4 {
		t.Errorf("legend = %+v, want waiting 1 stale 2 ci 2 repos 2 and working untouched", lg)
	}

	old := planetRow("feat/1-old", "done")
	old["ts"] = (planetNow - 3600) * 1000
	wait := planetRow("feat/2-wait", "blocked")
	wait["detail"] = "acceptance: ok?"
	wedged := planetRow("feat/3-wedged", "working")
	wedged["ts"] = (planetNow - 3600) * 1000
	_, lg = buildPlanets(planetModel(old, wait, wedged), planetNow)
	if lg.waiting != 1 || lg.stale != 1 || lg.ci != 0 || lg.repos != 0 {
		t.Errorf("built legend = %+v, want waiting 1 stale 1 ci 0 repos 0", lg)
	}
}

func TestPlanetCallerFieldsStayZero(t *testing.T) {
	for _, state := range []string{"working", "blocked", "pr_open", "done"} {
		p := onlyPlanet(t, planetRow("feat/1-a", state))
		if p.repo != "" || p.repoLabel != "" || p.ci != "" || p.waiting || p.stale || p.prURL != "" {
			t.Errorf("%s planet carries new fields: %+v", state, p)
		}
	}
}
