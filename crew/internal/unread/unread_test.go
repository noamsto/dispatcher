package unread

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/marks"
)

const (
	crew   = "c1"
	branch = "feat/x"
	me     = "worker:feat/x"
	sid    = "worker:feat/x#s100-1" // the sessioned watchdog's lead
	t0     = 1_000_000
)

// env is a temp crew dir with the bus log under it, as bus.Paths lays out.
func env(t *testing.T) (dir, log string) {
	t.Helper()
	root := t.TempDir()
	dir = root + "/crew"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "events.jsonl")
}

func writeLog(t *testing.T, log string, rows ...string) {
	t.Helper()
	if err := os.WriteFile(log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// msg is a msg row; kind keeps the status/nudge rows distinct.
func msg(ts int64, from, to string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":%q,"kind":"msg","body":"x"}`, ts, from, to)
}

func status(ts int64, from string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":"dispatcher:c1","kind":"status","body":"{}"}`, ts, from)
}

// deliver replays what `crew await`/`crew inbox` hand out: the marks those
// readers raise are marks.Record's, so a delivered msg disappears from the scan.
func deliver(t *testing.T, dir string, rows ...string) {
	t.Helper()
	vs := make([]any, len(rows))
	for i, r := range rows {
		dec := json.NewDecoder(strings.NewReader(r))
		dec.UseNumber()
		if err := dec.Decode(&vs[i]); err != nil {
			t.Fatal(err)
		}
	}
	if !marks.Record(dir, crew, sid, vs) {
		t.Fatal("marks.Record failed")
	}
}

func fill(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = `{"kind":"msg"}`
	}
	return out
}

func scan(dir, log string, mode Mode) string {
	return Scan(Request{Dir: dir, Log: log, Crew: crew, Branch: branch, Me: me, FromID: sid, T0: t0}, mode)
}

func TestScan(t *testing.T) {
	roleMsg := msg(1_500_000, "role:feat/x:reviewer", sid)
	dispMsg := msg(1_600_000, "dispatcher:c1", sid)
	leadReply := msg(1_700_000, sid, "role:feat/x:reviewer")

	cases := []struct {
		name       string
		rows       []string
		deliv      []string // rows the lead was handed, raising their senders' marks
		wantOldest string
		wantAny    string
	}{
		{
			name:       "empty log",
			wantOldest: "", wantAny: "",
		},
		{
			name:       "undelivered role msg",
			rows:       []string{roleMsg},
			wantOldest: "1500000 role", wantAny: "1500000 1500000 role",
		},
		{
			name:       "undelivered directive",
			rows:       []string{dispMsg},
			wantOldest: "1600000 dispatcher", wantAny: "1600000 1600000 dispatcher",
		},
		{
			name:       "oldest of both kinds, directive pair spans",
			rows:       []string{dispMsg, roleMsg, msg(1_800_000, "dispatcher:c1", sid)},
			wantOldest: "1500000 role", wantAny: "1500000 1800000 role",
		},
		{
			// A role msg drops out once the lead answered that role later;
			// an earlier reply does not answer it, and a lead msg to the
			// dispatcher is no answer either.
			name:       "role msg answered by the lead drops out",
			rows:       []string{roleMsg, leadReply, dispMsg},
			wantOldest: "1600000 dispatcher", wantAny: "1600000 1600000 dispatcher",
		},
		{
			name:       "a reply older than the verdict does not answer it",
			rows:       []string{msg(1_400_000, sid, "role:feat/x:reviewer"), roleMsg},
			wantOldest: "1500000 role", wantAny: "1500000 1500000 role",
		},
		{
			name:       "a lead msg to the dispatcher does not clear the directive",
			rows:       []string{dispMsg, msg(1_700_000, sid, "dispatcher:c1")},
			wantOldest: "1600000 dispatcher", wantAny: "1600000 1600000 dispatcher",
		},
		{
			// Delivery is the mark: the same rows, once handed out.
			name:       "delivered msgs drop out of both modes",
			rows:       []string{roleMsg, dispMsg},
			deliv:      []string{roleMsg, dispMsg},
			wantOldest: "", wantAny: "",
		},
		{
			name:       "a delivered directive leaves the undelivered verdict",
			rows:       []string{roleMsg, dispMsg},
			deliv:      []string{dispMsg},
			wantOldest: "1500000 role", wantAny: "1500000 1500000 role",
		},
		{
			name:       "a verdict for another session of the branch is not this lead's",
			rows:       []string{msg(1_500_000, "role:feat/x:reviewer", "worker:feat/x#s99-1")},
			wantOldest: "", wantAny: "",
		},
		{
			name: "other crews, branches, kinds and roles are not this scan's rows",
			rows: []string{
				fmt.Sprintf(`{"ts":1500000,"crew_id":"c2","from":"role:feat/x:reviewer","to":%q,"kind":"msg"}`, sid),
				msg(1_500_000, "role:feat/y:reviewer", sid),
				msg(1_500_000, "role:feat/x:reviewer", "dispatcher:c1"),
				status(1_500_000, "dispatcher:c1"),
			},
			wantOldest: "", wantAny: "",
		},
		{
			// The helper scopes by t0 only when it cannot trust the marks it
			// reads — a sessioned scan reads this run's marks, so older rows
			// still count.
			name:       "a pre-t0 row still counts for a sessioned scan",
			rows:       []string{msg(500_000, "role:feat/x:reviewer", sid)},
			wantOldest: "500000 role", wantAny: "500000 500000 role",
		},
		{
			name:       "junk lines drop out, a torn tail alone",
			rows:       []string{"not json", `{"ts":`, "", roleMsg},
			wantOldest: "1500000 role", wantAny: "1500000 1500000 role",
		},
		{
			// A row that decodes but is no object breaks the fold the way it
			// broke jq; the helper's `2>/dev/null || true` read that as empty.
			name:       "a bare-string row silences the scan, as a jq error did",
			rows:       []string{`"a string"`, roleMsg},
			wantOldest: "", wantAny: "",
		},
		{
			name:       "a msg within the last 2000 lines reads",
			rows:       append([]string{roleMsg}, fill(1999)...),
			wantOldest: "1500000 role", wantAny: "1500000 1500000 role",
		},
		{
			name:       "a msg scrolled out of the 2000-line tail reads as gone",
			rows:       append([]string{roleMsg}, fill(2000)...),
			wantOldest: "", wantAny: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, log := env(t)
			if tc.rows != nil {
				writeLog(t, log, tc.rows...)
			}
			if tc.deliv != nil {
				deliver(t, dir, tc.deliv...)
			}
			r := Request{Dir: dir, Log: log, Crew: crew, Branch: branch, Me: me, FromID: sid, T0: t0}
			if got := Scan(r, Oldest); got != tc.wantOldest {
				t.Errorf("oldest = %q, want %q", got, tc.wantOldest)
			}
			if got := Scan(r, Any); got != tc.wantAny {
				t.Errorf("any = %q, want %q", got, tc.wantAny)
			}
		})
	}
}

// A branch-keyed watchdog cannot name the lead's session, so it cannot read
// that session's marks; it stays silent rather than misreport.
func TestScanBranchKeyedIsSilent(t *testing.T) {
	dir, log := env(t)
	writeLog(t, log, msg(1_500_000, "role:feat/x:reviewer", me))
	r := Request{Dir: dir, Log: log, Crew: crew, Branch: branch, Me: me, FromID: me, T0: 0}
	if got := Scan(r, Oldest); got != "" {
		t.Errorf("oldest = %q, want silent", got)
	}
	if got := Scan(r, Any); got != "" {
		t.Errorf("any = %q, want silent", got)
	}
}

// A missing log is no log: the helper's `[ -f "$log" ] || return 0`.
func TestScanMissingLog(t *testing.T) {
	dir, log := env(t)
	r := Request{Dir: dir, Log: log, Crew: crew, Branch: branch, Me: me, FromID: sid, T0: t0}
	if got := Scan(r, Oldest); got != "" {
		t.Errorf("oldest = %q, want empty", got)
	}
	if got := Scan(r, Any); got != "" {
		t.Errorf("any = %q, want empty", got)
	}
}

// The marks the Go readers raise clear the scan: undelivered before the hand-
// out, gone after — the cross-reader contract the bash `_unread_scan` guards
// replayed before this scan moved in process (#968).
func TestScanClearsOnMarksTheGoReadersRaise(t *testing.T) {
	dir, log := env(t)
	m := msg(1_500_000, "dispatcher:c1", sid)
	writeLog(t, log, m)
	if got := scan(dir, log, Any); got != "1500000 1500000 dispatcher" {
		t.Fatalf("before delivery: %q", got)
	}
	deliver(t, dir, m)
	if got := scan(dir, log, Any); got != "" {
		t.Errorf("after delivery: %q, want empty", got)
	}
}
