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
	"testing"

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

const usageText = "usage: crew dash [--once | --json [--view <name> [--watch]]]\n" +
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
