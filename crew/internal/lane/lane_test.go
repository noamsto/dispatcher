package lane

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

func laneDir(t *testing.T, holder, tick string) string {
	t.Helper()
	dir := t.TempDir()
	if holder != "" {
		if err := os.MkdirAll(filepath.Join(dir, "stream.lock.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stream.lock.d", "pid"), []byte(holder+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if tick != "" {
		if err := os.WriteFile(filepath.Join(dir, "stream.tick"), []byte(tick), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestState(t *testing.T) {
	const now = 1_000_000_000
	live := proctest.New(map[int]bool{42: true}, nil)
	tick := func(ageS, park int64) string {
		return fmt.Sprintf(`{"pid":42,"ts":%d,"park":%d}`, now-ageS*1000, park)
	}
	for _, tc := range []struct {
		name, holder, tick string
		state              string
		rc                 int
		pid                string
		age                *int64
	}{
		{name: "no lock is dead", state: "dead", rc: 2},
		{name: "dead holder", holder: "43", tick: tick(1, 300), state: "dead", rc: 2, pid: "43"},
		{name: "fresh tick", holder: "42", tick: tick(10, 300), state: "alive", rc: 0, pid: "42", age: ptr(10)},
		{name: "tick past 2×park+60", holder: "42", tick: tick(661, 300), state: "stale", rc: 1, pid: "42", age: ptr(661)},
		{name: "no tick", holder: "42", state: "stale", rc: 1, pid: "42"},
		{name: "torn tick", holder: "42", tick: `{"pid":42,"ts"`, state: "stale", rc: 1, pid: "42"},
		{name: "non-numeric holder", holder: "abc", state: "dead", rc: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, rc, pid, age := State(laneDir(t, tc.holder, tc.tick), live, now)
			if state != tc.state || rc != tc.rc || string(pid) != tc.pid {
				t.Errorf("State = (%q, %d, %q), want (%q, %d, %q)", state, rc, pid, tc.state, tc.rc, tc.pid)
			}
			if (age == nil) != (tc.age == nil) || age != nil && *age != *tc.age {
				t.Errorf("age = %v, want %v", age, tc.age)
			}
		})
	}
}

func ptr(n int64) *int64 { return &n }

func TestReadTick(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       Tick
	}{
		{"numbers", `{"ts":5,"park":300}`, Tick{TS: 5, Park: 300, OK: true}},
		{"digit strings", `{"ts":"5","park":"300"}`, Tick{TS: 5, Park: 300, OK: true}},
		{"negative", `{"ts":-5,"park":300}`, Tick{}},
		{"fraction", `{"ts":5.5,"park":300}`, Tick{}},
		{"missing park", `{"ts":5}`, Tick{}},
		{"not an object", `[1]`, Tick{}},
		{"null", `null`, Tick{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stream.tick")
			if err := os.WriteFile(path, []byte(tc.text), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := ReadTick(path); got != tc.want {
				t.Errorf("ReadTick(%s) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
	if got := ReadTick(filepath.Join(t.TempDir(), "absent")); got != (Tick{}) {
		t.Errorf("ReadTick(absent) = %+v, want no tick", got)
	}
}

func TestBusScratch(t *testing.T) {
	got := BusScratch("/lane", "/home/u/git/x/.git")
	if filepath.Dir(got) != "/lane/buses" || len(filepath.Base(got)) != 16 {
		t.Errorf("BusScratch = %q, want /lane/buses/<16 hex>", got)
	}
	if got != BusScratch("/lane", "/home/u/git/x/.git") || got == BusScratch("/lane", "/home/u/git/y/.git") {
		t.Errorf("BusScratch must be stable per common dir and differ across dirs")
	}
}

func TestOverseerCursor(t *testing.T) {
	common := "/home/u/git/x/.git"
	want := filepath.Join(BusScratch("/state/crew/overseer", common), "cursor")
	if got := OverseerCursor("/state", common); got != want {
		t.Errorf("OverseerCursor = %q, want %q", got, want)
	}
}

func TestOverseerHostDir(t *testing.T) {
	if got, want := OverseerHostDir("/state", "mbp"), "/state/crew/overseer/hosts/mbp"; got != want {
		t.Errorf("OverseerHostDir = %q, want %q", got, want)
	}
	if got, want := OverseerHostInbox("/state", "mbp"), "/state/crew/overseer/hosts/mbp/inbox.json"; got != want {
		t.Errorf("OverseerHostInbox = %q, want %q", got, want)
	}
}

func TestFarInboxRoundTrip(t *testing.T) {
	path := OverseerHostInbox(t.TempDir(), "mbp")
	if _, ok := ReadFarInbox(path); ok {
		t.Fatal("a missing inbox read as present")
	}
	want := FarInbox{Floor: 1700, Seen: []string{"k2", "k1"}}
	if err := WriteFarInbox(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadFarInbox(path)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("ReadFarInbox = %+v, %v, want %+v", got, ok, want)
	}
	if err := os.WriteFile(path, []byte(`{"floor":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadFarInbox(path); ok {
		t.Error("a torn inbox read as present")
	}
}

// A write replaces the inbox whole and leaves no temp file behind; one that
// cannot land leaves the old inbox as it was.
func TestWriteFarInboxIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.json")
	for _, in := range []FarInbox{{Floor: 1, Seen: []string{"a", "b", "c"}}, {Floor: 2, Seen: []string{}}} {
		if err := WriteFarInbox(path, in); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := ReadFarInbox(path); got.Floor != 2 || len(got.Seen) != 0 {
		t.Errorf("inbox = %+v, want the second write whole", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dir holds %v, want inbox.json alone", entries)
	}

	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "inbox.json", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFarInbox(filepath.Join(blocked, "inbox.json"), FarInbox{Floor: 3}); err == nil {
		t.Error("a write over a non-empty dir succeeded")
	}
	if entries, _ := os.ReadDir(blocked); len(entries) != 1 {
		t.Errorf("a failed write left %v, want no temp file", entries)
	}
}

func TestFarInboxNextPrunesBelowTheFloor(t *testing.T) {
	in := FarInbox{Floor: 100, Seen: []string{"old"}}
	delivered := map[string]int64{"old": 150, "mid": 200, "new": 300}
	got := in.Next(180, delivered, 10)
	if want := (FarInbox{Floor: 180, Seen: []string{"new", "mid"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("Next = %+v, want %+v", got, want)
	}
	if got := in.Next(50, map[string]int64{}, 10); got.Floor != 100 || got.Seen == nil || len(got.Seen) != 0 {
		t.Errorf("Next with a lower floor = %+v, want floor 100 and an empty seen", got)
	}
}

func TestFarInboxNextKeepsTheNewestUpToTheCap(t *testing.T) {
	delivered := map[string]int64{"a": 10, "b": 40, "c": 30, "d": 40, "e": 20}
	got := FarInbox{}.Next(0, delivered, 3)
	if want := []string{"b", "d", "c"}; !slices.Equal(got.Seen, want) {
		t.Errorf("seen = %q, want the newest three %q", got.Seen, want)
	}
}
