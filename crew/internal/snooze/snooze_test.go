package snooze

import (
	"testing"
	"time"
)

func TestSetThenActiveUntilLapse(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	k := Key{Tip: "t1", Worktree: "/wt"}
	if Active(dir, "feat/a", k, now) {
		t.Fatal("nothing set yet")
	}
	if err := Set(dir, "feat/a", k, now, now.Add(Default)); err != nil {
		t.Fatal(err)
	}
	if !Active(dir, "feat/a", k, now.Add(Default-time.Second)) {
		t.Fatal("should be snoozed inside the window")
	}
	if Active(dir, "feat/a", k, now.Add(Default)) {
		t.Fatal("should lapse at the deadline")
	}
	if Active(dir, "feat/b", k, now) {
		t.Fatal("other branches are unaffected")
	}
}

func TestSetDropsLapsedEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	_ = Set(dir, "old", Key{}, now, now.Add(time.Hour))
	later := now.Add(2 * time.Hour)
	if err := Set(dir, "new", Key{}, later, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := read(dir); len(got) != 1 {
		t.Fatalf("store %v", got)
	}
}

func TestDifferentTipOrWorktreeIsNotSnoozed(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	k := Key{Tip: "t1", Worktree: "/wt"}
	if err := Set(dir, "feat/a", k, now, now.Add(Default)); err != nil {
		t.Fatal(err)
	}
	for _, other := range []Key{{Tip: "t2", Worktree: "/wt"}, {Tip: "t1", Worktree: "/other"}} {
		if Active(dir, "feat/a", other, now) {
			t.Fatalf("%+v still snoozed", other)
		}
	}
}

func TestSnoozedListsOnlyRunningEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	k := Key{Tip: "t1", Worktree: "/wt"}
	_ = Set(dir, "b", k, now, now.Add(time.Hour))
	_ = Set(dir, "a", k, now, now.Add(2*time.Hour))
	_ = Set(dir, "gone", k, now, now.Add(time.Minute))
	got := Snoozed(dir, now.Add(5*time.Minute))
	if len(got) != 2 || got[0].Branch != "a" || got[1].Branch != "b" || got[0].Key != k || !got[0].Until.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("snoozed %+v", got)
	}
}
