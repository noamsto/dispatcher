package peer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/remote"
)

func busStatus(crew, from, state string, age time.Duration) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"status","from":%q,"to":"dispatcher:%s","body":{"state":%q}}`, ms(age), crew, from, crew, state)
}

func busNamedDispatch(crew, branch, name, tier, engine string, age time.Duration) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q,"name":%q,"tier":%q,"engine":%q}`, ms(age), crew, branch, name, tier, engine)
}

func TestSnapshotFold(t *testing.T) {
	p := newBus(t)
	writeLines(t, p.Log,
		busNamedDispatch("c1", "feat/b1", "old", "trivial", "pi", 20*time.Minute),
		busNamedDispatch("c1", "feat/b1", "iris", "standard", "claude", 10*time.Minute),
		busNamedDispatch("c1", "feat/b2", "juno", "deep", "codex", 10*time.Minute),
		busNamedDispatch("c2", "feat/b1", "other", "deep", "codex", 10*time.Minute),
		busStatus("c1", "worker:feat/b1#s1-2", "working", 9*time.Minute),
		busStatus("c1", "worker:feat/b1#s1-2", "blocked", 2*time.Minute),
		busStatus("c1", "worker:feat/b2", "working", 9*time.Minute),
		busStatus("c1", "worker:feat/b2", "reaped", time.Minute),
		busStatus("c1", "worker:feat/b3", "working", 25*time.Hour),
		busStatus("c1", "role:feat/b4:critic", "working", time.Minute),
	)
	snap, err := TakeSnapshot(p.Log, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := Snapshot{TS: testNow.UnixMilli(), Rows: []SnapshotRow{
		{CrewID: "c1", Branch: "feat/b1", Name: "iris", State: "blocked", Tier: "standard", Engine: "claude", TS: ms(2 * time.Minute)},
	}}
	if !reflect.DeepEqual(snap, want) {
		t.Errorf("TakeSnapshot =\n%+v\nwant\n%+v", snap, want)
	}
}

func TestSnapshotMissingLog(t *testing.T) {
	snap, err := TakeSnapshot(newBus(t).Dir+"/nope.jsonl", testNow)
	if err != nil || len(snap.Rows) != 0 {
		t.Errorf("TakeSnapshot = %+v, %v; want no rows", snap, err)
	}
}

func TestSnapshotCapsRows(t *testing.T) {
	p := newBus(t)
	var lines []string
	for i := range 150 {
		lines = append(lines, busStatus("c1", fmt.Sprintf("worker:feat/b%03d", i), "working", time.Duration(150-i)*time.Second))
	}
	writeLines(t, p.Log, lines...)
	snap, err := TakeSnapshot(p.Log, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Rows) != MaxSnapshotRows {
		t.Fatalf("%d rows, want %d", len(snap.Rows), MaxSnapshotRows)
	}
	for i, r := range snap.Rows {
		if want := fmt.Sprintf("feat/b%03d", 50+i); r.Branch != want {
			t.Fatalf("row %d = %s, want %s (the newest, by branch)", i, r.Branch, want)
		}
	}
}

func TestAdmitSnapshot(t *testing.T) {
	valid := SnapshotRow{CrewID: "c1", Branch: "feat/b1", Name: "iris", State: "working", Tier: "standard", Engine: "claude", TS: ms(time.Minute)}
	with := func(f func(*SnapshotRow)) SnapshotRow {
		r := valid
		f(&r)
		return r
	}
	for _, c := range []struct {
		name   string
		row    SnapshotRow
		reason string
	}{
		{"valid", valid, ""},
		{"every live state", with(func(r *SnapshotRow) { r.State = "pr_open" }), ""},
		{"no name tier engine", with(func(r *SnapshotRow) { r.Name, r.Tier, r.Engine = "", "", "" }), ""},
		{"unknown state", with(func(r *SnapshotRow) { r.State = "pwned" }), "malformed"},
		{"no branch", with(func(r *SnapshotRow) { r.Branch = "" }), "malformed"},
		{"escape in name", with(func(r *SnapshotRow) { r.Name = "i\x1b[31mris" }), "control"},
		{"zero-width in branch", with(func(r *SnapshotRow) { r.Branch = "feat/\u200bb1" }), "control"},
		{"bidi override in engine", with(func(r *SnapshotRow) { r.Engine = "cl\u202eaude" }), "control"},
		{"long branch", with(func(r *SnapshotRow) { r.Branch = strings.Repeat("b", 201) }), "oversize"},
		{"long name", with(func(r *SnapshotRow) { r.Name = strings.Repeat("n", 65) }), "oversize"},
		{"long tier", with(func(r *SnapshotRow) { r.Tier = strings.Repeat("t", 17) }), "oversize"},
		{"long engine", with(func(r *SnapshotRow) { r.Engine = strings.Repeat("e", 33) }), "oversize"},
		{"bad crew id", with(func(r *SnapshotRow) { r.CrewID = "../x" }), "identity"},
		{"stale", with(func(r *SnapshotRow) { r.TS = ms(keepFor + time.Second) }), "stale"},
		{"no ts", with(func(r *SnapshotRow) { r.TS = 0 }), "stale"},
		{"future", with(func(r *SnapshotRow) { r.TS = testNow.Add(maxSkew + time.Second).UnixMilli() }), "malformed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, dropped := AdmitSnapshot(&Snapshot{TS: testNow.UnixMilli(), Rows: []SnapshotRow{c.row}}, testNow)
			if c.reason == "" {
				if !reflect.DeepEqual(rows, []SnapshotRow{c.row}) || len(dropped) != 0 {
					t.Errorf("AdmitSnapshot = %+v, %v; want the row admitted", rows, dropped)
				}
				return
			}
			if len(rows) != 0 || !reflect.DeepEqual(dropped, map[string]int{c.reason: 1}) {
				t.Errorf("AdmitSnapshot = %+v, %v; want dropped as %s", rows, dropped, c.reason)
			}
		})
	}
}

func TestAdmitSnapshotCapsRows(t *testing.T) {
	var raw Snapshot
	for i := range MaxSnapshotRows + 1 {
		raw.Rows = append(raw.Rows, SnapshotRow{CrewID: "c1", Branch: fmt.Sprintf("feat/b%d", i), State: "working", TS: ms(time.Minute)})
	}
	rows, dropped := AdmitSnapshot(&raw, testNow)
	if len(rows) != MaxSnapshotRows || !reflect.DeepEqual(dropped, map[string]int{"oversize": 1}) {
		t.Errorf("%d rows, dropped %v; want %d and one oversize", len(rows), dropped, MaxSnapshotRows)
	}
}

// peerStore is a store holding, per host, a link file and (when rows is not
// nil) a roster file.
func peerStore(t *testing.T, links map[string]string, rosters map[string][]SnapshotRow) Store {
	t.Helper()
	s := StoreFor(newBus(t))
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for host, l := range links {
		if err := os.WriteFile(s.path(host, ".link"), []byte(l+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for host, rows := range rosters {
		if err := s.writeRoster(host, rows, testNow); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func linkAt(ok, err time.Duration, detail string) string {
	at := func(d time.Duration) int64 {
		if d < 0 {
			return 0
		}
		return ms(d)
	}
	return fmt.Sprintf(`{"ok":%d,"err":%d,"detail":%q}`, at(ok), at(err), detail)
}

const never time.Duration = -1

func peerHosts(hosts map[string]remote.Host) func() (map[string]remote.Host, error) {
	return func() (map[string]remote.Host, error) { return hosts, nil }
}

var haloPeer = map[string]remote.Host{"halo": {SSH: "halo", Peer: true}}

var haloRoster = []SnapshotRow{
	{CrewID: "c2", Branch: "feat/h2", State: "blocked", TS: ms(30 * time.Second)},
	{CrewID: "c2", Branch: "feat/h1", Name: "iris", State: "working", Tier: "standard", Engine: "claude", TS: ms(2 * time.Minute)},
}

func TestRosterRowsHealthy(t *testing.T) {
	s := peerStore(t, map[string]string{"halo": linkAt(time.Minute, never, "")}, map[string][]SnapshotRow{"halo": haloRoster})
	rows, err := RosterRows(s, peerHosts(haloPeer), testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"branch": "feat/h1", "name": "iris", "state": "working", "tier": "standard", "engine": "claude", "crew_id": "c2",
			"ts": ms(2 * time.Minute), "age_s": int64(120), "host": "halo", "peer": true, "live": nil},
		{"branch": "feat/h2", "name": nil, "state": "blocked", "tier": nil, "engine": nil, "crew_id": "c2",
			"ts": ms(30 * time.Second), "age_s": int64(30), "host": "halo", "peer": true, "live": nil},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("RosterRows =\n%v\nwant\n%v", rows, want)
	}
}

// unhealthy is haloRoster's rows as RosterRows gives them for a link that is
// not healthy.
func unhealthy(why string) []map[string]any {
	return []map[string]any{
		{"branch": "feat/h1", "name": "iris", "state": nil, "tier": "standard", "engine": "claude", "crew_id": "c2",
			"ts": ms(2 * time.Minute), "age_s": int64(120), "host": "halo", "peer": true, "live": nil, "peer_error": why},
		{"branch": "feat/h2", "name": nil, "state": nil, "tier": nil, "engine": nil, "crew_id": "c2",
			"ts": ms(30 * time.Second), "age_s": int64(30), "host": "halo", "peer": true, "live": nil, "peer_error": why},
	}
}

func TestRosterRowsFailedSync(t *testing.T) {
	s := peerStore(t, map[string]string{"halo": linkAt(2*time.Minute, time.Minute, "ssh: exit 255")}, map[string][]SnapshotRow{"halo": haloRoster})
	rows, err := RosterRows(s, peerHosts(haloPeer), testNow)
	if want := unhealthy("last sync failed: ssh: exit 255"); err != nil || !reflect.DeepEqual(rows, want) {
		t.Errorf("RosterRows = %v, %v; want\n%v", rows, err, want)
	}
}

func TestRosterRowsStale(t *testing.T) {
	s := peerStore(t, map[string]string{"halo": linkAt(6*time.Minute+10*time.Second, never, "")}, map[string][]SnapshotRow{"halo": haloRoster})
	rows, err := RosterRows(s, peerHosts(haloPeer), testNow)
	if want := unhealthy("no sync for 6m"); err != nil || !reflect.DeepEqual(rows, want) {
		t.Errorf("RosterRows = %v, %v; want\n%v", rows, err, want)
	}
}

func TestRosterRowsNoSnapshot(t *testing.T) {
	s := peerStore(t, map[string]string{"halo": linkAt(never, time.Minute, "ssh: exit 255")}, nil)
	rows, err := RosterRows(s, peerHosts(haloPeer), testNow)
	want := []map[string]any{{"branch": nil, "name": nil, "state": nil, "live": nil, "host": "halo", "peer": true, "peer_error": "last sync failed: ssh: exit 255"}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Errorf("RosterRows = %v, %v; want %v", rows, err, want)
	}
}

func TestRosterRowsNeverSynced(t *testing.T) {
	s := peerStore(t, nil, map[string][]SnapshotRow{"halo": haloRoster})
	rows, err := RosterRows(s, peerHosts(haloPeer), testNow)
	if want := unhealthy("no sync yet"); err != nil || !reflect.DeepEqual(rows, want) {
		t.Errorf("RosterRows = %v, %v; want\n%v", rows, err, want)
	}
}

func TestRosterRowsHealthyWithoutWorkers(t *testing.T) {
	s := peerStore(t, map[string]string{"halo": linkAt(time.Minute, never, "")}, map[string][]SnapshotRow{"halo": {}})
	if rows, err := RosterRows(s, peerHosts(haloPeer), testNow); err != nil || len(rows) != 0 {
		t.Errorf("RosterRows = %v, %v; want none", rows, err)
	}
}

func TestRosterRowsSortsHosts(t *testing.T) {
	ok := linkAt(time.Minute, never, "")
	one := []SnapshotRow{{CrewID: "c2", Branch: "feat/x", State: "working", TS: ms(time.Minute)}}
	s := peerStore(t, map[string]string{"zed": ok, "halo": ok}, map[string][]SnapshotRow{"zed": one, "halo": one})
	rows, err := RosterRows(s, peerHosts(map[string]remote.Host{"zed": {Peer: true}, "halo": {Peer: true}}), testNow)
	if err != nil || len(rows) != 2 || rows[0]["host"] != "halo" || rows[1]["host"] != "zed" {
		t.Errorf("RosterRows = %v, %v; want halo then zed", rows, err)
	}
}

func TestRosterRowsSkipsNonPeerHost(t *testing.T) {
	ok := linkAt(time.Minute, never, "")
	s := peerStore(t, map[string]string{"halo": ok, "mini": ok}, map[string][]SnapshotRow{"halo": haloRoster, "mini": haloRoster})
	rows, err := RosterRows(s, peerHosts(map[string]remote.Host{"halo": {SSH: "halo"}}), testNow)
	if err != nil || len(rows) != 0 {
		t.Errorf("RosterRows = %v, %v; want none", rows, err)
	}
}

func TestRosterRowsNoStore(t *testing.T) {
	unused := func() (map[string]remote.Host, error) {
		t.Fatal("settings loaded without peer files")
		return nil, nil
	}
	if rows, err := RosterRows(Store{Dir: filepath.Join(t.TempDir(), "peers")}, unused, testNow); rows != nil || err != nil {
		t.Errorf("missing store: RosterRows = %v, %v; want nil, nil", rows, err)
	}
	s := StoreFor(newBus(t))
	storeRows(t, s, "halo", dispatchRow("feat/x", time.Minute, ""))
	if rows, err := RosterRows(s, unused, testNow); rows != nil || err != nil {
		t.Errorf("rows only: RosterRows = %v, %v; want nil, nil", rows, err)
	}
}

func TestRosterRowsSettings(t *testing.T) {
	s := peerStore(t, map[string]string{"halo": linkAt(time.Minute, never, "")}, map[string][]SnapshotRow{"halo": haloRoster})
	if rows, err := RosterRows(s, nil, testNow); rows != nil || err != nil {
		t.Errorf("nil loader: RosterRows = %v, %v; want nil, nil", rows, err)
	}
	notFound := func() (map[string]remote.Host, error) {
		return nil, fmt.Errorf("dispatch-config: %w", exec.ErrNotFound)
	}
	if rows, err := RosterRows(s, notFound, testNow); rows != nil || err != nil {
		t.Errorf("no dispatch-config: RosterRows = %v, %v; want nil, nil", rows, err)
	}
	bad := errors.New("settings: bad json")
	failing := func() (map[string]remote.Host, error) { return nil, bad }
	if _, err := RosterRows(s, failing, testNow); !errors.Is(err, bad) {
		t.Errorf("RosterRows err = %v, want the settings error", err)
	}
	calls := 0
	counted := func() (map[string]remote.Host, error) { calls++; return haloPeer, nil }
	if _, err := RosterRows(s, counted, testNow); err != nil || calls != 1 {
		t.Errorf("settings loaded %d times, %v; want once", calls, err)
	}
}
