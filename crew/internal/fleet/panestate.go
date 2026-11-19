package fleet

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

var paneIDRE = regexp.MustCompile(`^%[0-9]+$`)

// paneStates are the turn states tmux-og's hooks write.
var paneStates = []string{"processing", "waiting", "done", "idle", "compacting", "denied", "error", "interrupted"}

// paneStateCap bounds the read of a state file the dispatcher's own user
// writes but this reporter does not otherwise trust.
const paneStateCap = 4 << 10

// readPaneState is the tmux-og hook state of pane, nil when it cannot be
// vouched for. The status dir must be a real directory owned by this user and
// closed to others (tmux-og's ownerdir.OwnerOnly rule): a planted state file
// would otherwise steer the operator's attention. It never creates the dir.
func readPaneState(getenv func(string) string, pane string) *string {
	if !paneIDRE.MatchString(pane) {
		return nil
	}
	dir := getenv("CLAUDE_STATUS_DIR")
	if dir == "" {
		dir = "/tmp/claude-status-" + strconv.Itoa(os.Getuid())
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return nil
	}
	path := filepath.Join(dir, "panes", strings.TrimPrefix(pane, "%"))
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, paneStateCap))
	if err != nil {
		return nil
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "state="); ok && slices.Contains(paneStates, v) {
			return &v
		}
	}
	return nil
}
