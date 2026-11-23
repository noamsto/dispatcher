package snooze

import (
	"testing"
	"time"
)

func TestSetThenActiveUntilLapse(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	if Active(dir, "feat/a", now) {
		t.Fatal("nothing set yet")
	}
	if err := Set(dir, "feat/a", now, now.Add(Default)); err != nil {
		t.Fatal(err)
	}
	if !Active(dir, "feat/a", now.Add(Default-time.Second)) {
		t.Fatal("should be snoozed inside the window")
	}
	if Active(dir, "feat/a", now.Add(Default)) {
		t.Fatal("should lapse at the deadline")
	}
	if Active(dir, "feat/b", now) {
		t.Fatal("other branches are unaffected")
	}
}

func TestSetDropsLapsedEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	_ = Set(dir, "old", now, now.Add(time.Hour))
	later := now.Add(2 * time.Hour)
	if err := Set(dir, "new", later, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := read(dir); len(got) != 1 {
		t.Fatalf("store %v", got)
	}
}
