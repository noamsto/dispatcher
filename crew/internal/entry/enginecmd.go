package entry

import (
	"io"

	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// engineCmdUsage is the arm's `[ -n "${1:-}" ] ||` refusal.
const engineCmdUsage = "crew: engine-cmd <pane_current_command>\n"

// RunEngineCmd is crew.sh's `engine-cmd` arm: whether tmux's
// `#{pane_current_command}` names an engine. It prints nothing on success; exit
// 0 is the answer. The table is roster's, pinned against `_is_engine_cmd` by
// TestEngineCommandsMatchCrewSh.
func RunEngineCmd(args []string, stderr io.Writer) int {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if cmd == "" {
		say(stderr, "%s", engineCmdUsage)
		return exitFailure
	}
	if roster.IsEngineCmd(cmd) {
		return 0
	}
	return exitFailure
}
