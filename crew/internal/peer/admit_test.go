package peer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var testNow = time.UnixMilli(1_790_000_000_000)

func ms(d time.Duration) int64 { return testNow.Add(-d).UnixMilli() }

// writeLines appends each line plus a newline and returns the offset past each.
func writeLines(t *testing.T, path string, lines ...string) []int64 {
	t.Helper()
	var off int64
	if st, err := os.Stat(path); err == nil {
		off = st.Size()
	}
	var ends []int64
	for _, l := range lines {
		appendRaw(t, path, l+"\n")
		off += int64(len(l) + 1)
		ends = append(ends, off)
	}
	return ends
}

func TestExportMissingLog(t *testing.T) {
	rows, cursor, more, err := Export(filepath.Join(t.TempDir(), "events.jsonl"), 0, "halo", testNow)
	if err != nil || len(rows) != 0 || cursor != 0 || more {
		t.Fatalf("Export = %v, %d, %v, %v; want nothing, 0", rows, cursor, more, err)
	}
}

func TestExportSelectsRows(t *testing.T) {
	p := newBus(t)
	now := ms(time.Minute)
	ends := writeLines(t, p.Log,
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"claim-issue","issue":"42","branch":"feat/42-x","pid":123}`, now),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"feat/42-x","session":"s1-2","worker_id":"feat/42-x#s1-2","engine":"claude","model":"opus","host":"mbp","title":"Do <x>","name":"iris","also_closes":[43]}`, now),
		fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/41-y","pr":"7","pr_state":"MERGED","worktree":"/w/41"}`, now),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":"hi"}`, now),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:other","body":"no"}`, now),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"status","from":"worker:feat/42-x","state":"working"}`, now),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","from":"dispatcher:c1","branch":"feat/9-z"}`, now),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"feat/8-old"}`, ms(25*time.Hour)),
	)
	appendRaw(t, p.Log, fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"reap","bra`, now))

	rows, cursor, more, err := Export(p.Log, 0, "halo", testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{ends[0], json.RawMessage(fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"claim-issue","issue":"42","branch":"feat/42-x"}`, now))},
		{ends[1], json.RawMessage(fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"feat/42-x","session":"s1-2","worker_id":"feat/42-x#s1-2","host":"mbp","title":"Do <x>","name":"iris","also_closes":[43]}`, now))},
		{ends[2], json.RawMessage(fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/41-y"}`, now))},
		{ends[3], json.RawMessage(fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":"hi"}`, now))},
	}
	assertRows(t, rows, want)
	if cursor != ends[7] || more {
		t.Errorf("cursor, more = %d, %v; want %d, false", cursor, more, ends[7])
	}

	rows, again, more, err := Export(p.Log, cursor, "halo", testNow)
	if err != nil || len(rows) != 0 || again != cursor || more {
		t.Errorf("second Export = %v, %d, %v, %v; want nothing at %d", rows, again, more, err, cursor)
	}
}

func assertRows(t *testing.T, got, want []Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d:\n%s", len(got), len(want), rowsText(got))
	}
	for i := range want {
		if got[i].Seq != want[i].Seq || string(got[i].Row) != string(want[i].Row) {
			t.Errorf("row %d = %d %s\nwant      %d %s", i, got[i].Seq, got[i].Row, want[i].Seq, want[i].Row)
		}
	}
}

func rowsText(rows []Entry) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%d %s\n", r.Seq, r.Row)
	}
	return b.String()
}

func msgLine(body string) string {
	b, _ := json.Marshal(body)
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":%s}`, ms(0), b)
}

func TestExportStripsBodyControl(t *testing.T) {
	p := newBus(t)
	writeLines(t, p.Log, msgLine("a\x01b\nc\td\u0085e"))
	rows, _, _, err := Export(p.Log, 0, "halo", testNow)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Export = %v, %v", rows, err)
	}
	var row struct{ Body string }
	if err := json.Unmarshal(rows[0].Row, &row); err != nil || row.Body != "ab\nc\tde" {
		t.Errorf("body = %q, %v; want %q", row.Body, err, "ab\nc\tde")
	}
}

func TestExportSkipsOversizeRow(t *testing.T) {
	p := newBus(t)
	ends := writeLines(t, p.Log,
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"claim-issue","issue":"42","branch":"feat/42-%s"}`, ms(0), strings.Repeat("x", MaxRow)),
		msgLine("after"),
	)
	rows, cursor, _, err := Export(p.Log, 0, "halo", testNow)
	if err != nil || len(rows) != 1 || rows[0].Seq != ends[1] || cursor != ends[1] {
		t.Fatalf("Export = %s, %d, %v; want only the msg, cursor %d", rowsText(rows), cursor, err, ends[1])
	}
}

func TestExportShortensALongTitle(t *testing.T) {
	p := newBus(t)
	long := strings.Repeat("é", MaxRow)
	writeLines(t, p.Log,
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"feat/42-x","title":%q}`, ms(0), long),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"feat/43-%s","title":%q}`, ms(0), strings.Repeat("y", MaxRow), long),
	)
	rows, _, _, err := Export(p.Log, 0, "halo", testNow)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Export = %s, %v; want only the row whose title alone was too long", rowsText(rows), err)
	}
	if len(rows[0].Row) > MaxRow {
		t.Errorf("row is %d bytes, over MaxRow", len(rows[0].Row))
	}
	var row struct{ Branch, Title string }
	if err := json.Unmarshal(rows[0].Row, &row); err != nil {
		t.Fatal(err)
	}
	if row.Branch != "feat/42-x" || row.Title == "" || !utf8.ValidString(row.Title) || !strings.HasPrefix(long, row.Title) {
		t.Errorf("row = %+v; want feat/42-x with a shortened whole-rune title", row)
	}
}

func TestExportTruncatesAtMaxRows(t *testing.T) {
	p := newBus(t)
	lines := make([]string, MaxRows+1)
	for i := range lines {
		lines[i] = msgLine(fmt.Sprint(i))
	}
	ends := writeLines(t, p.Log, lines...)
	rows, cursor, more, err := Export(p.Log, 0, "halo", testNow)
	if err != nil || len(rows) != MaxRows || !more || cursor != ends[MaxRows-1] {
		t.Fatalf("Export = %d rows, cursor %d, more %v, %v; want %d, %d, true", len(rows), cursor, more, err, MaxRows, ends[MaxRows-1])
	}
	rows, cursor, more, err = Export(p.Log, cursor, "halo", testNow)
	if err != nil || len(rows) != 1 || more || cursor != ends[MaxRows] {
		t.Fatalf("rest = %d rows, cursor %d, more %v, %v", len(rows), cursor, more, err)
	}
}

func TestExportTruncatesAtMaxBatch(t *testing.T) {
	p := newBus(t)
	lines := make([]string, 150)
	for i := range lines {
		lines[i] = msgLine(strings.Repeat("b", 3000))
	}
	writeLines(t, p.Log, lines...)
	rows, cursor, more, err := Export(p.Log, 0, "halo", testNow)
	if err != nil || !more || len(rows) == 0 || len(rows) == len(lines) {
		t.Fatalf("Export = %d rows, more %v, %v; want a truncated batch", len(rows), more, err)
	}
	size := 0
	for _, r := range rows {
		size += len(r.Row)
	}
	if size > MaxBatch || size+len(rows[0].Row) <= MaxBatch {
		t.Errorf("batch is %d bytes; want the most rows within %d", size, MaxBatch)
	}
	if cursor != rows[len(rows)-1].Seq {
		t.Errorf("cursor = %d, want the last row's seq %d", cursor, rows[len(rows)-1].Seq)
	}
}

func TestExportSinceBeyondSizeReadsFromStart(t *testing.T) {
	p := newBus(t)
	ends := writeLines(t, p.Log, msgLine("one"))
	rows, cursor, _, err := Export(p.Log, 1<<30, "halo", testNow)
	if err != nil || len(rows) != 1 || cursor != ends[0] {
		t.Fatalf("Export = %s, %d, %v; want the one row", rowsText(rows), cursor, err)
	}
}

func TestAdmit(t *testing.T) {
	long := strings.Repeat("t", MaxRow)
	for _, c := range []struct {
		name, row, reason, want string
	}{
		// worker
		{name: "worker sender", row: `{"ts":1,"crew_id":"c1","kind":"msg","from":"worker:feat/x","to":"peer:halo","body":"x"}`, reason: "worker"},
		{name: "role sender", row: `{"ts":1,"crew_id":"c1","kind":"msg","from":"role:critic","to":"peer:halo","body":"x"}`, reason: "worker"},
		{name: "status kind", row: `{"ts":1,"crew_id":"c1","kind":"status","state":"working"}`, reason: "worker"},
		{name: "claim kind", row: `{"ts":1,"crew_id":"c1","kind":"claim","branch":"feat/x"}`, reason: "worker"},
		{name: "resume kind", row: `{"ts":1,"crew_id":"c1","kind":"resume"}`, reason: "worker"},
		{name: "nudge kind", row: `{"ts":1,"crew_id":"c1","kind":"nudge"}`, reason: "worker"},
		{name: "unknown kind", row: `{"ts":1,"crew_id":"c1","kind":"bogus"}`, reason: "worker"},
		// identity
		{name: "dispatch with from", row: `{"ts":1,"crew_id":"c1","kind":"dispatch","from":"dispatcher:c1","branch":"feat/x"}`, reason: "identity"},
		{name: "reap with from", row: `{"ts":1,"kind":"reap","from":"dispatcher:c1","branch":"feat/x"}`, reason: "identity"},
		{name: "msg from another crew", row: `{"ts":1,"crew_id":"c1","kind":"msg","from":"dispatcher:c2","to":"peer:halo","body":"x"}`, reason: "identity"},
		{name: "msg to another host", row: `{"ts":1,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:other","body":"x"}`, reason: "identity"},
		{name: "msg without crew", row: `{"ts":1,"kind":"msg","from":"dispatcher:","to":"peer:halo","body":"x"}`, reason: "identity"},
		{name: "claim-issue bad crew", row: `{"ts":1,"crew_id":"../x","kind":"claim-issue","issue":"42","branch":"feat/42-x"}`, reason: "identity"},
		{name: "dispatch without crew", row: `{"ts":1,"kind":"dispatch","branch":"feat/x"}`, reason: "identity"},
		// oversize
		{name: "row over MaxRow", row: `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","title":"` + long + `"}`, reason: "oversize"},
		{name: "body over MaxBody", row: `{"ts":1,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":"` + strings.Repeat("b", MaxBody+1) + `"}`, reason: "oversize"},
		// control
		{name: "escape in branch", row: `{"ts":1,"crew_id":"c1","kind":"claim-issue","issue":"42","branch":"feat/\u001b[31m"}`, reason: "control"},
		{name: "C1 in title", row: `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","title":"a\u0085b"}`, reason: "control"},
		{name: "bell in body", row: `{"ts":1,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":"a\u0007"}`, reason: "control"},
		{name: "newline in branch", row: `{"ts":1,"kind":"reap","branch":"feat/x\ny"}`, reason: "control"},
		{name: "DEL in also_closes", row: `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","also_closes":["4\u007f"]}`, reason: "control"},
		// malformed
		{name: "ts a string", row: `{"ts":"1","kind":"reap","branch":"feat/x"}`, reason: "malformed"},
		{name: "ts a float", row: `{"ts":1.5,"kind":"reap","branch":"feat/x"}`, reason: "malformed"},
		{name: "row an array", row: `[{"kind":"reap"}]`, reason: "malformed"},
		{name: "row null", row: `null`, reason: "malformed"},
		{name: "trailing value", row: `{"kind":"reap"} {}`, reason: "malformed"},
		{name: "invalid utf-8", row: "{\"ts\":1,\"kind\":\"reap\",\"branch\":\"feat/\xff\"}", reason: "malformed"},
		{name: "no kind", row: `{"ts":1,"branch":"feat/x"}`, reason: "malformed"},
		{name: "remote a string", row: `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","remote":"yes"}`, reason: "malformed"},
		{name: "also_closes a string", row: `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","also_closes":"42"}`, reason: "malformed"},
		{name: "also_closes object item", row: `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","also_closes":[{}]}`, reason: "malformed"},
		{name: "issue a bool", row: `{"ts":1,"crew_id":"c1","kind":"claim-issue","issue":true,"branch":"feat/x"}`, reason: "malformed"},
		{name: "body a number", row: `{"ts":1,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":7}`, reason: "malformed"},
		// accepted, projected
		{
			name: "reap row as crew reap writes it",
			row:  `{"ts":5,"kind":"reap","branch":"feat/42-x","pr":"7","pr_state":"MERGED","worktree":"/w/42","issues":["42"]}`,
			want: `{"ts":5,"kind":"reap","branch":"feat/42-x"}`,
		},
		{
			name: "msg with newline and tab",
			row:  `{"ts":5,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":"a\nb\tc","extra":1}`,
			want: `{"ts":5,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":"a\nb\tc"}`,
		},
		{
			name: "claim-issue as dispatch.sh writes it",
			row:  `{"ts":5,"crew_id":"c1","kind":"claim-issue","issue":"42","branch":"feat/42-x","pid":4242}`,
			want: `{"ts":5,"crew_id":"c1","kind":"claim-issue","issue":"42","branch":"feat/42-x"}`,
		},
		{
			name: "claim-issue numeric issue",
			row:  `{"branch":"feat/42-x","issue":42,"kind":"claim-issue","crew_id":"c1","ts":5}`,
			want: `{"ts":5,"crew_id":"c1","kind":"claim-issue","issue":42,"branch":"feat/42-x"}`,
		},
		{
			name: "dispatch as dispatch.sh writes it",
			row: `{"ts":5,"crew_id":"c1","kind":"dispatch","branch":"feat/42-x","session":"s1-2","worker_id":"feat/42-x#s1-2","engine":"claude","model":"opus",` +
				`"tier":"standard","effort":"medium","shape":"solo","task_kind":"feat","host":"mbp","title":"Fix <a> & b","plan":"","resume":false,"owner_auth":false,` +
				`"engine_session":null,"name":"iris","also_closes":[646],"chrome":true}`,
			want: `{"ts":5,"crew_id":"c1","kind":"dispatch","branch":"feat/42-x","session":"s1-2","worker_id":"feat/42-x#s1-2","host":"mbp","title":"Fix <a> & b","name":"iris","also_closes":[646]}`,
		},
		{
			name: "dispatch string also_closes and remote",
			row:  `{"ts":5,"crew_id":"c1","kind":"dispatch","branch":"feat/42-x","host":"mini","remote":true,"also_closes":["646"]}`,
			want: `{"ts":5,"crew_id":"c1","kind":"dispatch","branch":"feat/42-x","host":"mini","remote":true,"also_closes":["646"]}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			row, reason := Admit(Entry{Seq: 10, Row: json.RawMessage(c.row)}, "halo", testNow)
			if reason != c.reason || string(row) != c.want {
				t.Errorf("Admit = %s, %q; want %s, %q", row, reason, c.want, c.reason)
			}
		})
	}
}

func TestAdmitRefusesAFarFutureTS(t *testing.T) {
	for _, c := range []struct {
		ahead  time.Duration
		reason string
	}{
		{maxSkew, ""},
		{maxSkew + time.Millisecond, "malformed"},
		{100 * 365 * 24 * time.Hour, "malformed"},
	} {
		row := fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"claim-issue","issue":"42","branch":"feat/42-x"}`, ms(-c.ahead))
		if _, reason := Admit(Entry{Seq: 10, Row: json.RawMessage(row)}, "halo", testNow); reason != c.reason {
			t.Errorf("ts %s ahead: reason = %q, want %q", c.ahead, reason, c.reason)
		}
	}
}

func TestAdmitRefusesANonPositiveSeq(t *testing.T) {
	if _, reason := Admit(Entry{Seq: 0, Row: json.RawMessage(`{"ts":1,"kind":"reap","branch":"feat/x"}`)}, "halo", testNow); reason != "malformed" {
		t.Errorf("reason = %q, want malformed", reason)
	}
}
