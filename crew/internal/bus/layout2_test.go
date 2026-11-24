package bus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeStateFile writes content to path, creating its parent dirs.
func writeStateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, content)
}

func TestStateRoot(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"xdg wins", map[string]string{"XDG_STATE_HOME": "/x", "HOME": "/h"}, "/x/crew"},
		{"home fallback", map[string]string{"HOME": "/h"}, "/h/.local/state/crew"},
		{"empty xdg falls back", map[string]string{"XDG_STATE_HOME": "", "HOME": "/h"}, "/h/.local/state/crew"},
		{"neither", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StateRoot(func(k string) string { return c.env[k] }); got != c.want {
				t.Fatalf("StateRoot = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStoreAndArchiveDir(t *testing.T) {
	if got := StoreDir("/s/crew", "c1"); got != "/s/crew/crews/c1" {
		t.Fatalf("StoreDir = %q", got)
	}
	if got := ArchiveDir("/s/crew", "c1"); got != "/s/crew/archive/c1" {
		t.Fatalf("ArchiveDir = %q", got)
	}
}

func TestMachineLogPath(t *testing.T) {
	east := time.FixedZone("UTC+2", 2*3600)
	west := time.FixedZone("UTC-5", -5*3600)
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), "/s/crew/machine/2026-11.jsonl"},
		{time.Date(2026, 12, 1, 1, 0, 0, 0, east), "/s/crew/machine/2026-11.jsonl"},
		{time.Date(2026, 12, 31, 20, 0, 0, 0, west), "/s/crew/machine/2027-01.jsonl"},
	}
	for _, c := range cases {
		if got := MachineLogPath("/s/crew", c.t); got != c.want {
			t.Errorf("MachineLogPath(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestIsRefusal(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&ArchivedError{ID: "a"}, true},
		{&IndexOnlyError{ID: "a"}, true},
		{fmt.Errorf("wrapped: %w", &ArchivedError{ID: "a"}), true},
		{&InvalidCrewIDError{ID: "a/b"}, false},
		{ErrNotRepo, false},
		{errors.New("other"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsRefusal(c.err); got != c.want {
			t.Errorf("IsRefusal(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
