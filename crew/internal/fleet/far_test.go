package fleet_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/fleet/fleettest"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

const farNow = int64(1_800_000_000_000)

// farLane is halo's overseer with mbp its one partner, pulling a far fleet
// through ServeFar. Its inbox is the lane's: replies above the floor and not
// yet seen are delivered, then the floor rises to the answer's ts less
// fleet.FarSettle.
type farLane struct {
	far   *fleettest.Far
	state string
	opts  fleet.StatusOptions
	start int64
}

func newFarLane(t *testing.T) *farLane {
	t.Helper()
	l := &farLane{far: fleettest.NewFar(t, "mbp", farNow), state: t.TempDir(), start: farNow - time.Hour.Milliseconds()}
	l.opts = fleettest.Overseer(t, "halo", l.state, func() time.Time { return time.UnixMilli(farNow) }, l.far.Serve(), "mbp")
	return l
}

// host is one status read: mbp's host status and everything the collection
// printed (its JSON document and stderr).
func (l *farLane) host(t *testing.T) (fleet.HostStatus, string) {
	t.Helper()
	var stderr, out bytes.Buffer
	doc := fleet.CollectStatus(t.Context(), fleet.StatusRequest{}, &stderr, l.opts)
	if err := jsonval.Encode(&out, doc, false); err != nil {
		t.Fatal(err)
	}
	if len(doc.Hosts) != 1 {
		t.Fatalf("hosts = %+v, want mbp", doc.Hosts)
	}
	return doc.Hosts[0], out.String() + stderr.String()
}

// pull is one status read of an ok mbp: its host status, the bodies the lane
// delivers, and everything the collection printed.
func (l *farLane) pull(t *testing.T) (fleet.HostStatus, []string, string) {
	t.Helper()
	h, printed := l.host(t)
	if h.State != "ok" || h.TS == nil {
		t.Fatalf("host mbp = %s (%v), want ok", h.State, deref(h.Error))
	}
	path := lane.OverseerHostInbox(l.state, "mbp")
	in, ok := lane.ReadFarInbox(path)
	if !ok {
		in = lane.FarInbox{Floor: l.start}
	}
	seen := map[string]bool{}
	for _, k := range in.Seen {
		seen[k] = true
	}
	delivered := map[string]int64{}
	var bodies []string
	for _, b := range h.Batches() {
		for _, ev := range b.Events {
			ts, _ := watch.WholeMS(ev["ts"])
			raw, _ := json.Marshal(ev)
			key := b.Common + "\x00" + string(raw)
			if ts <= in.Floor {
				continue
			}
			delivered[key] = ts
			if !seen[key] {
				body, _ := ev["body"].(string)
				bodies = append(bodies, body)
			}
		}
	}
	if err := lane.WriteFarInbox(path, in.Next(*h.TS-fleet.FarSettle.Milliseconds(), delivered, 4*fleet.FarMaxEvents)); err != nil {
		t.Fatal(err)
	}
	return h, bodies, printed
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A far repo whose label or common dir is no identifier still has its replies
// delivered, unlabeled; neither its raw label nor any far common dir is
// printed, and the host's skipped notes count the unlabeled repos.
func TestFarUnlabeledReposDeliverEndToEnd(t *testing.T) {
	l := newFarLane(t)
	long := strings.Repeat("l", 130)
	names := []string{"my repo", "c++", long}
	for i, name := range names {
		l.far.Repo(name)
		l.far.Append(name, fleettest.Reply(farNow-int64(i+1)*10_000, "c1", "halo", "body-"+name[:2]))
	}
	h, bodies, printed := l.pull(t)
	slices.Sort(bodies)
	if want := []string{"body-c+", "body-ll", "body-my"}; !slices.Equal(bodies, want) {
		t.Errorf("delivered %q, want %q", bodies, want)
	}
	labels := map[string]bool{}
	for _, b := range h.Batches() {
		labels[b.Repo] = true
	}
	if want := map[string]bool{"": true, strings.Repeat("l", 120): true}; fmt.Sprint(labels) != fmt.Sprint(want) {
		t.Errorf("batch labels = %v, want the two invalid ones empty and the long one as it displays", labels)
	}
	root := filepath.Dir(filepath.Dir(l.far.Common("c++")))
	var lines strings.Builder
	lines.WriteString(printed)
	for _, b := range h.Batches() {
		raw, _ := json.Marshal(b.Events)
		fmt.Fprintf(&lines, "%s %s\n", b.Repo, raw)
	}
	for _, m := range h.Misroutes() {
		fmt.Fprintf(&lines, "%s %s\n", m.Repo, m.From)
	}
	for _, raw := range []string{root, "my?repo", "c??", "my repo", "c++", long} {
		if strings.Contains(lines.String(), raw) {
			t.Errorf("%q was printed:\n%s", raw, lines.String())
		}
	}
	want := fleet.Skipped{Path: "", Status: "2 repo(s) with an invalid label shown unlabeled"}
	if !slices.Contains(h.Skipped, want) {
		t.Errorf("skipped = %+v, want %+v", h.Skipped, want)
	}
	for _, r := range h.Repos {
		if r.Common != "" {
			t.Errorf("far repo %q shows common %q", r.Repo, r.Common)
		}
	}
}

// An answer over the event cap is truncated oldest-first with its ts lowered
// below the first row it left out, so successive pulls deliver every reply
// once and the host never turns unreachable.
func TestFarOverCapAnswerDeliversInPulls(t *testing.T) {
	l := newFarLane(t)
	l.far.Repo("a")
	const n = 1200
	var lines []string
	for i := range n {
		lines = append(lines, fleettest.Reply(farNow-3_000_000+int64(i)*1000, "c1", "halo", fmt.Sprintf("r%04d", i)))
	}
	l.far.Append("a", lines...)
	var all []string
	for p := 1; p <= 3; p++ {
		_, bodies, _ := l.pull(t)
		if len(bodies) > fleet.FarMaxEvents {
			t.Errorf("pull %d delivered %d replies, over the cap", p, len(bodies))
		}
		all = append(all, bodies...)
	}
	if len(all) != n {
		t.Fatalf("three pulls delivered %d replies, want %d", len(all), n)
	}
	if _, again, _ := l.pull(t); len(again) != 0 {
		t.Errorf("a fourth pull delivered %d replies again", len(again))
	}
	for i, b := range all {
		if want := fmt.Sprintf("r%04d", i); b != want {
			t.Fatalf("reply %d = %s, want %s: oldest first, each once", i, b, want)
		}
	}
}

// replies are n replies from c1 to halo, all at ts, bodies prefix0000 on.
func replies(n int, ts int64, prefix string) []string {
	var lines []string
	for i := range n {
		lines = append(lines, fleettest.Reply(ts, "c1", "halo", fmt.Sprintf("%s%04d", prefix, i)))
	}
	return lines
}

// A cut never splits a ts group: the first pull delivers the older group
// whole and nothing of the next, and later pulls deliver the rest, each once.
func TestFarCutKeepsTsGroupsWhole(t *testing.T) {
	l := newFarLane(t)
	l.far.Repo("a")
	t1, t2 := farNow-120_000, farNow-60_000
	l.far.Append("a", slices.Concat(replies(300, t1, "a"), replies(300, t2, "b"))...)
	from := func(prefix string) func(string) bool {
		return func(b string) bool { return !strings.HasPrefix(b, prefix) }
	}
	if _, first, _ := l.pull(t); len(first) != 300 || slices.ContainsFunc(first, from("a")) {
		t.Fatalf("first pull delivered %d replies, want the 300 at t1 only", len(first))
	}
	if _, second, _ := l.pull(t); len(second) != 300 || slices.ContainsFunc(second, from("b")) {
		t.Errorf("second pull delivered %d replies, want the 300 at t2", len(second))
	}
	if _, again, _ := l.pull(t); len(again) != 0 {
		t.Errorf("a third pull delivered %d replies again", len(again))
	}
}

// A ts group over the event cap cannot be cut without serving the same rows
// again on every pull: the far refuses its answer and the host reads
// unreachable in this host's words, never a silent ok.
func TestFarAnswerThatCannotAdvanceIsUnreachable(t *testing.T) {
	l := newFarLane(t)
	l.far.Repo("a")
	ts := farNow - 120_000
	l.far.Append("a", slices.Concat(replies(600, ts, "a"), replies(1, ts+1000, "later"))...)
	h, printed := l.host(t)
	if h.State != "unreachable" || deref(h.Error) != "exit status 1" {
		t.Errorf("host mbp = %s (%s), want unreachable (exit status 1)", h.State, deref(h.Error))
	}
	if strings.Contains(printed, "cannot advance") {
		t.Errorf("the far refusal was printed here:\n%s", printed)
	}
}
