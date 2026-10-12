package entry

import (
	"bytes"
	"os"
	"regexp"
	"testing"
	"time"
)

func TestNewPrintsTimestampPid(t *testing.T) {
	var stdout bytes.Buffer
	if code := RunNew(&stdout, NewOptions{Now: func() time.Time { return time.Unix(1720800000, 0) }, PID: 4242}); code != 0 {
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
		if code := RunNew(&stdout, NewOptions{Now: func() time.Time { return now }, PID: os.Getpid()}); code != 0 {
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

// TestNewPIDsDiffer: uniqueness comes from the pid, so two mints in the same
// second differ — the reason the arm printed `$$` and this prints its own pid,
// which is the same pid because crew.sh execs this binary.
func TestNewPIDsDiffer(t *testing.T) {
	now := time.Unix(1720800000, 0)
	ids := map[string]bool{}
	for _, pid := range []int{1, 2, os.Getpid()} {
		var stdout bytes.Buffer
		RunNew(&stdout, NewOptions{Now: func() time.Time { return now }, PID: pid})
		if ids[stdout.String()] {
			t.Fatalf("two mints printed the same id %q", stdout.String())
		}
		ids[stdout.String()] = true
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %v", ids)
	}
}
