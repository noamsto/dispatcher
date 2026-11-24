package bus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StateRoot is ${XDG_STATE_HOME:-$HOME/.local/state}/crew, "" when neither is
// set: layout-2 lookups are then skipped, never an error.
func StateRoot(getenv func(string) string) string {
	if state := getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "crew")
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".local/state", "crew")
	}
	return ""
}

// StoreDir is a layout-2 crew's dir: its log, meta.json and crew-scoped stores.
// Validate untrusted ids with ValidCrewID first.
func StoreDir(state, id string) string { return state + "/crews/" + id }

// ArchiveDir is where an ended layout-2 crew is kept.
func ArchiveDir(state, id string) string { return state + "/archive/" + id }

// MachineLogPath is the machine log segment for t's UTC month, for rows no
// crew resolves; it is written with Append.
func MachineLogPath(state string, t time.Time) string {
	return state + "/machine/" + t.UTC().Format("2006-01") + ".jsonl"
}

func metaPath(dir string) string { return dir + "/meta.json" }

// isFile is the hooks' `[ -f ]`: stat follows symlinks and must find a regular file.
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// ArchivedError refuses an id whose crew has ended and been archived.
type ArchivedError struct{ ID string }

func (e *ArchivedError) Error() string { return fmt.Sprintf("crew %s is archived", e.ID) }

// IndexOnlyError refuses an id a repo index lists although its layout-2 dir is
// gone, so it is neither resumed under nor silently recreated as layout 1.
type IndexOnlyError struct {
	ID   string
	Live []string
}

func (e *IndexOnlyError) Error() string {
	live := "none"
	if len(e.Live) > 0 {
		live = strings.Join(e.Live, ", ")
	}
	return fmt.Sprintf("crew %s is listed in the repo index but has no crew dir — run 'crew index rebuild'; live crews: %s", e.ID, live)
}

// IsRefusal reports whether err is an *ArchivedError or *IndexOnlyError.
func IsRefusal(err error) bool {
	var archived *ArchivedError
	var indexOnly *IndexOnlyError
	return errors.As(err, &archived) || errors.As(err, &indexOnly)
}
