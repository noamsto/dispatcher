package retro

import "testing"

func TestEvaluate(t *testing.T) {
	const fix = int64(1_000_000_000_000)
	settledEnd := fix + 10*dayMs // fix + 3d settle + 7d floor
	tests := []struct {
		name                       string
		hitsB, runsB, hitsA, runsA int
		dayEnd                     int64
		verdict, reason            string
	}{
		{"rate equal", 5, 100, 5, 100, settledEnd, "did-not-hold", ""},
		{"rate exactly half", 4, 100, 3, 150, settledEnd, "did-not-hold", ""}, // 2*3*100 == 4*150
		{"rate just under half", 4, 100, 3, 151, settledEnd, "inconclusive", "rate fell by more than half but still recurring"},
		{"well under half, still recurring", 50, 100, 3, 100, settledEnd, "inconclusive", "rate fell by more than half but still recurring"},
		{"hits 2", 5, 100, 2, 10, settledEnd, "inconclusive", "below recurrence h<3"},
		{"hits 1", 5, 100, 1, 10, settledEnd, "inconclusive", "below recurrence h<3"},
		{"held at 3/p_b boundary", 2, 100, 0, 150, settledEnd, "held", ""},
		{"one short of 3/p_b", 2, 100, 0, 149, settledEnd, "inconclusive", "too few runs 149/150"},
		{"floor 20 held", 50, 100, 0, 20, settledEnd, "held", ""},
		{"floor 20 one short", 50, 100, 0, 19, settledEnd, "inconclusive", "too few runs 19/20"},
		{"zero baseline with 3 hits", 0, 100, 3, 50, settledEnd, "did-not-hold", ""},
		{"zero baseline zero hits", 0, 100, 0, 50, settledEnd, "inconclusive", "no baseline before the fix"},
		{"zero baseline 2 hits", 0, 100, 2, 50, settledEnd, "inconclusive", "below recurrence h<3"},
		{"settle boundary held", 2, 100, 0, 150, fix + settleMs + heldFloorDays*dayMs, "held", ""},
		{"one ms before boundary", 2, 100, 0, 150, fix + settleMs + heldFloorDays*dayMs - 1, "inconclusive", "settling"},
		{"settling beats too few runs", 2, 100, 0, 1, fix + dayMs, "inconclusive", "settling"},
		{"settling beats no baseline", 0, 100, 0, 1, fix + dayMs, "inconclusive", "settling"},
		{"did-not-hold ignores settling", 5, 100, 3, 100, fix + dayMs, "did-not-hold", ""},
		{"runsB zero", 0, 0, 0, 50, settledEnd, "inconclusive", "no baseline before the fix"},
		{"runsB zero with hits", 0, 0, 3, 50, settledEnd, "did-not-hold", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, r := evaluate(tc.hitsB, tc.runsB, tc.hitsA, tc.runsA, fix, tc.dayEnd)
			if v != tc.verdict || r != tc.reason {
				t.Errorf("got (%q, %q), want (%q, %q)", v, r, tc.verdict, tc.reason)
			}
		})
	}
}

func TestWindows(t *testing.T) {
	const fix = int64(1_000_000_000_000)
	dayEnd := fix + 60*dayMs
	if got, want := afterWindow(fix, dayEnd), (window{dayEnd - 30*dayMs, dayEnd}); got != want {
		t.Errorf("long after: got %+v want %+v", got, want)
	}
	dayEnd = fix + 10*dayMs
	if got, want := afterWindow(fix, dayEnd), (window{fix + 3*dayMs, dayEnd}); got != want {
		t.Errorf("short after: got %+v want %+v", got, want)
	}
	b := beforeWindow(fix)
	if !b.has(fix-30*dayMs) || b.has(fix-30*dayMs-1) || !b.has(fix-1) || b.has(fix) {
		t.Errorf("before window is not [fix-30d, fix): %+v", b)
	}
}

func TestCountAndRunsIn(t *testing.T) {
	w := window{Lo: 100, Hi: 200}
	occ := []occurrence{
		{"a", 100}, // Lo excluded
		{"b", 101},
		{"b", 150}, // duplicate unit
		{"c", 200}, // Hi included
		{"d", 201},
	}
	if got := countIn(occ, w); got != 2 {
		t.Errorf("countIn = %d, want 2", got)
	}
	if got := countIn(nil, w); got != 0 {
		t.Errorf("countIn(nil) = %d, want 0", got)
	}
	if got := runsIn([]int64{100, 101, 150, 200, 201}, w); got != 3 {
		t.Errorf("runsIn = %d, want 3", got)
	}
}
