package fleet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/lock"
)

func TestWriteOKKeepsMax(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hosts", "mbp")
	for _, ts := range []int64{200, 100} {
		if err := WriteOK(dir, ts); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := ReadOK(dir); !ok || got != 200 {
		t.Errorf("ok = %d/%v, want 200", got, ok)
	}
	if err := WriteOK(dir, 300); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadOK(dir); got != 300 {
		t.Errorf("ok = %d, want 300", got)
	}
}

func TestWriteOKLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteOK(dir, 5); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "ok" && e.Name() != "ok.lock.d" {
			t.Errorf("stray entry %q", e.Name())
		}
	}
}

func TestWriteOKSkipsWhenLockHeld(t *testing.T) {
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "ok.lock.d")
	if !lock.Acquire(lockDir, "someone-else") {
		t.Fatal("setup lock refused")
	}
	t.Cleanup(func() { lock.Release(lockDir) })
	if err := WriteOK(dir, 5); err != nil {
		t.Fatalf("WriteOK = %v, want nil on a held lock", err)
	}
	if _, ok := ReadOK(dir); ok {
		t.Error("ok written despite the held lock")
	}
}
