package data

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const (
	fleetStatusKey = "crew fleet status --json --max-age 15"
	fleetQueueKey  = "crew fleet queue --json"
	tailscaleKey   = "tailscale status --json"
	sshHaloKey     = "ssh -o BatchMode=yes -o ConnectTimeout=3 -o ServerAliveInterval=2 -o ServerAliveCountMax=1 -- halo.lan true"
)

func fleetRunner(t *testing.T) *FakeRunner {
	t.Helper()
	return &FakeRunner{Responses: map[string]FakeResponse{
		fleetStatusKey: {Stdout: readTestdata(t, "fleet_status.json")},
		fleetQueueKey:  {Stdout: readTestdata(t, "fleet_queue.json")},
		tailscaleKey:   {Stdout: readTestdata(t, "tailscale_status.json")},
		sshHaloKey:     {},
	}}
}

func fleetSettings(tailscale bool) SettingsSection {
	rows := []SettingRow{
		leaf(`"halo.lan"`, "hosts", "halo", "ssh"),
		leaf(`false`, "hosts", "halo", "peer"),
		leaf(`"tp"`, "hosts", "tp", "ssh"),
		leaf(`false`, "hosts", "tp", "peer"),
		leaf(`"mbp.lan"`, "hosts", "mbp", "ssh"),
		leaf(`true`, "hosts", "mbp", "peer"),
		leaf(`"box.lan"`, "hosts", "box", "ssh"),
		leaf(`true`, "hosts", "box", "peer"),
		leaf(`"box-nas"`, "hosts", "box", "tailscale"),
		leaf(`["/srv/x"]`, "hosts", "box", "repos", "x"),
	}
	if tailscale {
		rows = append(rows, leaf(`true`, "dashTailscale"))
	}
	return SettingsSection{Rows: rows}
}

func leaf(value string, path ...string) SettingRow {
	return SettingRow{Path: path, Value: json.RawMessage(value), Origin: "user"}
}

func sshKey(dest string) string {
	return "ssh -o BatchMode=yes -o ConnectTimeout=3 -o ServerAliveInterval=2 -o ServerAliveCountMax=1 -- " + dest + " true"
}

// allSSH adds an ok response for every probe the fleet settings configure.
func allSSH(r *FakeRunner) {
	for _, d := range []string{"halo.lan", "tp", "mbp.lan", "box.lan"} {
		r.Responses[sshKey(d)] = FakeResponse{}
	}
}

func callNames(r *FakeRunner) []string {
	var out []string
	for _, c := range r.CallsSnapshot() {
		out = append(out, c.Name)
	}
	return out
}

func fixedProber(r Runner) *HostProber {
	return NewHostProber(r, func() time.Time { return time.Unix(1794800000, 0) })
}

func TestFleetStatusFixtureDecodes(t *testing.T) {
	r := fleetRunner(t)
	allSSH(r)
	sec := CollectFleet(context.Background(), r, baseConfig(), false, fleetSettings(false), fixedProber(r))
	if sec.StatusError != nil || sec.QueueError != nil {
		t.Fatalf("errors: %v %v", sec.StatusError, sec.QueueError)
	}
	st := sec.Status
	if st == nil || st.Host != "lap" || len(st.Repos) != 2 {
		t.Fatalf("status = %+v", st)
	}
	m := st.Machine
	if m == nil || m.Budget != 8 || *m.Free != 3 || len(m.LocalModels) != 1 || m.LocalModels[0].MaxConcurrent != 2 {
		t.Errorf("machine = %+v", m)
	}
	if !m.Engines["codex"].LimitReached || m.Engines["codex"].HotWindows[0] != "5h" {
		t.Errorf("engines = %+v", m.Engines)
	}
	c1 := st.Repos[0].Crews[0]
	if c1.Dispatcher.State != "live" || *c1.Dispatcher.PID != 4242 || len(c1.Relays) != 2 || len(c1.Holds) != 2 {
		t.Errorf("c1 = %+v", c1)
	}
	if c1.Relays[1].Live || *c1.Relays[0].CursorTS != 1794800005000 {
		t.Errorf("relays = %+v", c1.Relays)
	}
	if got := c1.Workers[4]["host_error"]; got == nil {
		t.Errorf("placeholder lost host_error: %v", c1.Workers[4])
	}
	if _, ok := c1.Workers[0]["age_s"]; !ok {
		t.Errorf("age_s not added: %v", c1.Workers[0])
	}
	p := st.Repos[0].Peers
	if len(p.Links) != 2 || p.Dropped != 1 || len(p.Rows) != 4 || *p.Links[0].Detail != "ssh: Connection refused" {
		t.Errorf("peers = %+v", p)
	}
	if st.Repos[1].Crews[0].Dispatcher.Home != nil {
		t.Errorf("closed home should be nil")
	}
	if len(st.Skipped) != 1 || st.Hosts == nil || *st.Queue.Parked[0].Reason != "oversize" {
		t.Errorf("skipped/hosts/queue = %+v %+v %+v", st.Skipped, st.Hosts, st.Queue)
	}
	if len(sec.Queue) != 3 || sec.Queue[0].State != "due" || sec.Queue[0].Reason != nil || *sec.Queue[1].Reason != "load" {
		t.Errorf("queue = %+v", sec.Queue)
	}
	if _, err := json.Marshal(sec); err != nil {
		t.Fatal(err)
	}
}

func TestCollectFleetStatusRecordsMaxAge(t *testing.T) {
	r := fleetRunner(t)
	allSSH(r)
	CollectFleet(context.Background(), r, baseConfig(), false, fleetSettings(false), fixedProber(r))
	if !slices.ContainsFunc(r.CallsSnapshot(), func(c FakeCall) bool {
		return slices.Equal(c.Args, []string{"fleet", "status", "--json", "--max-age", "15"})
	}) {
		t.Errorf("no fleet status call with --max-age 15: %+v", r.CallsSnapshot())
	}
}

func TestCollectFleetAgeS(t *testing.T) {
	r := fleetRunner(t)
	allSSH(r)
	cfg := baseConfig()
	cfg.Now = 1794800010
	sec := CollectFleet(context.Background(), r, cfg, false, fleetSettings(false), fixedProber(r))
	w := sec.Status.Repos[0].Crews[0].Workers[0]
	if w["age_s"] != int64(20) {
		t.Errorf("age_s = %v, want 20", w["age_s"])
	}
}

func TestCollectFleetDegrades(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		resp   FakeResponse
		status bool
		queue  bool
		tsErr  bool
	}{
		{"status", fleetStatusKey, FakeResponse{Stderr: []byte("boom\nmore"), Err: FakeExitError{Code: 2}}, true, false, false},
		{"status garbage", fleetStatusKey, FakeResponse{Stdout: []byte("not json")}, true, false, false},
		{"queue", fleetQueueKey, FakeResponse{Err: FakeExitError{Code: 2}}, false, true, false},
		{"tailscale", tailscaleKey, FakeResponse{Stderr: []byte("down"), Err: FakeExitError{Code: 1}}, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fleetRunner(t)
			allSSH(r)
			r.Responses[tc.key] = tc.resp
			sec := CollectFleet(context.Background(), r, baseConfig(), true, fleetSettings(true), fixedProber(r))
			if (sec.StatusError != nil) != tc.status || (sec.QueueError != nil) != tc.queue || (sec.TailscaleError != nil) != tc.tsErr {
				t.Fatalf("errors status=%v queue=%v ts=%v", sec.StatusError, sec.QueueError, sec.TailscaleError)
			}
			if tc.status && (sec.Status != nil || len(sec.Queue) != 3) {
				t.Errorf("status failure leaked into other sources: %+v", sec)
			}
			if tc.queue && (sec.Status == nil || sec.Queue == nil || len(sec.Queue) != 0) {
				t.Errorf("queue failure must keep status and an empty queue: %+v", sec.Queue)
			}
			if !tc.tsErr && sec.Reach[0].Source != "tailscale" {
				t.Errorf("reach = %+v", sec.Reach)
			}
		})
	}
	r := fleetRunner(t)
	allSSH(r)
	r.Responses[fleetStatusKey] = FakeResponse{Stderr: []byte("boom\nmore"), Err: FakeExitError{Code: 2}}
	sec := CollectFleet(context.Background(), r, baseConfig(), false, fleetSettings(false), fixedProber(r))
	if *sec.StatusError != "boom" {
		t.Errorf("StatusError = %q", *sec.StatusError)
	}
}

func TestCollectFleetQueueExit1IsFailure(t *testing.T) {
	r := fleetRunner(t)
	allSSH(r)
	r.Responses[fleetQueueKey] = FakeResponse{Stdout: readTestdata(t, "fleet_queue.json"), Err: FakeExitError{Code: 1}}
	sec := CollectFleet(context.Background(), r, baseConfig(), false, fleetSettings(false), fixedProber(r))
	if sec.QueueError == nil || len(sec.Queue) != 0 {
		t.Fatalf("queue = %+v err = %v, want an error for exit 1", sec.Queue, sec.QueueError)
	}
}

func TestFleetScannersIncremental(t *testing.T) {
	root := t.TempDir()
	mk := func(name, src string) string {
		common := filepath.Join(root, name, ".git")
		if err := os.MkdirAll(filepath.Join(common, "crew"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(common, "crew", "events.jsonl"), readTestdata(t, src), 0o600); err != nil {
			t.Fatal(err)
		}
		return common
	}
	a, b := mk("a", "fleet_events_a.jsonl"), mk("b", "fleet_events_b.jsonl")
	idle := filepath.Join(root, "idle", ".git") // never scanned: no file at all
	missing := filepath.Join(root, "missing", ".git")

	fs := NewFleetScanners()
	now := int64(1794800010000)
	d, errs := fs.Scan([]string{a, b}, now)
	if len(errs) != 0 || len(d) != 2 {
		t.Fatalf("digests=%d errs=%v", len(d), errs)
	}
	if d[a].PRs[12] == nil || d[b].PRs[12] == nil {
		t.Fatalf("PR 12 missing: %+v", d)
	}
	if d[a].PRs[12].Checks != "SUCCESS" || d[b].PRs[12].Checks != "PENDING" {
		t.Errorf("PR 12 mixed across repos: a=%+v b=%+v", d[a].PRs[12], d[b].PRs[12])
	}
	before := len(d[a].Recent)

	line := `{"ts":1794800005000,"crew_id":"c1","from":"worker:feat/a#s1","to":"dispatcher:c1","kind":"msg","body":"later"}` + "\n"
	f, err := os.OpenFile(filepath.Join(a, "crew", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	d2, errs := fs.Scan([]string{a, b, missing}, now)
	if len(d2[a].Recent) != before+1 || len(d2[b].Recent) != len(d[b].Recent) {
		t.Errorf("only a should advance: a %d->%d b %d->%d", before, len(d2[a].Recent), len(d[b].Recent), len(d2[b].Recent))
	}
	if _, ok := d2[missing]; ok || errs[missing] == "" {
		t.Errorf("missing events file: digest=%v errs=%v", d2[missing], errs)
	}
	d3, errs := fs.Scan([]string{b}, now)
	if _, ok := d3[idle]; ok || len(d3) != 1 || len(errs) != 0 {
		t.Errorf("only requested commons are scanned: %v %v", d3, errs)
	}
}

const reapKeepsKey = "crew reap --fleet --dry-run --json"

func TestCollectReapKeeps(t *testing.T) {
	cfg := Config{Crew: []string{"crew"}}
	r := &FakeRunner{Responses: map[string]FakeResponse{reapKeepsKey: {
		Stdout: []byte(`{"keeps":[{"repo":"/r/.git","crew":"c1","branch":"feat/a","reason":"unmerged commits"}]}`),
	}}}
	got := CollectReapKeeps(context.Background(), r, cfg)
	if got[WorkerKey{Crew: "c1", Branch: "feat/a"}] != "unmerged commits" || got[WorkerKey{Branch: "feat/a"}] != "unmerged commits" || len(got) != 2 {
		t.Errorf("keeps = %v", got)
	}
}

func TestCollectReapKeepsDecodesPartialFailure(t *testing.T) {
	cfg := Config{Crew: []string{"crew"}}
	r := &FakeRunner{Responses: map[string]FakeResponse{reapKeepsKey: {
		Stdout: []byte(`{"keeps":[{"crew":"c1","branch":"feat/a","reason":"why"}]}`),
		Err:    FakeExitError{Code: 1},
	}}}
	got := CollectReapKeeps(context.Background(), r, cfg)
	if got[WorkerKey{Crew: "c1", Branch: "feat/a"}] != "why" {
		t.Errorf("keeps = %v", got)
	}
}

func TestCollectReapKeepsToleratesFailure(t *testing.T) {
	cfg := Config{Crew: []string{"crew"}}
	for name, resp := range map[string]FakeResponse{
		"exit":    {Err: FakeExitError{Code: 2}},
		"garbage": {Stdout: []byte("not json")},
	} {
		r := &FakeRunner{Responses: map[string]FakeResponse{reapKeepsKey: resp}}
		if got := CollectReapKeeps(context.Background(), r, cfg); got != nil {
			t.Errorf("%s: keeps = %v, want nil", name, got)
		}
	}
	if got := CollectReapKeeps(context.Background(), &FakeRunner{}, cfg); got != nil {
		t.Errorf("no response: keeps = %v, want nil", got)
	}
}

func TestCollectReapKeepsAmbiguousBranchHasNoFallback(t *testing.T) {
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"crew reap --fleet --dry-run --json": {Stdout: []byte(`{"keeps":[{"repo":"a","crew":"c1","branch":"chore/x","reason":"ra"},{"repo":"b","crew":"c2","branch":"chore/x","reason":"rb"},{"repo":"a","crew":"c1","branch":"feat/u","reason":"ru"}]}`)},
	}}
	got := CollectReapKeeps(t.Context(), r, Config{Crew: []string{"crew"}})
	if _, ok := got[WorkerKey{Branch: "chore/x"}]; ok {
		t.Errorf("ambiguous branch got a crew-less entry: %v", got)
	}
	if got[WorkerKey{Branch: "feat/u"}] != "ru" {
		t.Errorf("unique branch lost its fallback: %v", got)
	}
}

func TestCollectFleetStatusStampsSince(t *testing.T) {
	common := filepath.Join(t.TempDir(), ".git")
	crews := filepath.Join(common, "crew", "crews")
	if err := os.MkdirAll(filepath.Join(crews, "c-live"), 0o755); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(crews, "c-live", "pid")
	if err := os.WriteFile(pid, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1794799900, 0)
	if err := os.Chtimes(pid, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	doc, err := json.Marshal(map[string]any{"host": "lap", "repos": []map[string]any{
		{"host": "lap", "repo": "a", "common": common, "crews": []map[string]any{
			{"crew_id": "c-live", "dispatcher": map[string]any{"state": "live"}},
			{"crew_id": "c-nopid", "dispatcher": map[string]any{"state": "live"}},
			{"crew_id": "c-dead", "dispatcher": map[string]any{"state": "dead"}},
		}},
		{"host": "far", "repo": "a", "common": common, "crews": []map[string]any{
			{"crew_id": "c-live", "dispatcher": map[string]any{"state": "live"}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := &FakeRunner{Responses: map[string]FakeResponse{fleetStatusKey: {Stdout: doc}}}
	st, errMsg := CollectFleetStatus(context.Background(), r, baseConfig(), FleetViewMaxAge)
	if errMsg != nil {
		t.Fatal(*errMsg)
	}
	local, far := st.Repos[0].Crews, st.Repos[1].Crews[0]
	if s := local[0].Dispatcher.Since; s == nil || *s != mtime.Unix() {
		t.Errorf("live local Since = %v, want %d", s, mtime.Unix())
	}
	if local[1].Dispatcher.Since != nil {
		t.Errorf("missing pid file Since = %v, want nil", *local[1].Dispatcher.Since)
	}
	if local[2].Dispatcher.Since != nil {
		t.Errorf("dead dispatcher Since = %v, want nil", *local[2].Dispatcher.Since)
	}
	if far.Dispatcher.Since != nil {
		t.Errorf("other-host Since = %v, want nil", *far.Dispatcher.Since)
	}
}
