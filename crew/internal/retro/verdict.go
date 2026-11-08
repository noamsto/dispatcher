package retro

import "strconv"

const (
	settleDays    = 3
	heldFloorDays = 7
	heldMinRuns   = 20
	recurAfter    = 3
	windowDays    = 30

	dayMs    = int64(24 * 60 * 60 * 1000)
	settleMs = settleDays * dayMs
)

const (
	verdictHeld       = "held"
	verdictDidNotHold = "did-not-hold"
	verdictInconclu   = "inconclusive"
)

// window selects timestamps with Lo < ts <= Hi.
type window struct{ Lo, Hi int64 }

func afterWindow(fix, dayEnd int64) window {
	return window{Lo: max(fix+settleMs, dayEnd-windowDays*dayMs), Hi: dayEnd}
}

func beforeWindow(fix int64) window {
	return window{Lo: fix - windowDays*dayMs - 1, Hi: fix - 1}
}

func (w window) has(ts int64) bool { return ts > w.Lo && ts <= w.Hi }

func countIn(occ []occurrence, w window) int {
	seen := map[string]struct{}{}
	for _, o := range occ {
		if w.has(o.TS) {
			seen[o.Unit] = struct{}{}
		}
	}
	return len(seen)
}

func runsIn(runTS []int64, w window) int {
	n := 0
	for _, ts := range runTS {
		if w.has(ts) {
			n++
		}
	}
	return n
}

// evaluate applies the spec's verdict table with integer arithmetic only.
func evaluate(hitsB, runsB, hitsA, runsA int, fix, dayEnd int64) (verdict, reason string) {
	if hitsA >= recurAfter && 2*hitsA*runsB >= hitsB*runsA {
		return verdictDidNotHold, ""
	}
	settled := dayEnd >= fix+settleMs+heldFloorDays*dayMs
	hasBase := hitsB > 0 && runsB > 0
	need := heldMinRuns
	if hasBase {
		need = max(heldMinRuns, (recurAfter*runsB+hitsB-1)/hitsB)
	}
	if hitsA == 0 && hasBase && runsA >= need && settled {
		return verdictHeld, ""
	}
	switch {
	case hitsA >= recurAfter:
		return verdictInconclu, "rate fell by more than half but still recurring"
	case hitsA > 0:
		return verdictInconclu, "below recurrence h<3"
	case !settled:
		return verdictInconclu, "settling"
	case !hasBase:
		return verdictInconclu, "no baseline before the fix"
	}
	return verdictInconclu, "too few runs " + strconv.Itoa(runsA) + "/" + strconv.Itoa(need)
}
