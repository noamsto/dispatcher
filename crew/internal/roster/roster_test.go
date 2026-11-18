package roster

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const localModelsScript = "../../../adapters/core/local-models.sh"

// now is fixed, so a row at ts 1700000000000 is 100s old.
const now = 1700000100.0

const t0 = 1700000000000

// status is a worker status event of crew c1.
func status(ts int64, from, body string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"status","from":%q,"to":"dispatcher:c1","body":%s}`, ts, from, body)
}

func state(s string) string { return fmt.Sprintf(`{"state":%q}`, s) }

func dispatch(ts int64, fields string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch",%s}`, ts, fields)
}

func worktree(path, branch string) string {
	return fmt.Sprintf("worktree %s\nHEAD 0123abc\nbranch refs/heads/%s\n\n", path, branch)
}

// rows and expectations below were produced by running the arm in crew.sh
// under jq 1.8.2 with `now` pinned, a stubbed tmux and a stubbed
// `git worktree list`, then compacting the output.
type rosterCase struct {
	name      string
	crew      string // "c1" when empty
	events    []string
	panes     string
	worktrees string
	wtErr     *git.ExitError
	want      string
	wantErr   string // "type" or "exit"
	wantWT    int
}

var cases = []rosterCase{
	{
		name: "recorded codename without a suffix",
		events: []string{
			dispatch(1, `"branch":"feat/33-thing","name":"sage","color":"green","tmux":"colour28"`),
			dispatch(2, `"branch":"feat/38-thing","name":"atlas","color":"blue","tmux":"colour32"`),
			status(t0+1000, "worker:feat/33-thing#s1-1", state("working")),
			status(t0+2000, "worker:feat/38-thing#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/33-thing#s1-1","branch":"feat/33-thing","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage","color":"green","tmux":"colour28"},{"from":"worker:feat/38-thing#s1-1","branch":"feat/38-thing","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":98}],"name":"atlas","color":"blue","tmux":"colour32"}]`,
	},
	{
		name: "sessions of one branch collapse into one row",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+2000, "worker:feat/x#s1-1", state("done")),
			status(t0+3000, "worker:feat/x#s2-2", state("working")),
		},
		want: `[{"from":"worker:feat/x#s2-2","branch":"feat/x","session":"s2-2","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000003000,"pr_url":null,"age_s":97,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"done","age_s":98},{"session":"s2-2","state":"working","age_s":97}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "engine_session from the dispatch event",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine_session":"aaaa"}`,
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":"aaaa","sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "engine_session from a newer resume event",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine_session":"aaaa"}`,
			`{"ts":9999999999999,"crew_id":"c1","kind":"resume","branch":"feat/x","engine_session":"bbbb"}`,
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":"bbbb","sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "engine_session from a newer relaunch event",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine_session":"aaaa"}`,
			`{"ts":9999999999999,"crew_id":"c1","kind":"relaunch","branch":"feat/x","engine_session":"cccc"}`,
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":"cccc","sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:   "codename derives from the branch hash",
		events: []string{status(t0+1000, "worker:feat/x#s1-1", state("working"))},
		want:   `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "one session's exited is not resolved from another's history",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+2000, "worker:feat/x#s2-2", state("exited")),
		},
		worktrees: worktree("/wt/other", "main"),
		want:      `[{"from":"worker:feat/x#s2-2","branch":"feat/x","session":"s2-2","state":"exited","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99},{"session":"s2-2","state":"exited","age_s":98}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
		wantWT:    1,
	},
	{
		name: "false exited with a live wrapped pane resolves to the previous state",
		events: []string{
			status(t0+1000, "worker:feat/roster-live#s1-1", state("working")),
			status(t0+2000, "worker:feat/roster-live#s1-1", state("exited")),
		},
		panes:     "fish /somewhere\n.claude-wrapped /wt/live\n",
		worktrees: worktree("/wt/other", "main") + worktree("/wt/live", "feat/roster-live"),
		want:      `[{"from":"worker:feat/roster-live#s1-1","branch":"feat/roster-live","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"prev_state":"working","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"ash","color":"dimgrey","tmux":"colour102"}]`,
		wantWT:    1,
	},
	{
		name: "exited with no previous state resolves to working",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("exited")),
		},
		panes:     "node /wt/x\n",
		worktrees: worktree("/wt/x", "feat/x"),
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"prev_state":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
		wantWT:    1,
	},
	{
		name: "exited session with no engine pane stays exited",
		events: []string{
			status(t0+1000, "worker:feat/roster-dead#s1-1", state("working")),
			status(t0+2000, "worker:feat/roster-dead#s1-1", state("exited")),
		},
		panes:     "fish /wt/dead\n",
		worktrees: worktree("/wt/dead", "feat/roster-dead"),
		want:      `[{"from":"worker:feat/roster-dead#s1-1","branch":"feat/roster-dead","session":"s1-1","state":"exited","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"name":"coral","color":"salmon","tmux":"colour167"}]`,
		wantWT:    1,
	},
	{
		name: "live engine at a sibling path does not resolve the exited row",
		events: []string{
			status(t0+1000, "worker:feat/roster-sib#s1-1", state("working")),
			status(t0+2000, "worker:feat/roster-sib#s1-1", state("exited")),
		},
		panes:     ".claude-wrapped /wt/sib-sibling\nclaude /wt/si\n",
		worktrees: worktree("/wt/sib", "feat/roster-sib"),
		want:      `[{"from":"worker:feat/roster-sib#s1-1","branch":"feat/roster-sib","session":"s1-1","state":"exited","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"name":"atlas","color":"blue","tmux":"colour32"}]`,
		wantWT:    1,
	},
	{
		name: "exited with no worktree for the branch stays exited",
		events: []string{
			status(t0+1000, "worker:feat/gone#s1-1", state("exited")),
		},
		panes:     "claude /wt/gone\n",
		worktrees: worktree("/wt/x", "feat/x"),
		want:      `[{"from":"worker:feat/gone#s1-1","branch":"feat/gone","session":"s1-1","state":"exited","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"name":"khaki","color":"darkkhaki","tmux":"colour101"}]`,
		wantWT:    1,
	},
	{
		name: "two worktrees on one branch never equal a pane path",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+2000, "worker:feat/x#s1-1", state("exited")),
		},
		panes:     "claude /wt/a\nclaude /wt/b\nclaude /wt/a\n/wt/b\n",
		worktrees: worktree("/wt/a", "feat/x") + worktree("/wt/b", "feat/x"),
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"exited","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
		wantWT:    1,
	},
	{
		name: "a worktree path is only its first whitespace field",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+2000, "worker:feat/x#s1-1", state("exited")),
		},
		panes:     "claude /wt/a\n",
		worktrees: worktree("/wt/a b", "feat/x"),
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"prev_state":"working","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
		wantWT:    1,
	},
	{
		name: "a pane row with no space is its own command and path",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+2000, "worker:feat/x#s1-1", state("exited")),
		},
		panes:     "\n\nnode\n",
		worktrees: worktree("node", "feat/x"),
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"prev_state":"working","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
		wantWT:    1,
	},
	{
		name: "trailing newlines on state and branch are dropped by the shell",
		events: []string{
			status(t0+1000, "worker:feat/x\n#s1-1", state("working")),
			status(t0+2000, "worker:feat/x\n#s1-1", `{"state":"exited\n"}`),
		},
		panes:     "pi /wt/x\n",
		worktrees: worktree("/wt/x", "feat/x"),
		want:      `[{"from":"worker:feat/x\n#s1-1","branch":"feat/x\n","session":"s1-1","state":"exited\n","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"prev_state":"exited\n","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited\n","age_s":98}],"exit_suspect":true}]`,
		wantWT:    1,
	},
	{
		name: "exited rows with an empty branch skip the worktree probe",
		events: []string{
			status(t0+1000, "worker:#s1-1", state("exited")),
		},
		panes:  "claude /wt/x\n",
		want:   `[{"from":"worker:#s1-1","branch":"","session":"s1-1","state":"exited","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}]}]`,
		wantWT: 0,
	},
	{
		name: "the worktree probe runs once per exited branch",
		events: []string{
			status(t0+1000, "worker:feat/a#s1-1", state("exited")),
			status(t0+1000, "worker:feat/b#s1-1", state("exited")),
			status(t0+1000, "worker:feat/c#s1-1", state("working")),
		},
		panes:     "claude /wt/b\n",
		worktrees: worktree("/wt/a", "feat/a") + worktree("/wt/b", "feat/b"),
		want:      `[{"from":"worker:feat/a#s1-1","branch":"feat/a","session":"s1-1","state":"exited","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b#s1-1","branch":"feat/b","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"prev_state":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"exit_suspect":true,"name":"cobalt","color":"royalblue","tmux":"colour68"},{"from":"worker:feat/c#s1-1","branch":"feat/c","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"ember","color":"orange","tmux":"colour130"}]`,
		wantWT:    2,
	},
	{
		name: "a resolved row keeps prev_state",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", `{"state":"blocked","detail":"q"}`),
			status(t0+2000, "worker:feat/x#s1-1", state("exited")),
		},
		panes:     "claude /wt/x\n",
		worktrees: worktree("/wt/x", "feat/x"),
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"blocked","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"prev_state":"blocked","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
		wantWT:    1,
	},
	{
		name: "title joins on the branch",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1","title":"Do a thing"}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":"Do a thing","engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "engine, model and tier join on the branch",
		events: []string{
			`{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1","engine":"claude","model":"sonnet","tier":"standard","title":"T"}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":"T","engine":"claude","model":"sonnet","tier":"standard","engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "the last dispatch event wins and false becomes null",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","title":"old","engine":"claude","model":"opus"}`,
			`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x","title":false,"engine":"codex","tier":0}`,
			`{"ts":3,"crew_id":"c2","kind":"dispatch","branch":"feat/x","title":"other crew","model":"haiku"}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":"codex","model":null,"tier":0,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:   "engine, model and tier default to null with no dispatch event",
		events: []string{status(t0+1000, "worker:feat/x", state("working"))},
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:   "a legacy branch-keyed row still renders",
		events: []string{status(t0+1000, "worker:feat/x", state("working"))},
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "a hash in the branch splits at the last hash",
		events: []string{
			status(t0+1000, "worker:a#b#s1-1", state("working")),
			status(t0+2000, "worker:a#b", state("working")),
			status(t0+3000, "worker:c#", state("working")),
		},
		want: `[{"from":"worker:a#b","branch":"a","session":"b","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"b","state":"working","age_s":98}],"name":"orchid","color":"orchid","tmux":"colour169"},{"from":"worker:a#b#s1-1","branch":"a#b","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"mauve","color":"palevioletred","tmux":"colour132"},{"from":"worker:c#","branch":"c","session":"","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000003000,"pr_url":null,"age_s":97,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"","state":"working","age_s":97}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "source is carried and a 200 char detail is cut to 120",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":"quiet: `+strings.Repeat("x", 200)+`","source":"watchdog"}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":"quiet: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","source":"watchdog","blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "detail is cut by codepoint, not byte",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":"`+strings.Repeat("é世🙂", 43)+`"}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":"é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂","source":null,"blocked_since":1700000001000,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "a worker-posted row has a null source",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":"which approach?"}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":"which approach?","source":null,"blocked_since":1700000001000,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "a suppressed re-stamp still refreshes age_s",
		events: []string{
			status(t0+100000-30000, "worker:feat/x#s1-1", `{"state":"blocked","detail":"need a waiver (cycle 7 of 24)"}`),
			status(t0+100000, "worker:feat/x#s1-1", `{"state":"blocked","detail":"need a waiver (cycle 8 of 24)"}`),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"blocked","detail":"need a waiver (cycle 8 of 24)","source":null,"blocked_since":1700000100000,"answered_at":null,"ts":1700000100000,"pr_url":null,"age_s":0,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"blocked","age_s":0}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "detail array is sliced to 120 elements",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":[`+strings.TrimSuffix(strings.Repeat(`1.0e2,`, 130), ",")+`]}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":[1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2],"source":null,"blocked_since":1700000001000,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "detail false and null are null",
		events: []string{
			status(t0+1000, "worker:feat/a", `{"state":"working","detail":false}`),
			status(t0+1000, "worker:feat/b", `{"state":"working","detail":null}`),
		},
		want: `[{"from":"worker:feat/a","branch":"feat/a","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b","branch":"feat/b","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"cobalt","color":"royalblue","tmux":"colour68"}]`,
	},
	{
		name:    "detail number is a type error",
		events:  []string{status(t0+1000, "worker:feat/x", `{"state":"working","detail":5}`)},
		wantErr: "type",
	},
	{
		name:    "detail true is a type error",
		events:  []string{status(t0+1000, "worker:feat/x", `{"state":"working","detail":true}`)},
		wantErr: "type",
	},
	{
		name:    "detail object is a type error",
		events:  []string{status(t0+1000, "worker:feat/x", `{"state":"working","detail":{"k":1}}`)},
		wantErr: "type",
	},
	{
		name: "an older event's detail is never sliced",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", `{"state":"working","detail":5}`),
			status(t0+2000, "worker:feat/x#s1-1", `{"state":"working","detail":"ok"}`),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":"ok","source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":98}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "pr_url is carried forward and false is kept",
		events: []string{
			status(t0+1000, "worker:feat/a#s1-1", `{"state":"pr_open","pr_url":"https://x/1"}`),
			status(t0+2000, "worker:feat/a#s1-1", state("done")),
			status(t0+1000, "worker:feat/b#s1-1", `{"state":"pr_open","pr_url":"https://x/2"}`),
			status(t0+2000, "worker:feat/b#s1-1", `{"state":"done","pr_url":false}`),
			status(t0+3000, "worker:feat/b#s1-1", `{"state":"done","pr_url":null}`),
		},
		want: `[{"from":"worker:feat/a#s1-1","branch":"feat/a","session":"s1-1","state":"done","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000002000,"pr_url":"https://x/1","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"done","age_s":98}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b#s1-1","branch":"feat/b","session":"s1-1","state":"done","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000003000,"pr_url":false,"age_s":97,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"done","age_s":97}],"name":"cobalt","color":"royalblue","tmux":"colour68"}]`,
	},
	{
		name: "ties on ts pick the last event",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+1000, "worker:feat/x#s1-1", state("blocked")),
			status(t0+1000, "worker:feat/x#s2-2", state("done")),
		},
		want: `[{"from":"worker:feat/x#s2-2","branch":"feat/x","session":"s2-2","state":"done","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"blocked","age_s":99},{"session":"s2-2","state":"done","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "age_s keeps the exponent form of a huge ts",
		events: []string{
			status(t0+1000, "worker:feat/a", state("working")),
			`{"ts":1e30,"crew_id":"c1","kind":"status","from":"worker:feat/b","body":{"state":"working"}}`,
			`{"ts":1.0e3,"crew_id":"c1","kind":"status","from":"worker:feat/c","body":{"state":"working"}}`,
		},
		want: `[{"from":"worker:feat/a","branch":"feat/a","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b","branch":"feat/b","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1E+30,"pr_url":null,"age_s":-1E+27,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":-1E+27}],"name":"cobalt","color":"royalblue","tmux":"colour68"},{"from":"worker:feat/c","branch":"feat/c","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1.0E+3,"pr_url":null,"age_s":1700000099,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":1700000099}],"name":"ember","color":"orange","tmux":"colour130"}]`,
	},
	{
		name: "other crews, other kinds and non-worker senders are ignored",
		events: []string{
			`{"ts":1,"crew_id":"c2","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`,
			`{"ts":1,"crew_id":"c1","kind":"msg","from":"worker:feat/x#s1-1","body":"hi"}`,
			`{"ts":1,"crew_id":"c1","kind":"status","from":"dispatcher:c1","body":{"state":"working"}}`,
			`{"ts":1,"crew_id":"c1","kind":"status","from":false,"body":{"state":"working"}}`,
			`{"ts":1,"crew_id":"c1","kind":"status","body":{"state":"working"}}`,
			`null`,
			`{}`,
		},
		want: `[]`,
	},
	{
		name:   "an empty crew selects the events whose crew_id is empty",
		crew:   "-",
		events: []string{`{"ts":1000,"crew_id":"","kind":"status","from":"worker:feat/x","body":{"state":"working"}}`, status(t0, "worker:feat/y", state("working"))},
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1000,"pr_url":null,"age_s":1700000099,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":1700000099}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:   "an empty bus is an empty roster",
		events: nil,
		want:   "[]",
	},
	{
		name: "colliding codenames get the issue token or the whole branch",
		events: []string{
			dispatch(1, `"branch":"feat/207-thing","name":"sage","color":"green","tmux":"colour28"`),
			dispatch(2, `"branch":"eng-6789-stuff","name":"sage","color":"green","tmux":"colour28"`),
			dispatch(3, `"branch":"wild","name":"sage","color":"green","tmux":"colour28"`),
			dispatch(4, `"branch":"docs/55-solo","name":"atlas","color":"blue","tmux":"colour32"`),
			status(t0+1000, "worker:feat/207-thing#s1-1", state("working")),
			status(t0+1000, "worker:eng-6789-stuff#s1-1", state("working")),
			status(t0+1000, "worker:wild#s1-1", state("working")),
			status(t0+1000, "worker:docs/55-solo#s1-1", state("working")),
		},
		want: `[{"from":"worker:docs/55-solo#s1-1","branch":"docs/55-solo","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"atlas","color":"blue","tmux":"colour32"},{"from":"worker:eng-6789-stuff#s1-1","branch":"eng-6789-stuff","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage·eng-6789","color":"green","tmux":"colour28"},{"from":"worker:feat/207-thing#s1-1","branch":"feat/207-thing","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage·207","color":"green","tmux":"colour28"},{"from":"worker:wild#s1-1","branch":"wild","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage·wild","color":"green","tmux":"colour28"}]`,
	},
	{
		name: "a branch with a space gets no identity",
		events: []string{
			dispatch(1, `"branch":"has space","name":"sage","color":"green","tmux":"colour28"`),
			status(t0+1000, "worker:has space#s1-1", state("working")),
			status(t0+1000, "worker:tab\tb#s1-1", state("working")),
			status(t0+1000, "worker:feat/y#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/y#s1-1","branch":"feat/y","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"rose","color":"pink","tmux":"colour162"},{"from":"worker:has space#s1-1","branch":"has space","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}]},{"from":"worker:tab\tb#s1-1","branch":"tab\tb","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}]}]`,
	},
	{
		name: "a malformed recorded identity falls back to the hash",
		events: []string{
			dispatch(1, `"branch":"feat/x","name":"Bad Name","color":"green","tmux":"colour28"`),
			dispatch(2, `"branch":"feat/y","name":"sage","color":null,"tmux":"colour28"`),
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+1000, "worker:feat/y#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"},{"from":"worker:feat/y#s1-1","branch":"feat/y","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage","color":null,"tmux":"colour28"}]`,
	},
	{
		name: "a dispatch event with a null branch is a type error",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":null}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		wantErr: "type",
	},
	{
		name: "a dispatch event with no branch is a type error",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","title":"t"}`,
		},
		wantErr: "type",
	},
	{
		name: "a resume event with a numeric branch is a type error",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"resume","branch":7}`,
		},
		wantErr: "type",
	},
	{
		name: "a dispatch event of another crew with a null branch is ignored",
		events: []string{
			`{"ts":1,"crew_id":"c2","kind":"dispatch","branch":null}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:    "a non-object event is a type error",
		events:  []string{`5`, status(t0+1000, "worker:feat/x#s1-1", state("working"))},
		wantErr: "type",
	},
	{
		name:    "a non-string from on a status row is a type error",
		events:  []string{`{"ts":1,"crew_id":"c1","kind":"status","from":7,"body":{"state":"working"}}`},
		wantErr: "type",
	},
	{
		name:   "a non-string from on another crew's row is ignored",
		events: []string{`{"ts":1,"crew_id":"c2","kind":"status","from":7}`, status(t0+1000, "worker:feat/x", state("working"))},
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:    "a non-object body is a type error",
		events:  []string{status(t0+1000, "worker:feat/x", `"working"`)},
		wantErr: "type",
	},
	{
		name:    "a non-object body on an older event is a type error",
		events:  []string{status(t0+1000, "worker:feat/x#s1-1", `[]`), status(t0+2000, "worker:feat/x#s1-1", state("working"))},
		wantErr: "type",
	},
	{
		name:   "a null or missing body is a row with null fields",
		events: []string{status(t0+1000, "worker:feat/a", `null`), `{"ts":2000,"crew_id":"c1","kind":"status","from":"worker:feat/b"}`},
		want:   `[{"from":"worker:feat/a","branch":"feat/a","session":null,"state":null,"detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":null,"age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b","branch":"feat/b","session":null,"state":null,"detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":2000,"pr_url":null,"age_s":1700000098,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":null,"age_s":1700000098}],"name":"cobalt","color":"royalblue","tmux":"colour68"}]`,
	},
	{
		name:    "a non-number ts on the latest event is a type error",
		events:  []string{`{"ts":"1700000000000","crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}`},
		wantErr: "type",
	},
	{
		name: "a computed infinity reads back clamped",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"a","tier":1e400}`,
			`{"ts":1e400,"crew_id":"c1","kind":"status","from":"worker:a#s1-1","body":{"state":"w"}}`,
		},
		want: `[{"from":"worker:a#s1-1","branch":"a","session":"s1-1","state":"w","detail":null,"source":null,"blocked_since":null,"answered_at":null,"ts":1.7976931348623157E+308,"pr_url":null,"age_s":-1.7976931348623157E+308,"title":null,"engine":null,"model":null,"tier":1.7976931348623157E+308,"engine_session":null,"sessions":[{"session":"s1-1","state":"w","age_s":-1.7976931348623157E+308}],"name":"orchid","color":"orchid","tmux":"colour169"}]`,
	},
	{
		name: "an overflowing literal in detail reads back clamped",
		events: []string{
			`{"ts":1700000001000,"crew_id":"c1","kind":"status","from":"worker:a#s1-1","body":{"state":"w","detail":[1e1000000000,-1e1000000000,1.5,100]}}`,
		},
		want: `[{"from":"worker:a#s1-1","branch":"a","session":"s1-1","state":"w","detail":[1.7976931348623157E+308,-1.7976931348623157E+308,1.5,100],"source":null,"blocked_since":null,"answered_at":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"w","age_s":99}],"name":"orchid","color":"orchid","tmux":"colour169"}]`,
	},
	{
		name:    "a missing ts on the latest event is a type error",
		events:  []string{`{"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}`},
		wantErr: "type",
	},
	{
		name:      "a failing worktree probe is propagated",
		events:    []string{status(t0+1000, "worker:feat/x#s1-1", state("exited"))},
		panes:     "claude /wt/x\n",
		worktrees: "",
		wtErr:     &git.ExitError{Code: 128, Stderr: "fatal: not a git repository\n"},
		wantErr:   "exit",
		wantWT:    1,
	},
}

func decodeEvents(t *testing.T, lines []string) []any {
	t.Helper()
	evs := busEvents(t, lines...)
	docs := make([]any, len(evs))
	for i, ev := range evs {
		docs[i] = ev.Doc
	}
	return docs
}

// compact encodes v and re-reads it through testjson, so two values compare
// on their values, key order aside.
func compact(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	if err := jsonval.Encode(&b, v, false); err != nil {
		t.Fatal(err)
	}
	return testjson.Compact(testjson.MustParse(t, b.String()))
}

func TestFold(t *testing.T) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			crew := c.crew
			switch crew {
			case "":
				crew = "c1"
			case "-":
				crew = ""
			}
			var wts int
			tx := &tmuxtest.Fake{Out: map[string]string{"list-panes": c.panes}}
			p := Probes{
				Tmux: tx,
				Worktrees: func() (string, error) {
					wts++
					if c.wtErr != nil {
						return "", c.wtErr
					}
					return c.worktrees, nil
				},
			}
			got, err := Fold(t.Context(), decodeEvents(t, c.events), crew, now, p)
			switch c.wantErr {
			case "type":
				if err == nil {
					t.Fatalf("err = nil, want the jq runtime error")
				}
				if n := len(tx.Calls()); n != 0 {
					t.Errorf("tmux called %d times after a failed base, want 0", n)
				}
				return
			case "exit":
				var ee *git.ExitError
				if !errors.As(err, &ee) || ee != c.wtErr {
					t.Fatalf("err = %v, want %v", err, c.wtErr)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if s, want := compact(t, got), testjson.Compact(testjson.MustParse(t, c.want)); s != want {
					t.Errorf("got\n %s\nwant\n %s", s, want)
				}
			}
			if calls := tx.Calls(); len(calls) != 1 {
				t.Errorf("tmux calls = %q, want one list-panes", calls)
			}
			if wts != c.wantWT {
				t.Errorf("Worktrees called %d times, want %d", wts, c.wantWT)
			}
		})
	}
}

// --live adds one boolean to each row, and only when asked: the rows are
// otherwise the ones TestFold pins.
func TestFoldLive(t *testing.T) {
	events := decodeEvents(t, []string{
		status(t0+1000, "worker:feat/a#s1-1", state("working")),
		status(t0+2000, "worker:feat/b#s1-1", state("working")),
	})
	tx := &tmuxtest.Fake{Out: map[string]string{"list-panes": "", "list-windows": "feat/a\tc1\nfeat/b\tc2"}}
	probes := Probes{Tmux: tx, Worktrees: func() (string, error) { return "", nil }}
	plain, err := Fold(t.Context(), events, "c1", now, probes)
	if err != nil {
		t.Fatal(err)
	}
	probes.Live = true
	live, err := Fold(t.Context(), events, "c1", now, probes)
	if err != nil {
		t.Fatal(err)
	}
	if got := tx.Calls(); len(got) != 3 || !strings.HasPrefix(got[2], "list-windows") {
		t.Errorf("tmux calls = %q, want list-panes twice then one list-windows", got)
	}
	for i, row := range live {
		want := map[string]bool{"feat/a": true, "feat/b": false}
		branch, _ := row["branch"].(string)
		got, ok := row["live"]
		if !ok || got != want[branch] {
			t.Errorf("%s: live = %v (present %t), want %t", branch, got, ok, want[branch])
		}
		if _, ok := plain[i]["live"]; ok {
			t.Errorf("%s: a fold without Live carries live", branch)
		}
		// Everything but live is the plain row.
		stripped := maps.Clone(row)
		delete(stripped, "live")
		if compact(t, stripped) != compact(t, plain[i]) {
			t.Errorf("%s: --live changed more than live:\n%s\n%s", branch, compact(t, stripped), compact(t, plain[i]))
		}
	}
}

func TestCrewWindowBranches(t *testing.T) {
	out := "feat/a\tc1\nfeat/b\t\nfeat/c\tc2\n\tc1\nfeat/d\n"
	got := CrewWindowBranches(out, "c1")
	for branch, want := range map[string]bool{"feat/a": true, "feat/b": true, "feat/c": false, "feat/d": true, "": false} {
		if got[branch] != want {
			t.Errorf("%q live = %t, want %t", branch, got[branch], want)
		}
	}
}

func TestEngineCommands(t *testing.T) {
	for cmd, want := range map[string]bool{
		"claude": true, ".claude-wrapped": true, "codex": true, ".codex-wrapped": true,
		"cursor-agent": true, "node": true, ".node": true, "pi": true, "pi-wrapped": true,
		"..claude": false, "claude-wrapped-wrapped": false, "Claude": false, "bash": false,
		"": false, "-wrapped": false, "node ": false,
	} {
		if got := IsEngineCmd(cmd); got != want {
			t.Errorf("IsEngineCmd(%q) = %t, want %t", cmd, got, want)
		}
	}
}

// TestEngineCommandsMatchLocalModels guards EngineCommands against drift from
// the case pattern of _is_engine_cmd. local-models.sh is sourced by dispatch.sh
// and carries the copy that ships; dispatch-notify.sh carries the other one,
// pinned byte-for-byte by tests/adapters.bats. The nix build sandbox holds only
// ./crew, so the lib is absent there and the guard skips; CI runs it with
// CREW_DRIFT_REQUIRED.
func TestEngineCommandsMatchLocalModels(t *testing.T) {
	src, err := os.ReadFile(localModelsScript)
	if err != nil {
		drift.Missing(t, localModelsScript, err)
	}
	var got []string
	in := false
	for line := range strings.SplitSeq(string(src), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "_is_engine_cmd() {"):
			in = true
		case in && strings.HasSuffix(line, ") return 0 ;;"):
			pattern, _, _ := strings.Cut(line, ")")
			for alt := range strings.SplitSeq(pattern, "|") {
				got = append(got, strings.TrimSpace(alt))
			}
			in = false
		}
	}
	if len(got) == 0 {
		t.Fatalf("no _is_engine_cmd case pattern found in %s", localModelsScript)
	}
	if !slices.Equal(got, EngineCommands) {
		t.Errorf("EngineCommands drifted from local-models.sh:\n bash: %v\n go:   %v", got, EngineCommands)
	}
}

func reply(ts int64, to string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":%q,"body":"go"}`, ts, to)
}

func TestFoldBlockedEpisode(t *testing.T) {
	const w = "worker:feat/x#s1-1"
	blocked := func(ts int64, detail, extra string) string {
		return status(ts, w, fmt.Sprintf(`{"state":"blocked","detail":%q%s}`, detail, extra))
	}
	cases := []struct {
		name         string
		events       []string
		state        string
		since, reply any
	}{
		{"unanswered block keeps its first ts across restamps", []string{
			blocked(t0+1000, "q", ""),
			blocked(t0+2000, "q (cycle 1 of 24)", `,"restamp":true`),
			blocked(t0+3000, "q (cycle 2 of 24)", `,"restamp":true`),
			blocked(t0+4000, "q (cycle 2 of 24)", ""),
		}, "blocked", int64(t0 + 1000), nil},
		{"a dispatcher reply answers it without a re-stamp", []string{
			blocked(t0+1000, "q", ""),
			reply(t0+2000, w),
		}, "answered", int64(t0 + 1000), int64(t0 + 2000)},
		{"a reply to another worker answers nothing", []string{
			blocked(t0+1000, "q", ""),
			reply(t0+2000, "worker:feat/y#s1-1"),
		}, "blocked", int64(t0 + 1000), nil},
		{"a reply that predates the block answers nothing", []string{
			reply(t0+500, w),
			blocked(t0+1000, "q", ""),
		}, "blocked", int64(t0 + 1000), nil},
		{"a re-stamp after the reply clears the answer, keeping the episode", []string{
			blocked(t0+1000, "q", ""),
			reply(t0+2000, w),
			blocked(t0+3000, "q (cycle 1 of 24)", `,"restamp":true`),
		}, "blocked", int64(t0 + 1000), nil},
		{"a fresh question after the reply starts a new episode", []string{
			blocked(t0+1000, "q1", ""),
			reply(t0+2000, w),
			blocked(t0+3000, "q2", ""),
		}, "blocked", int64(t0 + 3000), nil},
		{"a watchdog row neither starts nor extends the episode", []string{
			status(t0+500, w, `{"state":"blocked","detail":"quiet: x","source":"watchdog"}`),
			blocked(t0+1000, "q", ""),
			status(t0+1500, w, `{"state":"blocked","detail":"stalled: x","source":"watchdog"}`),
		}, "blocked", nil, nil},
		{"working ends the episode", []string{
			blocked(t0+1000, "q", ""),
			reply(t0+2000, w),
			status(t0+3000, w, state("working")),
		}, "working", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Probes{Tmux: &tmuxtest.Fake{}, Worktrees: func() (string, error) { return "", nil }}
			rows, err := Fold(t.Context(), decodeEvents(t, c.events), "c1", now, p)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows = %v, err = %v", rows, err)
			}
			row := rows[0]
			got := func(k string) any {
				if row[k] == nil {
					return nil
				}
				n, _ := number(row[k])
				return int64(n)
			}
			if row["state"] != c.state || got("blocked_since") != c.since || got("answered_at") != c.reply {
				t.Errorf("state=%v blocked_since=%v answered_at=%v, want %v %v %v",
					row["state"], row["blocked_since"], row["answered_at"], c.state, c.since, c.reply)
			}
		})
	}
}

// A branch-level reap row has no crew_id and `reaped` lands only on the newest
// session's crew, so another crew's older finished row must still fold to reaped.
func TestFoldReapSupersedesOtherCrewsFinishedRow(t *testing.T) {
	const b = "feat/1-x"
	reap := fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":%q,"pr":"https://github.com/o/r/pull/2","pr_state":"CLOSED"}`, t0+5000, b)
	failed := status(t0+1000, "worker:"+b+"#s1-1", `{"state":"failed","detail":"migrating to halo"}`)
	issueReap := fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":%q,"pr":"","pr_state":"ISSUE_CLOSED","issues":["12","3"]}`, t0+5000, b)
	bareReap := fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":%q}`, t0+5000, b)
	cases := []struct {
		name   string
		events []string
		want   string
		detail string
	}{
		{"older failed row, later reap row", []string{failed, reap}, "reaped", "PR #2 CLOSED"},
		{"issue-mode reap", []string{failed, issueReap}, "reaped", "issue #12, #3 CLOSED"},
		{"reap row with nothing known", []string{failed, bareReap}, "reaped", "reaped"},
		{"reap row older than the row", []string{reap, status(t0+9000, "worker:"+b+"#s1-1", `{"state":"failed"}`)}, "failed", ""},
		{"a live row is never reaped", []string{status(t0+1000, "worker:"+b+"#s1-1", `{"state":"working"}`), reap}, "working", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Probes{Tmux: &tmuxtest.Fake{Out: map[string]string{}}, Worktrees: func() (string, error) { return "", nil }}
			got, err := Fold(t.Context(), decodeEvents(t, c.events), "c1", now, p)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("rows = %d, want 1: %s", len(got), compact(t, got))
			}
			if st, _ := got[0]["state"].(string); st != c.want {
				t.Errorf("state = %q, want %q", st, c.want)
			}
			if c.detail == "" {
				return
			}
			if d, _ := got[0]["detail"].(string); d != c.detail {
				t.Errorf("detail = %q, want %q", d, c.detail)
			}
		})
	}
}

// A colliding codename's suffix is the id the queue's branch rule reads off the
// branch: an issue number, a Linear id, else the whole branch.
func TestFoldSuffixFollowsBranchIssueRule(t *testing.T) {
	branches := []struct{ branch, suffix string }{
		{"feat/12x", "feat/12x"},
		{"feat/12-x", "12"},
		{"feat/grok-4-7-x", "feat/grok-4-7-x"},
		{"eng-42-x", "eng-42"},
		{"fix/7-y", "7"},
		{"fix/7", "7"},
		{"12-a/x", "12-a/x"},
		{"feat/eng-42-x", "feat/eng-42-x"},
	}
	var events []string
	for i, b := range branches {
		events = append(events,
			dispatch(int64(i+1), fmt.Sprintf(`"branch":%q,"name":"sage","color":"green","tmux":"colour28"`, b.branch)),
			status(t0+1000, "worker:"+b.branch+"#s1-1", state("working")))
	}
	rows, err := Fold(t.Context(), decodeEvents(t, events), "c1", now, Probes{Tmux: &tmuxtest.Fake{}, Worktrees: func() (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		found := false
		for _, r := range rows {
			if r["branch"] == b.branch {
				found = true
				if want := "sage·" + b.suffix; r["name"] != want {
					t.Errorf("branch %s: name = %v, want %s", b.branch, r["name"], want)
				}
			}
		}
		if !found {
			t.Errorf("no row for %s", b.branch)
		}
	}
}

// The newest stale row per branch flags a terminal worker as needing a human
// cleanup, until a stale:false row or a newer status row clears it.
func TestFoldNeedsCleanup(t *testing.T) {
	const b = "feat/9-x"
	from := "worker:" + b + "#s1-1"
	stale := func(ts int64, on bool) string {
		if !on {
			return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"stale","branch":%q,"session":"s1-1","stale":false}`, ts, b)
		}
		return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"stale","branch":%q,"session":"s1-1","stale":true,"reason":"uncommitted changes; issue closed","worktree":"/w/x","inspect":"git -C '/w/x' status --short","remove":"git worktree remove --force '/w/x'"}`, ts, b)
	}
	failed := status(t0+1000, from, state("failed"))
	cases := []struct {
		name   string
		events []string
		want   bool
	}{
		{"stale row after terminal status", []string{failed, stale(t0+2000, true)}, true},
		{"done and exited count", []string{status(t0+1000, from, state("done")), stale(t0+2000, true)}, true},
		{"no stale row", []string{failed}, false},
		{"stale:false row is newest", []string{failed, stale(t0+2000, true), stale(t0+3000, false)}, false},
		{"stale:true row is newest again", []string{failed, stale(t0+2000, false), stale(t0+3000, true)}, true},
		{"newer status row", []string{stale(t0+2000, true), status(t0+3000, from, state("failed"))}, false},
		{"non-terminal state", []string{status(t0+1000, from, state("working")), stale(t0+2000, true)}, false},
		{"reaped", []string{failed, stale(t0+2000, true), fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":%q}`, t0+3000, b)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Probes{Tmux: &tmuxtest.Fake{Out: map[string]string{}}, Worktrees: func() (string, error) { return "", nil }}
			got, err := Fold(t.Context(), decodeEvents(t, c.events), "c1", now, p)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("rows = %d, want 1: %s", len(got), compact(t, got))
			}
			r := got[0]
			if nc, _ := r["needs_cleanup"].(bool); nc != c.want {
				t.Fatalf("needs_cleanup = %v, want %v: %s", nc, c.want, compact(t, r))
			}
			if !c.want {
				for _, k := range []string{"cleanup_reason", "cleanup_worktree", "cleanup_inspect", "cleanup_remove"} {
					if _, ok := r[k]; ok {
						t.Errorf("%s present on a row that needs no cleanup", k)
					}
				}
				return
			}
			if r["cleanup_reason"] != "uncommitted changes; issue closed" || r["cleanup_worktree"] != "/w/x" ||
				r["cleanup_inspect"] != "git -C '/w/x' status --short" || r["cleanup_remove"] != "git worktree remove --force '/w/x'" {
				t.Errorf("cleanup fields wrong: %s", compact(t, r))
			}
		})
	}
}
