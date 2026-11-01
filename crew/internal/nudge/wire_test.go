package nudge

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

// Default is the wiring `crew nudge` actually runs, and the arm's own tests inject a
// fake scan, so this pins what the real gate is wired to: every undelivered
// reason — a dispatcher directive or a role verdict — keyed on the branch id for
// the lead and the session id for its marks.
func TestDefaultUnreadReadsEveryUndeliveredReason(t *testing.T) {
	const (
		crew  = "c1"
		br    = "feat/x"
		side  = "worker:feat/x#s100-1"
		disp  = `{"ts":1600000,"crew_id":"c1","from":"dispatcher:c1","to":"` + side + `","kind":"msg","body":"go"}`
		role  = `{"ts":1500000,"crew_id":"c1","from":"role:feat/x:reviewer","to":"` + side + `","kind":"msg","body":"verdict"}`
		other = `{"ts":1700000,"crew_id":"c1","from":"worker:other#s1-1","to":"dispatcher:c1","kind":"status","body":"{}"}`
	)
	paths := bus.Paths{Common: t.TempDir()}
	paths.Dir, paths.Log = paths.Common+"/crew", paths.Common+"/crew/events.jsonl"
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gate := Default(paths, io.Discard, 1, func() string { return crew }).Unread
	scan := func(t *testing.T, rows ...string) string {
		t.Helper()
		if err := os.WriteFile(paths.Log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return gate(context.Background(), crew, br, side)
	}
	deliver := func(t *testing.T, rows ...string) {
		t.Helper()
		vs := make([]any, len(rows))
		for i, r := range rows {
			dec := json.NewDecoder(strings.NewReader(r))
			dec.UseNumber()
			if err := dec.Decode(&vs[i]); err != nil {
				t.Fatal(err)
			}
		}
		if !marks.Record(paths.Dir, crew, side, vs) {
			t.Fatal("marks.Record failed")
		}
	}

	t.Run("no directive", func(t *testing.T) {
		if got := scan(t, other); got != "" {
			t.Errorf("scan = %q, want empty for a log with no directive", got)
		}
	})

	// An undelivered role msg is a nudge reason too: the lead owes the role a
	// reply, so the scan reports it as the pair with the role tag.
	t.Run("a role msg is a nudge reason", func(t *testing.T) {
		if got := scan(t, role, other); got != "1500000 1500000 role" {
			t.Errorf("scan = %q, want %q for an undelivered role msg", got, "1500000 1500000 role")
		}
	})

	// The pair spans every nudge reason, oldest to newest, and src is the oldest's.
	t.Run("an undelivered pair spans role and directive", func(t *testing.T) {
		if got := scan(t, role, disp, other); got != "1500000 1600000 role" {
			t.Errorf("scan = %q, want %q", got, "1500000 1600000 role")
		}
	})

	// What `crew inbox`/`crew await` hand out is a mark on the session, and the scan
	// has to read it: a directive the lead already read is not unread.
	t.Run("a delivered directive is no longer unread", func(t *testing.T) {
		if got := scan(t, disp); got == "" {
			t.Fatal("the directive read as delivered before anything delivered it")
		}
		deliver(t, disp)
		if got := scan(t, disp); got != "" {
			t.Errorf("scan = %q, want empty once the lead was handed the directive", got)
		}
	})

	// The marks are the session's. A branch-keyed caller — the delegation arm's watchdog
	// — cannot name the lead's session, so it cannot read them, and `unread.Scan` says
	// "nothing unread" rather than misreporting.
	t.Run("a branch-keyed scan reads no session marks", func(t *testing.T) {
		if got := Default(paths, io.Discard, 1, func() string { return crew }).Unread(
			context.Background(), crew, br, "worker:"+br); got != "" {
			t.Errorf("scan = %q, want the branch-keyed watchdog silent", got)
		}
	})
}
