package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
)

// TestMain re-executes the test binary as crew-dash itself, so the CLI tests
// below drive the real run() (argv, exit codes, stdout/stderr split) without
// a separate go build.
func TestMain(m *testing.M) {
	if os.Getenv("CREW_DASH_TEST_RUN") == "1" {
		os.Exit(run())
	}
	os.Exit(m.Run())
}

const testdata = "internal/data/testdata/"

// sandbox is a directory of stub sources (dispatch-config, refresh-budget,
// crew) that replay fixtures, wired into a crew-dash environment.
type sandbox struct {
	t   *testing.T
	dir string
	env []string
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(testdata + name)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	dir := t.TempDir()
	s := &sandbox{t: t, dir: dir}
	s.setDispatchConfig("cat " + fixture(t, "settings_layers.json"))
	writeScript(t, filepath.Join(dir, "refresh-budget"), "cat "+fixture(t, "budget_report.json")+"\n")
	s.setCrew(`cat ` + fixture(t, "retro_rows.json"))
	s.env = []string{
		"CREW_DASH_TEST_RUN=1",
		"PATH=" + dir + ":" + os.Getenv("PATH"),
		"CREW_DASH_NOW=1790000000",
		"CREW_BIN=" + filepath.Join(dir, "crew"),
		"DISPATCH_CONFIG_BIN=" + filepath.Join(dir, "dispatch-config"),
	}
	return s
}

// setDispatchConfig installs a dispatch-config stub: --show-origin replays
// the origin fixture, --layers runs layersCmd.
func (s *sandbox) setDispatchConfig(layersCmd string) {
	s.t.Helper()
	writeScript(s.t, filepath.Join(s.dir, "dispatch-config"),
		"case \"$1\" in\n--show-origin) cat "+fixture(s.t, "settings_show_origin.json")+" ;;\n--layers) "+layersCmd+" ;;\nesac\n")
}

// setCrew installs a crew stub: retro --report --json runs retroCmd, rate
// replays the ratings fixture, crews reports no live crew.
func (s *sandbox) setCrew(retroCmd string) {
	s.t.Helper()
	writeScript(s.t, filepath.Join(s.dir, "crew"),
		"case \"$1\" in\nretro) "+retroCmd+" ;;\nrate) cat "+fixture(s.t, "rate_report.json")+" ;;\ncrews) printf 'crew_id\\tlast_event_s\\tfirst_event_s\\tworkers\\tpid\\talive\\n' ;;\nesac\n")
}

func (s *sandbox) run(extraEnv []string, args ...string) (stdout, stderr string, code int) {
	s.t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(slices.Clone(s.env), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatalf("running crew-dash: %v", err)
	}
	return out.String(), errb.String(), code
}

func section(out, from, to string) string {
	i := strings.Index(out, from)
	j := strings.Index(out, to)
	if i < 0 || j < i {
		return ""
	}
	return out[i:j]
}

func TestOnceToPipeCarriesNoColorUnlessForced(t *testing.T) {
	s := newSandbox(t)

	out, _, code := s.run(nil, "--once")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "== Settings ==") || strings.Contains(out, "\x1b[") {
		t.Errorf("piped --once should render plain sections:\n%q", out)
	}

	out, _, code = s.run([]string{"CREW_DASH_COLOR=always"}, "--once")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "\x1b[1;33mprofile: ") {
		t.Errorf("CREW_DASH_COLOR=always left the locked profile row uncolored:\n%q", out)
	}

	out, _, code = s.run([]string{"CREW_DASH_COLOR=always", "NO_COLOR=1"}, "--once")
	if code != 0 || !strings.Contains(out, "== Settings ==") || strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR must beat CREW_DASH_COLOR=always:\n%q", out)
	}
}

func TestJSONShape(t *testing.T) {
	s := newSandbox(t)
	out, _, code := s.run(nil, "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var snap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, out)
	}
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"budget", "now", "roster", "runs", "settings"}; !slices.Equal(keys, want) {
		t.Errorf("top-level keys = %v, want %v", keys, want)
	}

	if string(snap["now"]) != "1790000000" {
		t.Errorf("now = %s, want 1790000000", snap["now"])
	}
	var budget struct {
		Report *struct {
			Engines map[string]json.RawMessage `json:"engines"`
		} `json:"report"`
	}
	if err := json.Unmarshal(snap["budget"], &budget); err != nil {
		t.Fatal(err)
	}
	if budget.Report == nil || len(budget.Report.Engines) == 0 {
		t.Errorf("budget section is empty: %s", snap["budget"])
	}

	var settings struct {
		Layers struct {
			Locked string `json:"locked"`
		} `json:"layers"`
		Rows []struct {
			Path     []string `json:"path"`
			Origin   string   `json:"origin"`
			Editable bool     `json:"editable"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(snap["settings"], &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Layers.Locked != "<LOCKED>" {
		t.Errorf("settings.layers.locked = %q, want <LOCKED>", settings.Layers.Locked)
	}
	byPath := map[string]int{}
	for i, r := range settings.Rows {
		byPath[strings.Join(r.Path, "/")] = i
	}
	profile, ok := byPath["profile"]
	if !ok {
		t.Fatal("no profile row")
	}
	if r := settings.Rows[profile]; r.Origin != "locked" || r.Editable {
		t.Errorf("profile row = %+v, want origin locked, editable false", r)
	}
	engines, ok := byPath["engines"]
	if !ok {
		t.Fatal("no engines row")
	}
	if !settings.Rows[engines].Editable {
		t.Errorf("engines row = %+v, want editable", settings.Rows[engines])
	}
}

func TestBrokenDispatchConfigDegradesOnlySettingsPane(t *testing.T) {
	s := newSandbox(t)
	s.env = append(s.env, "DISPATCH_CONFIG_BIN=false")
	out, _, code := s.run(nil, "--once")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if got := section(out, "== Settings ==", "== Budget =="); !strings.Contains(got, "unavailable: exit 1") {
		t.Errorf("settings pane = %q, want unavailable: exit 1", got)
	}
	if got := section(out, "== Budget ==", "== Runs =="); !strings.Contains(got, "claude (oauth_usage)") {
		t.Errorf("budget pane = %q, want the claude budget intact", got)
	}
}

func TestSourceWarningSurfacesInPaneNeverOnStderr(t *testing.T) {
	s := newSandbox(t)
	s.setDispatchConfig("cat " + fixture(t, "settings_layers.json") +
		"; echo 'warning: dispatch-config: ignoring grantRoots from /x' >&2")
	out, stderr, code := s.run(nil, "--once")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "warning: dispatch-config: ignoring grantRoots from ") {
		t.Errorf("warning missing from the settings pane:\n%s", out)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestRetroNotesRenderAsCrewSection(t *testing.T) {
	s := newSandbox(t)
	out, _, code := s.run(nil, "--once")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"crew c1",
		"summary: khaki: deep/claude/opus done",
		"misrouted: trivial",
		"gate_thrash: circled build",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "session_summary") {
		t.Errorf("session_summary must render as the summary line, not a tag:\n%s", out)
	}
}

func TestRetroReportWithoutRowsRendersNoNotesYet(t *testing.T) {
	s := newSandbox(t)
	s.setCrew(`echo '{"tags":[],"unknown":[]}'`)
	out, _, code := s.run(nil, "--once")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "no retro notes yet") {
		t.Errorf("output missing 'no retro notes yet':\n%s", out)
	}
}

const usageText = "usage: crew dash [--once | --json [--view <name> [--watch] [--keeps]]]\n" +
	"views: overview, roster, budget, runs, settings, map, fleet\n"

func TestUnknownArgumentExits2WithUsage(t *testing.T) {
	s := newSandbox(t)
	out, stderr, code := s.run(nil, "--bogus")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if want := "crew dash: unknown argument \"--bogus\"\n" + usageText; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

func TestUnknownViewExits2NamingViews(t *testing.T) {
	s := newSandbox(t)
	out, stderr, code := s.run(nil, "--json", "--view", "nope")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.HasSuffix(stderr, usageText) || !strings.Contains(stderr, `unknown view "nope"`) {
		t.Errorf("stderr = %q, want the unknown view and the usage naming the views", stderr)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    opts
		wantErr string
	}{
		{args: nil, want: opts{}},
		{args: []string{"--once"}, want: opts{mode: "once"}},
		{args: []string{"--json"}, want: opts{mode: "json"}},
		{args: []string{"--json", "--view", "overview"}, want: opts{mode: "json", view: "overview"}},
		{args: []string{"--view", "roster", "--json"}, want: opts{mode: "json", view: "roster"}},
		{args: []string{"--json", "--view", "overview", "--watch"}, want: opts{mode: "json", view: "overview", watch: true}},
		{args: []string{"--json", "--view", "fleet"}, want: opts{mode: "json", view: "fleet"}},
		{args: []string{"--json", "--view", "fleet", "--watch"}, want: opts{mode: "json", view: "fleet", watch: true}},
		{args: []string{"--view", "overview"}, wantErr: "--view needs --json"},
		{args: []string{"--json", "--watch"}, wantErr: "--watch needs --view"},
		{args: []string{"--json", "--view", "nope"}, wantErr: "overview, roster, budget, runs, settings, map, fleet"},
		{args: []string{"--json", "--view"}, wantErr: "--view needs a name"},
		{args: []string{"--once", "--json"}, wantErr: "--once takes no other flags"},
		{args: []string{"--once", "--watch"}, wantErr: "--once takes no other flags"},
		{args: []string{"--json", "extra"}, wantErr: `unknown argument "extra"`},
		{args: []string{"--json", "--json"}, wantErr: "--json given twice"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseArgs(%q) err = %v, want one containing %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// setGit installs a git stub whose rev-parse names gitDir as the common git
// dir, so the bus log is gitDir/crew/events.jsonl.
func (s *sandbox) setGit(gitDir string) {
	s.t.Helper()
	writeScript(s.t, filepath.Join(s.dir, "git"), "echo "+gitDir+"\n")
}

func TestJSONViewOverview(t *testing.T) {
	s := newSandbox(t)
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	bus, err := os.ReadFile("internal/view/testdata/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "crew", "events.jsonl"), bus, 0o644); err != nil {
		t.Fatal(err)
	}
	s.setGit(gitDir)

	out, stderr, code := s.run(nil, "--json", "--view", "overview")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var v struct {
		SchemaVersion int               `json:"schema_version"`
		View          string            `json:"view"`
		GeneratedAt   int64             `json:"generated_at"`
		Events        []json.RawMessage `json:"events"`
		Errors        []string          `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json --view overview is not JSON: %v\n%s", err, out)
	}
	if v.SchemaVersion != 1 || v.View != "overview" || v.GeneratedAt != 1790000000 {
		t.Errorf("schema_version/view/generated_at = %d/%q/%d", v.SchemaVersion, v.View, v.GeneratedAt)
	}
	if want := bytes.Count(bus, []byte("\n")); len(v.Events) != want {
		t.Errorf("events = %d, want %d (one per bus line)", len(v.Events), want)
	}
	if len(v.Errors) != 0 {
		t.Errorf("errors = %q, want none", v.Errors)
	}
	if !strings.HasPrefix(out, "{\n  \"schema_version\": 1,\n") {
		t.Errorf("output is not indented:\n%s", out)
	}
}

func TestJSONViewBusErrorListedNotFatal(t *testing.T) {
	s := newSandbox(t)
	s.setGit(t.TempDir())

	out, stderr, code := s.run(nil, "--json", "--view", "overview")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var v struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(v.Errors) != 1 || !strings.HasPrefix(v.Errors[0], "bus: ") {
		t.Errorf("errors = %q, want one bus error for the missing events.jsonl", v.Errors)
	}
}

func TestWatchPaths(t *testing.T) {
	if got := watchPaths(nil); len(got) != 0 {
		t.Errorf("watchPaths(nil) = %v, want none", got)
	}
	got := watchPaths([]string{"/r/a/.git", "/r/b/.git"})
	want := []string{
		"/r/a/.git/crew/events.jsonl",
		"/r/a/.git/crew/crews/*/relay/*.cursor",
		"/r/a/.git/crew/peers/*.link",
		"/r/a/.git/crew/peers/*.roster",
		"/r/b/.git/crew/events.jsonl",
		"/r/b/.git/crew/crews/*/relay/*.cursor",
		"/r/b/.git/crew/peers/*.link",
		"/r/b/.git/crew/peers/*.roster",
	}
	if !slices.Equal(got, want) {
		t.Errorf("watchPaths = %v, want %v", got, want)
	}
}

func TestUsageListsFleetView(t *testing.T) {
	if !strings.Contains(usage(), "fleet") {
		t.Errorf("usage lacks fleet: %q", usage())
	}
}

func TestFleetCollector(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(testdata + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	r := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"dispatch-config --show-origin":  {Stdout: read("settings_show_origin.json")},
		"dispatch-config --layers":       {Stdout: read("settings_layers.json")},
		"refresh-budget --report --json": {Stdout: read("budget_report.json")},
		"crew fleet status --json":       {Stdout: read("fleet_status.json")},
		"crew fleet queue --json":        {Stdout: read("fleet_queue.json")},
	}}
	cfg := data.Config{Crew: []string{"crew"}, DispatchConfigBin: "dispatch-config", RefreshBudget: "refresh-budget", Now: 1794800010}
	fc := newFleetCollector(r, cfg)

	in := fc.collect(context.Background(), 1794800010)
	v := view.BuildFleet(in)
	if v.Summary.Crews == 0 {
		t.Fatalf("no crews: %+v", v.Summary)
	}
	active := view.ActiveCommons(in.Fleet)
	scanned := map[string]bool{}
	for c := range in.Digests {
		scanned[c] = true
	}
	for c := range in.DigestErrs {
		scanned[c] = true
	}
	if len(scanned) != len(active) {
		t.Errorf("scanned %v, want exactly the active commons %v", scanned, active)
	}
	for _, c := range active {
		if !scanned[c] {
			t.Errorf("active common %s not scanned", c)
		}
	}
	if known := fc.knownCommons(); len(known) < len(active) {
		t.Errorf("knownCommons = %v, want every repo common (at least %v)", known, active)
	}
	for _, c := range r.CallsSnapshot() {
		if c.Name == "crew" && len(c.Args) > 0 && c.Args[0] == "crews" {
			t.Errorf("fleet collect ran the roster loop: %v", c)
		}
	}
}

func TestFleetCollectorKeepsCommonsAcrossFailedStatus(t *testing.T) {
	status, err := os.ReadFile(testdata + "fleet_status.json")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := os.ReadFile(testdata + "fleet_queue.json")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(testdata + "settings_show_origin.json")
	if err != nil {
		t.Fatal(err)
	}
	layers, err := os.ReadFile(testdata + "settings_layers.json")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := os.ReadFile(testdata + "budget_report.json")
	if err != nil {
		t.Fatal(err)
	}
	failing := data.FakeResponse{Err: data.FakeExitError{Code: 1}}
	r := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"dispatch-config --show-origin":  {Stdout: settings},
		"dispatch-config --layers":       {Stdout: layers},
		"refresh-budget --report --json": {Stdout: budget},
		"crew fleet status --json":       failing,
		"crew fleet queue --json":        {Stdout: queue},
	}}
	cfg := data.Config{Crew: []string{"crew"}, DispatchConfigBin: "dispatch-config", RefreshBudget: "refresh-budget", Now: 1794800010}
	fc := newFleetCollector(r, cfg)

	fc.collect(context.Background(), cfg.Now)
	if known := fc.knownCommons(); len(known) != 0 {
		t.Fatalf("commons after a failed first collect = %v, want none", known)
	}

	r.Responses["crew fleet status --json"] = data.FakeResponse{Stdout: status}
	fc.collect(context.Background(), cfg.Now)
	recovered := fc.knownCommons()
	if len(recovered) == 0 {
		t.Fatal("commons not recorded after recovery")
	}

	r.Responses["crew fleet status --json"] = failing
	fc.collect(context.Background(), cfg.Now)
	if got := fc.knownCommons(); !slices.Equal(got, recovered) {
		t.Errorf("commons after a failed collect = %v, want the last known %v", got, recovered)
	}
}

// serialFleetInputs is the pre-streaming composition: settings, then the
// fleet sources alongside budget, then the bus scans.
func serialFleetInputs(ctx context.Context, fc *fleetCollector, nowSec int64) view.FleetInputs {
	cfg := fc.cfg
	cfg.Now = nowSec
	settings := data.CollectSettings(ctx, fc.runner, cfg)
	budget := data.CollectBudget(ctx, fc.runner, cfg)
	sec := data.CollectFleet(ctx, fc.runner, cfg, data.TailscaleEnabled(settings.Rows), settings, fc.prober)
	digests, digestErrs := fc.scanners.Scan(view.ActiveCommons(sec), nowSec*1000)
	return view.FleetInputs{
		Fleet:      sec,
		Digests:    digests,
		DigestErrs: digestErrs,
		Snap:       data.Snapshot{Now: nowSec, Settings: settings, Budget: budget},
		Loaded:     view.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
		NowMS:      nowSec * 1000,
		Crew:       cfg.Crew,
	}
}

func fleetJSON(t *testing.T, in view.FleetInputs) string {
	t.Helper()
	b, err := json.Marshal(view.BuildFleet(in))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fleetFixtures(t *testing.T) map[string]data.FakeResponse {
	t.Helper()
	read := func(name string) data.FakeResponse {
		b, err := os.ReadFile(testdata + name)
		if err != nil {
			t.Fatal(err)
		}
		return data.FakeResponse{Stdout: b}
	}
	return map[string]data.FakeResponse{
		"dispatch-config --show-origin":  read("settings_show_origin.json"),
		"dispatch-config --layers":       read("settings_layers.json"),
		"refresh-budget --report --json": read("budget_report.json"),
		"crew fleet status --json":       read("fleet_status.json"),
		"crew fleet queue --json":        read("fleet_queue.json"),
	}
}

func TestFleetStreamFinalMatchesSerialComposition(t *testing.T) {
	r := &data.FakeRunner{Responses: fleetFixtures(t)}
	cfg := data.Config{Crew: []string{"crew"}, DispatchConfigBin: "dispatch-config", RefreshBudget: "refresh-budget", Now: 1794800010}
	fc := newFleetCollector(r, cfg)
	ref := newFleetCollector(r, cfg)

	for i, failStatus := range []bool{false, true} {
		if failStatus {
			r.Responses["crew fleet status --json"] = data.FakeResponse{Err: data.FakeExitError{Code: 1}}
		}
		got := fleetJSON(t, fc.collect(context.Background(), cfg.Now))
		want := fleetJSON(t, serialFleetInputs(context.Background(), ref, cfg.Now))
		if got != want {
			t.Errorf("run %d (status failing=%v): stream final differs from the serial composition\ngot  %s\nwant %s", i, failStatus, got, want)
		}
	}
	if got, want := fc.knownCommons(), ref.knownCommons(); len(got) == 0 {
		t.Errorf("commons lost after a failed status: %v (reference %v)", got, want)
	}
}

func TestFleetStreamEmitsBeforeSourcesFinish(t *testing.T) {
	const settingsDoc = `{"hosts":{"halo":{"ssh":{"value":"halo.lan","origin":"user"},"peer":{"value":false,"origin":"user"}}}}`
	resp := fleetFixtures(t)
	release := make(chan struct{})
	resp["dispatch-config --show-origin"] = data.FakeResponse{Stdout: []byte(settingsDoc), Wait: release}
	resp["crew fleet queue --json"] = data.FakeResponse{Stdout: resp["crew fleet queue --json"].Stdout, Wait: release}
	resp["ssh -o BatchMode=yes -o ConnectTimeout=3 -o ServerAliveInterval=2 -o ServerAliveCountMax=1 -- halo.lan true"] = data.FakeResponse{}
	r := &data.FakeRunner{Responses: resp}
	cfg := data.Config{Crew: []string{"crew"}, DispatchConfigBin: "dispatch-config", RefreshBudget: "refresh-budget", Now: 1794800010}
	fc := newFleetCollector(r, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var partials []view.FleetInputs
	statusSeen := make(chan struct{})
	var once sync.Once
	var firstCalls int
	final := make(chan view.FleetInputs, 1)
	go func() {
		final <- fc.stream(ctx, cfg.Now, func(in view.FleetInputs) {
			mu.Lock()
			defer mu.Unlock()
			if len(partials) == 0 {
				firstCalls = len(r.CallsSnapshot())
			}
			partials = append(partials, in)
			if in.Fleet.Status != nil {
				once.Do(func() { close(statusSeen) })
			}
		})
	}()

	select {
	case <-statusSeen:
	case <-ctx.Done():
		t.Fatal("no partial with Status arrived while settings and queue were blocked")
	}
	mu.Lock()
	first := partials[0]
	last := partials[len(partials)-1]
	mu.Unlock()
	if firstCalls != 0 {
		t.Errorf("first emit came after %d subprocess calls", firstCalls)
	}
	if want := (view.FleetPending{Status: true, Queue: true, Reach: true, Hosts: true}); first.Pending != want {
		t.Errorf("first emit Pending = %+v, want all true", first.Pending)
	}
	if !last.Pending.Hosts || !last.Pending.Reach || !last.Pending.Queue || last.Pending.Status {
		t.Errorf("status partial Pending = %+v, want only Status cleared", last.Pending)
	}

	close(release)
	got := <-final
	if got.Pending != (view.FleetPending{}) {
		t.Errorf("final Pending = %+v, want none", got.Pending)
	}
	if len(got.Fleet.Hosts) != 1 || len(got.Fleet.Reach) != 1 {
		t.Errorf("final hosts/reach = %+v / %+v, want one each", got.Fleet.Hosts, got.Fleet.Reach)
	}

	before, err := json.Marshal(got.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	var second []view.FleetInputs
	secondFinal := fc.stream(ctx, cfg.Now, func(in view.FleetInputs) { second = append(second, in) })
	if len(second) == 0 {
		t.Fatal("second stream emitted nothing")
	}
	if first := second[0]; first.Pending != (view.FleetPending{}) {
		t.Errorf("second stream's first emit Pending = %+v, want none", first.Pending)
	}
	for i, in := range second {
		if in.Fleet.Status == nil || in.Pending.Status {
			t.Errorf("second stream emit %d wiped the previous snapshot: status %v pending %+v", i, in.Fleet.Status != nil, in.Pending)
		}
	}
	if after, err := json.Marshal(got.Fleet); err != nil || !bytes.Equal(before, after) {
		t.Errorf("second stream mutated an already emitted value (err %v)", err)
	}
	if len(secondFinal.Fleet.Hosts) != 1 || len(secondFinal.Fleet.Reach) != 1 {
		t.Errorf("second final hosts/reach = %+v / %+v, want one each", secondFinal.Fleet.Hosts, secondFinal.Fleet.Reach)
	}
}

func TestResolveCrewMakesRelativeCrewBinAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "crewbin"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREW_BIN", "crewbin")
	got := resolveCrew()
	want, err := filepath.Abs("crewbin")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{want}) {
		t.Errorf("resolveCrew = %v, want [%s]", got, want)
	}
}

func keepsOf(reason string) map[data.WorkerKey]string {
	return map[data.WorkerKey]string{{Crew: "c", Branch: "b"}: reason}
}

// waitFor polls cond until it holds; the cache refreshes in the background.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 400 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestKeepCacheTTLNeverBlocks(t *testing.T) {
	var kc keepCache
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func() map[data.WorkerKey]string {
		calls.Add(1)
		<-release
		return keepsOf("why")
	}
	if got := kc.cached(clock, fetch); got != nil {
		t.Fatalf("cold read = %v, want nil without blocking", got)
	}
	kc.cached(clock, fetch)
	close(release)
	waitFor(t, func() bool { return kc.cached(clock, fetch) != nil })
	if n := calls.Load(); n != 1 {
		t.Fatalf("fetch calls = %d, want 1", n)
	}
	now = now.Add(reapKeepsTTL + time.Second)
	if got := kc.cached(clock, fetch); got[data.WorkerKey{Crew: "c", Branch: "b"}] != "why" {
		t.Fatalf("stale read = %v, want the old value", got)
	}
	waitFor(t, func() bool { return calls.Load() == 2 })
}

func TestKeepCacheFailureRetriesAfterBackoffAndKeepsValue(t *testing.T) {
	var kc keepCache
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	var calls atomic.Int32
	var result atomic.Pointer[map[data.WorkerKey]string]
	fetch := func() map[data.WorkerKey]string {
		calls.Add(1)
		if r := result.Load(); r != nil {
			return *r
		}
		return nil
	}
	settled := func(n int32) func() bool {
		return func() bool {
			kc.mu.Lock()
			defer kc.mu.Unlock()
			return calls.Load() == n && kc.inflight == nil
		}
	}
	kc.cached(clock, fetch)
	waitFor(t, settled(1))
	kc.cached(clock, fetch)
	if calls.Load() != 1 {
		t.Fatalf("retried inside the backoff: %d calls", calls.Load())
	}
	now = now.Add(reapKeepsRetry + time.Second)
	good := keepsOf("why")
	result.Store(&good)
	kc.cached(clock, fetch)
	waitFor(t, settled(2))
	if got := kc.cached(clock, fetch); got[data.WorkerKey{Crew: "c", Branch: "b"}] != "why" {
		t.Fatalf("keeps = %v", got)
	}
	now = now.Add(reapKeepsTTL + time.Second)
	result.Store(nil)
	kc.cached(clock, fetch)
	waitFor(t, settled(3))
	if got := kc.cached(clock, fetch); got[data.WorkerKey{Crew: "c", Branch: "b"}] != "why" {
		t.Fatalf("failed refresh dropped the last good value: %v", got)
	}
}

func TestKeepCacheFetchNowBlocksOnceAndHonoursCtx(t *testing.T) {
	var kc keepCache
	clock := time.Now
	var calls atomic.Int32
	fetch := func() map[data.WorkerKey]string {
		calls.Add(1)
		return keepsOf("why")
	}
	for range 2 {
		if got := kc.fetchNow(context.Background(), clock, fetch); got[data.WorkerKey{Crew: "c", Branch: "b"}] != "why" {
			t.Fatalf("keeps = %v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls.Load())
	}

	var busy keepCache
	release := make(chan struct{})
	defer close(release)
	go busy.fetchNow(context.Background(), clock, func() map[data.WorkerKey]string { <-release; return nil })
	waitFor(t, func() bool { busy.mu.Lock(); defer busy.mu.Unlock(); return busy.inflight != nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := busy.fetchNow(ctx, clock, nil); got != nil {
		t.Fatalf("canceled waiter = %v, want nil", got)
	}
}

func TestFleetCollectorCachesReapKeeps(t *testing.T) {
	r := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew reap --fleet --dry-run --json": {Stdout: []byte(`{"keeps":[{"crew":"c1","branch":"feat/a","reason":"r"}]}`)},
	}}
	fc := newFleetCollector(r, data.Config{Crew: []string{"crew"}})
	if in := fc.collect(context.Background(), 1794800010); in.ReapKeeps != nil {
		t.Fatalf("first collect must not wait for the probe, got %v", in.ReapKeeps)
	}
	waitFor(t, func() bool { return fc.reapKeeps(context.Background()) != nil })
	in := fc.collect(context.Background(), 1794800010)
	if in.ReapKeeps[data.WorkerKey{Crew: "c1", Branch: "feat/a"}] != "r" {
		t.Fatalf("ReapKeeps = %v", in.ReapKeeps)
	}
	n := 0
	for _, c := range r.CallsSnapshot() {
		if strings.Contains(strings.Join(c.Args, " "), "reap") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("reap calls = %d, want 1", n)
	}
}

func TestParseArgsKeeps(t *testing.T) {
	for _, ok := range [][]string{
		{"--json", "--view", "fleet", "--keeps"},
		{"--json", "--view", "overview", "--keeps"},
	} {
		if o, err := parseArgs(ok); err != nil || !o.keeps {
			t.Errorf("%v: %+v, %v", ok, o, err)
		}
	}
	for _, bad := range [][]string{
		{"--keeps"},
		{"--json", "--keeps"},
		{"--json", "--view", "roster", "--keeps"},
		{"--once", "--keeps"},
		{"--json", "--view", "fleet", "--keeps", "--keeps"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
}
