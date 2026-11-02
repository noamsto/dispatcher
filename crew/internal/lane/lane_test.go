package lane

import (
	"fmt"
	"os"
	"path/filepath"
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
