package release

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

const (
	mb     = "feat/m"
	tip40  = "0123456789abcdef0123456789abcdef01234567"
	tree40 = "89abcdef0123456789abcdef0123456789abcdef"
)

func dispatchRow(session string) string {
	return fmt.Sprintf(`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":%q,"session":%q,"worker_id":"worker:%s#%s"}`, mb, session, mb, session)
}

func remoteRow(session, host string) string {
	return fmt.Sprintf(`{"ts":4,"crew_id":"c1","kind":"dispatch","branch":%q,"session":%q,"worker_id":"worker:%s#%s","host":%q,"remote":true}`, mb, session, mb, session, host)
}

func resumeRow(prev, next string) string {
	return fmt.Sprintf(`{"ts":2,"crew_id":"c1","kind":"resume","branch":%q,"prev_worker_id":"worker:%s#%s","worker_id":"worker:%s#%s"}`, mb, mb, prev, mb, next)
}

func statusRow(session, body string) string {
	return fmt.Sprintf(`{"ts":3,"crew_id":"c1","from":"worker:%s#%s","to":"dispatcher:c1","kind":"status","body":%s}`, mb, session, body)
}

func events(t *testing.T, rows ...string) []bus.Event {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evs, err := bus.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

const markJSON = `{"state":"migrated","to_host":"mbp","source":"migrate","mode":"now","tip":"` + tip40 + `","tree":"` + tree40 + `","dir":"/c/migrate/feat/m/20270115T080000Z","patch":"","detail":"moved to mbp"}`

func TestReleased(t *testing.T) {
	d := dispatchRow("s1-1")
	mark := statusRow("s1-1", markJSON)
	for _, c := range []struct {
		name string
		rows []string
		host string
		want bool
	}{
		{"mark", []string{d, statusRow("s1-1", `{"state":"working"}`), mark}, "mbp", true},
		{"another host", []string{d, mark}, "studio", false},
		{"exited after", []string{d, mark, statusRow("s1-1", `{"state":"exited","detail":"SessionEnd"}`)}, "mbp", true},
		{"watchdog after", []string{d, mark, statusRow("s1-1", `{"state":"blocked","detail":"dead: pane gone","source":"watchdog"}`)}, "mbp", true},
		{"working after", []string{d, mark, statusRow("s1-1", `{"state":"working"}`)}, "mbp", false},
		{"worker-posted", []string{d, statusRow("s1-1", `{"state":"migrated","to_host":"mbp","detail":"moved"}`)}, "mbp", false},
		{"older session's mark", []string{d, mark, resumeRow("s1-1", "s2-1")}, "mbp", false},
		{"resumed session's mark", []string{d, resumeRow("s1-1", "s2-1"), statusRow("s2-1", markJSON)}, "mbp", true},
		{"no dispatch row", []string{mark}, "mbp", false},
		{"empty host", []string{d, statusRow("s1-1", `{"state":"migrated","source":"migrate","to_host":""}`)}, "", false},
		// The confirmed remote resume is the newest dispatch row, under the
		// remote worker's id: it must not hide the local worker's mark.
		{"remote row after", []string{d, mark, remoteRow("s9-1", "mbp")}, "mbp", true},
		{"only a remote row", []string{remoteRow("s9-1", "mbp")}, "mbp", false},
	} {
		if got := Released(events(t, c.rows...), mb, c.host); got != c.want {
			t.Errorf("%s: Released=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestActiveRelease(t *testing.T) {
	rel, ok := Active(events(t, dispatchRow("s1-1"), statusRow("s1-1", markJSON), remoteRow("s9-1", "mbp")), mb)
	if !ok || rel != (Release{ToHost: "mbp", Mode: "now", Tip: tip40, Tree: tree40, Dir: "/c/migrate/feat/m/20270115T080000Z"}) {
		t.Fatalf("release %+v %v", rel, ok)
	}
}

func TestNewestWorker(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []string
		want string
	}{
		{"dispatch", []string{dispatchRow("s1-1")}, "worker:feat/m#s1-1"},
		{"resumed", []string{dispatchRow("s1-1"), resumeRow("s1-1", "s2-1")}, "worker:feat/m#s2-1"},
		{"remote row skipped", []string{dispatchRow("s1-1"), remoteRow("s9-1", "mbp")}, "worker:feat/m#s1-1"},
		{"none", []string{statusRow("s1-1", `{"state":"working"}`)}, ""},
	} {
		if got := NewestWorker(events(t, c.rows...), mb); got != c.want {
			t.Errorf("%s: NewestWorker=%q, want %q", c.name, got, c.want)
		}
	}
}
