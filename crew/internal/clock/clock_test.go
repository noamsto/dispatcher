package clock

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// now is 1700000000, the second the hold fixtures stamp their clock file with.
var now = time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)

func compact(v jsonv.Value) string { return string(jsonv.Append(nil, v, jsonv.Options{})) }

func clockIn(t *testing.T, text string) Clock {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clock")
	if text != "" {
		if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Clock{Now: func() time.Time { return now }, CrewClock: path}
}

// `_clock_now` seeds an unset clock file from the real time, and the file it
// seeds is the one every later read uses.
func TestTextSeedsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clock")
	c := Clock{Now: func() time.Time { return now }, CrewClock: path}
	if got := c.Text(); got != "1700000000" {
		t.Errorf("Text = %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "1700000000\n" {
		t.Errorf("seeded %q (%v)", data, err)
	}
	if got := c.Text(); got != "1700000000" {
		t.Errorf("re-read = %q", got)
	}
}

// With no CREW_CLOCK the clock is the real one at whole-second precision.
func TestTextWithoutCrewClock(t *testing.T) {
	c := Clock{Now: func() time.Time { return now.Add(999 * time.Millisecond) }}
	if got := c.Text(); got != "1700000000" {
		t.Errorf("Text = %q", got)
	}
}

// A clock file holding anything bash's arithmetic would reject reads as 0, and
// NowMS follows it to 0 rather than to the real time.
func TestSecondsOnUnusableFile(t *testing.T) {
	for _, text := range []string{"not-a-clock", "1.5"} {
		c := clockIn(t, text)
		if got := c.Seconds(); got != 0 {
			t.Errorf("Seconds(%q) = %d", text, got)
		}
		if got := c.NowMS(); got != 0 {
			t.Errorf("NowMS(%q) = %d", text, got)
		}
	}
}

func TestNowMS(t *testing.T) {
	if got := (Clock{Now: func() time.Time { return now.Add(999500 * time.Microsecond) }}).NowMS(); got != 1700000000999 {
		t.Errorf("real NowMS = %d", got)
	}
	if got := clockIn(t, "1800000000").NowMS(); got != 1800000000000 {
		t.Errorf("virtual NowMS = %d", got)
	}
}

// The bus stamps real milliseconds and `_clock_now_f` hands jq real seconds, so
// neither may lose the precision the other has: floor for the stamp, fraction
// for the compare a hold maturing inside the current second depends on.
func TestRealMSAndNowFPrecision(t *testing.T) {
	c := Clock{Now: func() time.Time { return now.Add(500 * time.Microsecond) }}
	if got := c.RealMS(); got != 1700000000000 {
		t.Errorf("RealMS = %d", got)
	}
	if got := compact(c.NowF()); got != "1700000000.0005" {
		t.Errorf("NowF = %s", got)
	}
	if got := compact(clockIn(t, "1800000000").NowF()); got != "1800000000" {
		t.Errorf("virtual NowF = %s", got)
	}
}

// `_clock_sleep` under CREW_CLOCK: the integer part, a fraction rounded up, and
// anything bash reads as an unset variable advancing nothing.
func TestSleepAdvancesTheClock(t *testing.T) {
	cases := []struct {
		interval string
		want     int64
	}{
		{"2", 1800000002},
		{"0", 1800000000},
		{"0.5", 1800000001},
		{".5", 1800000001},
		{"1.9", 1800000002},
		{"-1.5", 1800000000},
		{"abc", 1800000000},
		{"abc.5", 1800000001},
	}
	for _, tc := range cases {
		c := clockIn(t, "1800000000")
		if err := c.Sleep(tc.interval, nil); err != nil {
			t.Fatalf("Sleep(%q): %v", tc.interval, err)
		}
		if got := c.Seconds(); got != tc.want {
			t.Errorf("Sleep(%q): clock = %d, want %d", tc.interval, got, tc.want)
		}
	}
}

// The advance is the helper's tmp file plus rename, so a concurrent reader never
// sees a half-written clock and nothing is left behind.
func TestSleepWritesThroughATmpFile(t *testing.T) {
	c := clockIn(t, "1800000000")
	if err := c.Sleep("3", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(c.CrewClock))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "clock" {
		t.Errorf("clock dir = %v", entries)
	}
}

// The virtual poll keeps the arm's rate: the clock advances by the interval and
// the poll still costs about what the arm's jq process start cost, so a
// CREW_CLOCK wait never reads the bus in a hot loop.
func TestSleepUnderCrewClockPacesThePoll(t *testing.T) {
	c := clockIn(t, "1800000000")
	start := time.Now()
	if err := c.Sleep("2", nil); err != nil {
		t.Fatal(err)
	}
	if got := time.Since(start); got < virtualPollFloor {
		t.Errorf("poll took %v, want at least %v", got, virtualPollFloor)
	}
	if got := c.Seconds(); got != 1800000002 {
		t.Errorf("clock = %d", got)
	}
}

// With no CREW_CLOCK it is the real `sleep`, so a value GNU sleep rejects costs
// its own message and an error the caller exits 1 on, exactly as `set -e` made
// the arm do.
func TestSleepWithoutCrewClock(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep on PATH")
	}
	c := Clock{Now: func() time.Time { return now }}
	var errB bytes.Buffer
	if err := c.Sleep("0.01", &errB); err != nil {
		t.Errorf("Sleep(0.01): %v", err)
	}
	errB.Reset()
	if err := c.Sleep("not-an-interval", &errB); err == nil {
		t.Error("Sleep(not-an-interval) = nil")
	}
	if !strings.Contains(errB.String(), "not-an-interval") {
		t.Errorf("stderr = %q", errB.String())
	}
}
