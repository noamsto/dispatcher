package data

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestReadEventsTailThenResume(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, p, `{"ts":1,"kind":"a"}`+"\n"+`{"ts":2,"kind":"b"}`+"\n"+`{"ts":3,"kind":"c"}`+"\n")
	page, err := ReadEvents(p, "", 2)
	if err != nil || len(page.Events) != 2 || page.Events[0].Ev["kind"] != "b" {
		t.Fatalf("tail = %+v, %v", page, err)
	}
	idB := page.Events[0].ID
	if idB != EventID([]byte(`{"ts":2,"kind":"b"}`)) {
		t.Errorf("id = %q is not the line hash", idB)
	}
	// A torn trailing line is not consumed and not lost.
	appendFile(t, p, `{"ts":4,"ki`)
	empty, err := ReadEvents(p, page.Cursor, 0)
	if err != nil || len(empty.Events) != 0 || empty.Cursor != page.Cursor {
		t.Fatalf("torn = %+v, %v", empty, err)
	}
	appendFile(t, p, `nd":"d"}`+"\n"+`{"ts":5,"kind":"e"}`+"\n")
	next, err := ReadEvents(p, page.Cursor, 0)
	if err != nil || len(next.Events) != 2 || next.Events[0].Ev["kind"] != "d" || next.Reset {
		t.Fatalf("next = %+v, %v", next, err)
	}
	again, _ := ReadEvents(p, next.Cursor, 0)
	if len(again.Events) != 0 {
		t.Errorf("replayed %d rows", len(again.Events))
	}
}

func TestReadEventsLimitMoreAndNoGaps(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, p, `{"n":1}`+"\n"+`{"n":2}`+"\n"+`{"n":3}`+"\n")
	page, _ := ReadEvents(p, "", 100)
	cur, got := page.Cursor, []float64{}
	appendFile(t, p, `{"n":4}`+"\n"+`{"n":5}`+"\n"+`{"n":6}`+"\n")
	for i := 0; i < 5; i++ {
		pg, err := ReadEvents(p, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range pg.Events {
			got = append(got, e.Ev["n"].(float64))
		}
		cur = pg.Cursor
		if !pg.More {
			break
		}
	}
	if len(got) != 3 || got[0] != 4 || got[2] != 6 {
		t.Errorf("rows = %v, want 4 5 6", got)
	}
}

func TestReadEventsResetOnRotateAndBadCursor(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	appendFile(t, p, `{"n":1}`+"\n"+`{"n":2}`+"\n")
	page, _ := ReadEvents(p, "", 10)
	if err := os.Rename(p, p+".old"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, `{"n":9}`+"\n")
	rot, err := ReadEvents(p, page.Cursor, 0)
	if err != nil || !rot.Reset || len(rot.Events) != 1 {
		t.Fatalf("rotated = %+v, %v", rot, err)
	}
	if _, err := ReadEvents(p, "junk", 0); err == nil {
		t.Error("bad cursor accepted")
	}
	missing, err := ReadEvents(filepath.Join(dir, "none"), "", 5)
	if err != nil || len(missing.Events) != 0 {
		t.Errorf("missing = %+v, %v", missing, err)
	}
}

func TestCrewRegistrationReadsPidAndPane(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(self)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pane"), []byte("%3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid, pane, alive := crewRegistration(context.Background(), nil, Config{}, root, "c1")
	if pid == nil || *pid != self || pane == nil || *pane != "%3" || alive == nil || !*alive {
		t.Fatalf("got %v %v %v", pid, pane, alive)
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte("2147483646\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, alive := crewRegistration(context.Background(), nil, Config{}, root, "c1"); alive == nil || *alive {
		t.Errorf("dead pid alive = %v", alive)
	}
}
