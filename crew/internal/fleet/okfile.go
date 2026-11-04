package fleet

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

// ReadOK is the last-ok ms recorded in dir's ok file, false when it is
// missing or not all digits.
func ReadOK(dir string) (int64, bool) {
	return watch.ReadCursor(filepath.Join(dir, "ok"))
}

// WriteOK records ts as dir's last-ok ms, keeping the larger of it and what is
// there. The lock refuses rather than blocks, and a refused write is skipped:
// the next successful fetch writes a newer ts.
func WriteOK(dir string, ts int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lockDir := filepath.Join(dir, "ok.lock.d")
	if !lock.Acquire(lockDir, "ok-"+strconv.Itoa(os.Getpid())) {
		return nil
	}
	defer lock.Release(lockDir)
	if cur, ok := ReadOK(dir); ok && cur >= ts {
		return nil
	}
	return watch.WriteCursor(filepath.Join(dir, "ok"), ts)
}
