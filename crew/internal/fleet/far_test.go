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
)

const farNow = int64(1_800_000_000_000)

// farLane is halo's overseer with mbp its one partner, pulling a far fleet
// through ServeFar.
type farLane struct {
	far   *fleettest.Far
	state string
	opts  fleet.StatusOptions
}

func newFarLane(t *testing.T) *farLane {
	t.Helper()
	l := &farLane{far: fleettest.NewFar(t, "mbp", farNow), state: t.TempDir()}
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A far repo whose label or common dir is no identifier is served unlabeled;
// neither its raw label nor any far common dir is printed, and the host's
// skipped notes count the unlabeled repos.
func TestFarUnlabeledReposServeUnlabeled(t *testing.T) {
	l := newFarLane(t)
	long := strings.Repeat("l", 130)
	names := []string{"my repo", "c++", long}
	for i, name := range names {
		l.far.Repo(name)
		l.far.Append(name, fleettest.Reply(farNow-int64(i+1)*10_000, "c1", "halo", "body-"+name[:2]))
	}
	h, printed := l.host(t)
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

// replies are n replies from c1 to halo, all at ts, bodies prefix0000 on.
func replies(n int, ts int64, prefix string) []string {
	var lines []string
	for i := range n {
		lines = append(lines, fleettest.Reply(ts, "c1", "halo", fmt.Sprintf("%s%04d", prefix, i)))
	}
	return lines
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
