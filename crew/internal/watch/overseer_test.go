package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

const overseerMe = "overseer:host1"

// overseerMsg is a msg row to an arbitrary recipient; crew "" leaves crew_id off.
func overseerMsg(ts int64, crew, from, to, body string) string {
	row := `{"ts":` + itoa(ts)
	if crew != "" {
		row += `,"crew_id":` + quote(crew)
	}
	return row + `,"from":` + quote(from) + `,"to":` + quote(to) + `,"kind":"msg","body":` + quote(body) + "}"
}

func foldOverseer(t *testing.T, since int64, rows ...string) (OverseerFold, bool) {
	t.Helper()
	p := fixture(t)
	writeLog(t, p, rows...)
	return FoldOverseer(p.Log, overseerMe, since)
}

func batchOf(t *testing.T, cursor int64, rows ...string) string {
	t.Helper()
	return testjson.Compact(testjson.MustParse(t, `{"cursor":`+itoa(cursor)+`,"events":[`+strings.Join(rows, ",")+`]}`))
}

func TestFoldOverseerSplitsDispatcherMsgsFromMisrouted(t *testing.T) {
	ok1 := overseerMsg(1001, "c1", "dispatcher:c1", overseerMe, "ack")
	ok2 := overseerMsg(1006, "c2", "dispatcher:c2", overseerMe, "re 4")
	rows := []string{
		overseerMsg(900, "c1", "dispatcher:c1", overseerMe, "already seen"),
		ok1,
		overseerMsg(1002, "c1", "worker:feat/x#s1-1", overseerMe, "forged"),
		overseerMsg(1003, "c1", "dispatcher:c2", overseerMe, "another crew's dispatcher"),
		overseerMsg(1004, "", "dispatcher:", overseerMe, "no crew"),
		`{"ts":1005,"crew_id":7,"from":"dispatcher:7","to":"` + overseerMe + `","kind":"msg","body":"odd crew"}`,
		ok2,
		overseerMsg(1007, "c1", "worker:feat/x#s1-1", "dispatcher:c1", "to the dispatcher"),
		overseerMsg(1008, "c1", "dispatcher:c1", "overseer:host2", "another host"),
		statusIn("c1", 1009, "worker:feat/x#s1-1", "blocked", "", ""),
	}
	got, ok := foldOverseer(t, 1000, rows...)
	if !ok {
		t.Fatal("fold found nothing")
	}
	if got.Cursor != 1006 {
		t.Errorf("cursor = %d, want 1006 (the newest msg to me, misrouted or not)", got.Cursor)
	}
	if b := testjson.Compact(got.Batch); b != batchOf(t, 1006, ok1, ok2) {
		t.Errorf("batch = %s, want %s", b, batchOf(t, 1006, ok1, ok2))
	}
	want := []Misrouted{
		{TS: 1002, Crew: "c1", From: "worker:feat/x#s1-1"},
		{TS: 1003, Crew: "c1", From: "dispatcher:c2"},
		{TS: 1004, Crew: "", From: "dispatcher:"},
		{TS: 1005, Crew: "", From: "dispatcher:7"},
	}
	if !reflect.DeepEqual(got.Misrouted, want) {
		t.Errorf("misrouted = %+v, want %+v", got.Misrouted, want)
	}
}

func TestFoldOverseerOnlyMisroutedAdvancesWithNoBatch(t *testing.T) {
	got, ok := foldOverseer(t, 0, overseerMsg(2000, "c1", "worker:feat/x#s1-1", overseerMe, "forged"))
	if !ok {
		t.Fatal("a misrouted msg must still fold, or the cursor never passes it")
	}
	if got.Batch != nil || got.Cursor != 2000 || len(got.Misrouted) != 1 {
		t.Errorf("fold = %+v, want no batch, cursor 2000 and one misrouted", got)
	}
}

func TestFoldOverseerNothingNewPollsAgain(t *testing.T) {
	if got, ok := foldOverseer(t, 5000,
		overseerMsg(5000, "c1", "dispatcher:c1", overseerMe, "at the cursor"),
		overseerMsg(6000, "c1", "dispatcher:c1", "dispatcher:c1", "not to me"),
	); ok {
		t.Errorf("fold = %+v, want nothing above the cursor", got)
	}
	if _, ok := FoldOverseer(filepath.Join(t.TempDir(), "events.jsonl"), overseerMe, 0); ok {
		t.Error("a missing log folded")
	}
}

func TestFoldOverseerKeepsLogOrderOnEqualTimestamps(t *testing.T) {
	a := overseerMsg(3000, "c2", "dispatcher:c2", overseerMe, "a")
	b := overseerMsg(3000, "c1", "dispatcher:c1", overseerMe, "b")
	got, ok := foldOverseer(t, 0, a, b)
	if !ok || testjson.Compact(got.Batch) != batchOf(t, 3000, a, b) {
		t.Errorf("batch = %s, want %s", testjson.Compact(got.Batch), batchOf(t, 3000, a, b))
	}
}

// A non-object row on the log must not throw the fold, or one bad append would
// stop the overseer reading that bus.
func TestFoldOverseerSkipsNonObjectRows(t *testing.T) {
	row := overseerMsg(4000, "c1", "dispatcher:c1", overseerMe, "x")
	got, ok := foldOverseer(t, 0, `7`, `"text"`, row)
	if !ok || got.Cursor != 4000 {
		t.Errorf("fold = %+v, %v; want the one msg", got, ok)
	}
}

func TestFoldOverseerEscapesInvisibleText(t *testing.T) {
	got, ok := foldOverseer(t, 0, overseerMsg(4000, "c1", "dispatcher:c1", overseerMe, "a\u200bb"))
	if !ok {
		t.Fatal("fold found nothing")
	}
	if b := testjson.Compact(got.Batch); strings.Contains(b, "\u200b") {
		t.Errorf("batch %s carries a raw zero-width space", b)
	}
}

func TestReadCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor")
	if _, ok := ReadCursor(path); ok {
		t.Error("a missing cursor read")
	}
	for _, text := range []string{"", "\n", "12x\n", "-5\n", " 12\n"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if ts, ok := ReadCursor(path); ok {
			t.Errorf("cursor %q read as %d", text, ts)
		}
	}
	if err := os.WriteFile(path, []byte("1785951264000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ts, ok := ReadCursor(path); !ok || ts != 1785951264000 {
		t.Errorf("cursor = %d, %v", ts, ok)
	}
}

func TestWriteCursorCreatesItsDirAndLeavesNoTemp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "overseer", "buses", "0123456789abcdef")
	path := filepath.Join(dir, "cursor")
	for _, ts := range []int64{1000, 2000} {
		if err := WriteCursor(path, ts); err != nil {
			t.Fatal(err)
		}
		if got, ok := ReadCursor(path); !ok || got != ts {
			t.Errorf("cursor = %d, %v; want %d", got, ok, ts)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dir holds %v, want the cursor alone", entries)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteCursor(filepath.Join(blocker, "cursor"), 1); err == nil {
		t.Error("a cursor under a file wrote")
	}
}
