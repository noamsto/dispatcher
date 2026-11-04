package retro

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// bigBus is a synthetic log shaped like a real crew bus: many crews, re-dispatched
// branches, and per run a dispatch row, a stream of status rows and worker msgs
// (metrics snapshots, review seams, retro notes), plus the dispatcher's own rows.
func bigBus(runs int) string {
	var b strings.Builder
	ts := int64(1_789_000_000_000)
	row := func(format string, args ...any) {
		ts += 1000
		fmt.Fprintf(&b, format+"\n", append([]any{ts}, args...)...)
	}
	for i := range runs {
		crew := fmt.Sprintf("c%d", i/50)
		branch := fmt.Sprintf("feat/%d-task", i%(runs*9/10))
		from := fmt.Sprintf("worker:%s#s%d", branch, i)
		row(`{"ts":%d,"crew_id":%q,"kind":"claim-issue","issue":"%d","branch":%q}`, crew, i, branch)
		row(`{"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q,"engine":"claude","model":"sonnet","tier":"standard","plan":"required","title":"t"}`, crew, branch)
		for range 10 {
			row(`{"ts":%d,"crew_id":%q,"kind":"status","from":%q,"to":"dispatcher:%s","body":{"state":"working"}}`, crew, from, crew)
		}
		row(`{"ts":%d,"crew_id":%q,"kind":"msg","from":%q,"to":"review:%s","body":"{\"seam\":\"plan\",\"plan_critic_first_pass\":\"accept\"}"}`, crew, from, crew)
		if i%3 == 0 {
			row(`{"ts":%d,"crew_id":%q,"kind":"msg","from":%q,"to":"retro:%s","body":"{\"seam\":\"execute\",\"tag\":\"gate_thrash\",\"detail\":\"circled build %d\"}"}`, crew, from, crew, i)
		}
		row(`{"ts":%d,"crew_id":%q,"kind":"msg","from":%q,"to":"metrics:%s","body":"{\"review_mode\":\"full\",\"plan_critic_first_pass\":\"accept\",\"notes\":[{\"seam\":\"gate\",\"tag\":\"other\",\"detail\":\"note %d\"}]}"}`, crew, from, crew, i)
		row(`{"ts":%d,"crew_id":%q,"kind":"status","from":%q,"to":"dispatcher:%s","body":{"state":"done"}}`, crew, from, crew)
		row(`{"ts":%d,"crew_id":%q,"kind":"reap","branch":%q}`, crew, branch)
		if i%20 == 0 {
			row(`{"ts":%d,"crew_id":%q,"kind":"msg","from":"dispatcher:%s","to":"retro:%s","body":"{\"seam\":\"dispatch\",\"tag\":\"fanout_binder\",\"detail\":\"d%d\"}"}`, crew, crew, crew, i)
		}
	}
	return b.String()
}

// The per-run scans of the whole bus were quadratic and took 65-86 s here against
// ~0.4 s indexed (BenchmarkReportJSON shows the real number). The bound only has to
// catch their return, so it sits far above what -race or a contended CI box costs.
func TestReportJSONSpeedOnBigBus(t *testing.T) {
	if testing.Short() {
		t.Skip("big-bus speed gate")
	}
	paths := fixture(t, bigBus(900))
	var out, errb bytes.Buffer
	start := time.Now()
	code := Run(Request{Report: true, JSON: true}, paths, &out, &errb, Options{})
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if elapsed >= 15*time.Second {
		t.Errorf("--report --json over a 900-run bus took %v, want well under 15s (indexed: ~0.4s)", elapsed)
	}
}

func BenchmarkReportJSON(b *testing.B) {
	events := bigBus(900)
	if n := strings.Count(events, "\n"); n < 14000 {
		b.Fatalf("synthetic bus has %d events, want >= 14000", n)
	}
	paths := fixture(b, events)
	b.ResetTimer()
	for b.Loop() {
		var out, errb bytes.Buffer
		if code := Run(Request{Report: true, JSON: true}, paths, &out, &errb, Options{}); code != 0 {
			b.Fatalf("exit %d: %s", code, errb.String())
		}
	}
}
