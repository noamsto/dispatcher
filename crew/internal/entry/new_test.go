package entry

import (
	"bytes"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
)

func TestNewPrintsTimestampPid(t *testing.T) {
	var stdout bytes.Buffer
	if code := RunNew(&stdout, NewOptions{Clock: clock.Clock{Now: func() time.Time { return time.Unix(1720800000, 0) }}, PID: 4242}); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	if stdout.String() != "1720800000-4242\n" {
		t.Fatalf("stdout %q, want the timestamp-pid the arm printed", stdout.String())
	}
}

// TestNewIsTheArmsShape pins what the bats rows pin: every id matches
// ^[0-9]+-[0-9]+$, whatever the clock and the pid are.
func TestNewIsTheArmsShape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9]+-[0-9]+$`)
	for _, now := range []time.Time{time.Unix(1720800000, 999), time.Unix(0, 0), time.Unix(4070908800, 0)} {
		var stdout bytes.Buffer
		if code := RunNew(&stdout, NewOptions{Clock: clock.Clock{Now: func() time.Time { return now }}, PID: os.Getpid()}); code != 0 {
			t.Fatalf("code %d, want 0", code)
		}
		line := stdout.String()
		if line[len(line)-1] != '\n' {
			t.Fatalf("stdout %q is no line", line)
		}
		if got := line[:len(line)-1]; !re.MatchString(got) {
			t.Errorf("new printed %q, which is no timestamp-pid", got)
		}
	}
}
