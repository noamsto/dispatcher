package watch

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
)

var errStop = errors.New("stop the poll")

// relayRun drives Relay on a stubbed clock. steps[i] runs inside the i-th
// Sleep, which is how a test lands a row between two polls; once they are spent
// the next Sleep ends the loop, so Relay returns 1 with everything it printed.
func relayRun(t *testing.T, p bus.Paths, req RelayRequest, steps ...func()) (string, int, *park) {
	t.Helper()
	pk := &park{now: base}
	n := 0
	var out bytes.Buffer
	code := Relay(req, p, &out, io.Discard, Options{
		CrewID: func() string { return "c1" },
		Clock:  clock.Clock{Now: pk.Now},
		Sleep: func(interval string, w io.Writer) error {
			if n >= len(steps) {
				return errStop
			}
			steps[n]()
			n++
			return pk.Sleep(interval, w)
		},
	})
	return out.String(), code, pk
}

func appendRow(t *testing.T, p bus.Paths, row string) func() {
	return func() {
		t.Helper()
		log, err := os.ReadFile(p.Log)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.Log, append(log, row+"\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRelayBatchMatchesWatch(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951264000, "worker:feat/x#s1-1", "blocked", "need input", ""))

	watched, _, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 || watched == "" {
		t.Fatalf("watch: code %d stdout %q", code, watched)
	}
	got, code, _ := relayRun(t, p, RelayRequest{Since: since("0")})
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	if got != watched {
		t.Errorf("relay = %s, watch = %s", got, watched)
	}
}

func TestRelayAdvancesItsCursorInMemory(t *testing.T) {
	p := fixture(t)
	first := status(1785951264000, "worker:feat/x#s1-1", "blocked", "one", "")
	second := status(1785951265000, "worker:feat/x#s1-1", "blocked", "two", "")
	writeLog(t, p, first)

	got, _, _ := relayRun(t, p, RelayRequest{Since: since("0")}, appendRow(t, p, second))
	if want := want(t, 1785951264000, first) + want(t, 1785951265000, second); got != want {
		t.Errorf("relay = %s, want %s", got, want)
	}
}

func TestRelayHeartbeatsWhenIdle(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951264000, "worker:feat/x#s1-1", "working", "", ""))

	steps := make([]func(), 16)
	for i := range steps {
		steps[i] = func() {}
	}
	got, _, pk := relayRun(t, p, RelayRequest{Since: since("0"), Heartbeat: "30"}, steps...)
	if want := `{"relay":"heartbeat","ts":` + itoa(base.UnixMilli()+30_000) + "}\n"; got != want {
		t.Errorf("relay = %q, want %q", got, want)
	}
	if len(pk.intervals) == 0 || pk.intervals[0] != "2" {
		t.Errorf("intervals = %v, want the watch default 2", pk.intervals)
	}
}

type epipeWriter struct{ writes int }

func (w *epipeWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, syscall.EPIPE
}

func TestRelayEndsOnAWriteError(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951264000, "worker:feat/x#s1-1", "done", "", ""))
	w := &epipeWriter{}
	code := Relay(RelayRequest{Since: since("0")}, p, w, io.Discard, Options{
		CrewID: func() string { return "c1" },
		Clock:  clock.Clock{Now: (&park{now: base}).Now},
		Sleep:  func(string, io.Writer) error { t.Fatal("polled on after the pipe closed"); return nil },
	})
	if code != 0 || w.writes != 1 {
		t.Errorf("code %d after %d writes, want 0 after 1", code, w.writes)
	}
}

func TestRelayLeavesNoLockOrCursor(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(1785951264000, "worker:feat/x#s1-1", "done", "", ""))
	relayRun(t, p, RelayRequest{Since: since("0")})

	_ = filepath.WalkDir(p.Dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && (d.Name() == "watch.lock.d" || d.Name() == "cursor" || strings.HasPrefix(d.Name(), ".cursor")) {
			t.Errorf("relay left %s", path)
		}
		return nil
	})
}

func TestRelayRefusesWhatWatchRefuses(t *testing.T) {
	p := fixture(t)
	var errB bytes.Buffer
	code := Relay(RelayRequest{Since: since("x")}, p, io.Discard, &errB, Options{CrewID: func() string { return "c1" }})
	if code != 1 || errB.String() != "crew: --since must be an integer ms timestamp\n" {
		t.Errorf("code %d stderr %q", code, errB.String())
	}
}
