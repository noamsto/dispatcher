package remote

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

func tempBus(t *testing.T, lines ...string) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "events.jsonl")
	for _, l := range lines {
		if err := bus.Append(log, l); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

func TestDispatchRow(t *testing.T) {
	log := tempBus(t,
		`{"ts":1,"kind":"dispatch","crew_id":"c","branch":"feat/a","session":"s1-1","worker_id":"worker:feat/a#s1-1"}`,
		`{"ts":2,"kind":"status","crew_id":"c","branch":"feat/a","session":"s1-1"}`,
		`{"ts":3,"kind":"dispatch","crew_id":"d","branch":"feat/d","session":"s1-1"}`,
		`{"ts":4,"kind":"dispatch","crew_id":"c","branch":"feat/b","session":"s2-2"}`,
		`{"torn":`,
		`{"ts": 5, "kind":"dispatch","crew_id":"c","branch":"feat/a2","session":"s1-1","body":{"z":1,"a":[1, 2]}}`,
		`5`,
		`{"ts":6,"kind":"dispatch","crew_id":"c","session":7}`,
	)
	var stdout, stderr bytes.Buffer
	if rc := DispatchRow(log, "c", "s1-1", &stdout, &stderr); rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, stderr.String())
	}
	if want := `{"ts":5,"kind":"dispatch","crew_id":"c","branch":"feat/a2","session":"s1-1","body":{"z":1,"a":[1,2]}}` + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestDispatchRowNone(t *testing.T) {
	log := tempBus(t, `{"ts":1,"kind":"dispatch","crew_id":"c","branch":"feat/a","session":"s1-1"}`)
	for _, tc := range []struct{ log, crew, session string }{
		{log, "c", "s9-9"},
		{log, "d", "s1-1"},
		{filepath.Join(t.TempDir(), "missing.jsonl"), "c", "s1-1"},
	} {
		var stdout, stderr bytes.Buffer
		if rc := DispatchRow(tc.log, tc.crew, tc.session, &stdout, &stderr); rc != 1 || stdout.Len() != 0 {
			t.Errorf("%+v: rc %d, stdout %q", tc, rc, stdout.String())
		}
	}
}

func TestDispatchRowValidates(t *testing.T) {
	log := tempBus(t, `{"ts":1,"kind":"dispatch","crew_id":"c","session":"s1-1"}`)
	for _, tc := range []struct{ crew, session string }{
		{"c", "s1"},
		{"c", "s1-1\n"},
		{"c", "x1-1"},
		{"", "s1-1"},
		{"-c", "s1-1"},
	} {
		var stdout, stderr bytes.Buffer
		if rc := DispatchRow(log, tc.crew, tc.session, &stdout, &stderr); rc != 2 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "crew: dispatch-row: ") {
			t.Errorf("%+v: rc %d, stdout %q, stderr %q", tc, rc, stdout.String(), stderr.String())
		}
	}
}

func TestDispatchRowUnreadable(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(log, []byte(`{"kind":"dispatch","crew_id":"c","session":"s1-1"}`+"\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	var stdout, stderr bytes.Buffer
	if rc := DispatchRow(log, "c", "s1-1", &stdout, &stderr); rc == 0 || rc == 1 || stderr.Len() == 0 {
		t.Fatalf("rc %d, stderr %q: an unreadable bus must not read as no row", rc, stderr.String())
	}
}
