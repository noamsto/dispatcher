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

	"github.com/noamsto/dispatcher/crew/internal/proc"
)

var paneIDRE = regexp.MustCompile(`^%[0-9]+$`)

// paneStates are the turn states tmux-og's hooks write. interrupted is not
// one: tmux-og derives it when it reads a long-quiet processing pane.
var paneStates = []string{"processing", "waiting", "done", "idle", "compacting", "denied", "error"}

// paneStateCap bounds the read of a state file the dispatcher's own user
// writes but this reporter does not otherwise trust.
const paneStateCap = 4 << 10

// readPaneState is the tmux-og hook state of pane, nil when it cannot be
// vouched for. The status dir must be a real directory owned by this user and
// closed to others (tmux-og's ownerdir.OwnerOnly rule): a planted state file
// would otherwise steer the operator's attention. It never creates the dir.
//
// The status dir is shared by every tmux server of this uid, so a pane id alone
// does not say whose file it is. A file's agent_pid line must name a live
// process in the dispatcher's tree (the dispatcher pid is its launcher); present
// but dead, elsewhere or malformed, the file is another server's. A file
// without the line predates it and is taken as written.
func readPaneState(getenv func(string) string, pane string, procs proc.Table, dispatcher int) *string {
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
	var state *string
	var agent *string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "state="); ok && slices.Contains(paneStates, v) && state == nil {
			state = &v
		}
		if v, ok := strings.CutPrefix(line, "agent_pid="); ok && agent == nil {
			agent = &v
		}
	}
	if state == nil || agent != nil && !underDispatcher(procs, *agent, dispatcher) {
		return nil
	}
	return state
}

// underDispatcher is whether agent, an agent_pid value, is a positive pid that
// is alive and the dispatcher's own or its descendant.
func underDispatcher(procs proc.Table, agent string, dispatcher int) bool {
	pid, err := strconv.Atoi(agent)
	if err != nil || pid <= 0 || dispatcher <= 0 || !procs.Alive(pid) {
		return false
	}
	return pid == dispatcher || proc.AncestorsOf(procs, pid)[dispatcher]
}
