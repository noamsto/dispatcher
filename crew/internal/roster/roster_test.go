package roster

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

const crewScript = "../../../adapters/core/crew.sh"

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
	wtErr     *ExitError
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
		want: `[{"from":"worker:feat/33-thing#s1-1","branch":"feat/33-thing","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage","color":"green","tmux":"colour28"},{"from":"worker:feat/38-thing#s1-1","branch":"feat/38-thing","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":98}],"name":"atlas","color":"blue","tmux":"colour32"}]`,
	},
	{
		name: "sessions of one branch collapse into one row",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+2000, "worker:feat/x#s1-1", state("done")),
			status(t0+3000, "worker:feat/x#s2-2", state("working")),
		},
		want: `[{"from":"worker:feat/x#s2-2","branch":"feat/x","session":"s2-2","state":"working","detail":null,"source":null,"ts":1700000003000,"pr_url":null,"age_s":97,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"done","age_s":98},{"session":"s2-2","state":"working","age_s":97}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "engine_session from the dispatch event",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine_session":"aaaa"}`,
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":"aaaa","sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "engine_session from a newer resume event",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine_session":"aaaa"}`,
			`{"ts":9999999999999,"crew_id":"c1","kind":"resume","branch":"feat/x","engine_session":"bbbb"}`,
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":"bbbb","sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:   "codename derives from the branch hash",
		events: []string{status(t0+1000, "worker:feat/x#s1-1", state("working"))},
		want:   `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "one session's exited is not resolved from another's history",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+2000, "worker:feat/x#s2-2", state("exited")),
		},
		worktrees: worktree("/wt/other", "main"),
		want:      `[{"from":"worker:feat/x#s2-2","branch":"feat/x","session":"s2-2","state":"exited","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99},{"session":"s2-2","state":"exited","age_s":98}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want:      `[{"from":"worker:feat/roster-live#s1-1","branch":"feat/roster-live","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"prev_state":"working","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"ash","color":"dimgrey","tmux":"colour102"}]`,
		wantWT:    1,
	},
	{
		name: "exited with no previous state resolves to working",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("exited")),
		},
		panes:     "node /wt/x\n",
		worktrees: worktree("/wt/x", "feat/x"),
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"prev_state":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want:      `[{"from":"worker:feat/roster-dead#s1-1","branch":"feat/roster-dead","session":"s1-1","state":"exited","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"name":"coral","color":"salmon","tmux":"colour167"}]`,
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
		want:      `[{"from":"worker:feat/roster-sib#s1-1","branch":"feat/roster-sib","session":"s1-1","state":"exited","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"name":"atlas","color":"blue","tmux":"colour32"}]`,
		wantWT:    1,
	},
	{
		name: "exited with no worktree for the branch stays exited",
		events: []string{
			status(t0+1000, "worker:feat/gone#s1-1", state("exited")),
		},
		panes:     "claude /wt/gone\n",
		worktrees: worktree("/wt/x", "feat/x"),
		want:      `[{"from":"worker:feat/gone#s1-1","branch":"feat/gone","session":"s1-1","state":"exited","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"name":"khaki","color":"darkkhaki","tmux":"colour101"}]`,
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
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"exited","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"prev_state":"working","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"prev_state":"working","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want:      `[{"from":"worker:feat/x\n#s1-1","branch":"feat/x\n","session":"s1-1","state":"exited\n","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"prev_state":"exited\n","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited\n","age_s":98}],"exit_suspect":true}]`,
		wantWT:    1,
	},
	{
		name: "exited rows with an empty branch skip the worktree probe",
		events: []string{
			status(t0+1000, "worker:#s1-1", state("exited")),
		},
		panes:  "claude /wt/x\n",
		want:   `[{"from":"worker:#s1-1","branch":"","session":"s1-1","state":"exited","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}]}]`,
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
		want:      `[{"from":"worker:feat/a#s1-1","branch":"feat/a","session":"s1-1","state":"exited","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b#s1-1","branch":"feat/b","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"prev_state":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":99}],"exit_suspect":true,"name":"cobalt","color":"royalblue","tmux":"colour68"},{"from":"worker:feat/c#s1-1","branch":"feat/c","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"ember","color":"orange","tmux":"colour130"}]`,
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
		want:      `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"blocked","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"prev_state":"blocked","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"exited","age_s":98}],"exit_suspect":true,"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
		wantWT:    1,
	},
	{
		name: "title joins on the branch",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1","title":"Do a thing"}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":"Do a thing","engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "engine, model and tier join on the branch",
		events: []string{
			`{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s1-1","engine":"claude","model":"sonnet","tier":"standard","title":"T"}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":"T","engine":"claude","model":"sonnet","tier":"standard","engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "the last dispatch event wins and false becomes null",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","title":"old","engine":"claude","model":"opus"}`,
			`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x","title":false,"engine":"codex","tier":0}`,
			`{"ts":3,"crew_id":"c2","kind":"dispatch","branch":"feat/x","title":"other crew","model":"haiku"}`,
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":"codex","model":null,"tier":0,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:   "engine, model and tier default to null with no dispatch event",
		events: []string{status(t0+1000, "worker:feat/x", state("working"))},
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name:   "a legacy branch-keyed row still renders",
		events: []string{status(t0+1000, "worker:feat/x", state("working"))},
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "a hash in the branch splits at the last hash",
		events: []string{
			status(t0+1000, "worker:a#b#s1-1", state("working")),
			status(t0+2000, "worker:a#b", state("working")),
			status(t0+3000, "worker:c#", state("working")),
		},
		want: `[{"from":"worker:a#b","branch":"a","session":"b","state":"working","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"b","state":"working","age_s":98}],"name":"orchid","color":"orchid","tmux":"colour169"},{"from":"worker:a#b#s1-1","branch":"a#b","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"mauve","color":"palevioletred","tmux":"colour132"},{"from":"worker:c#","branch":"c","session":"","state":"working","detail":null,"source":null,"ts":1700000003000,"pr_url":null,"age_s":97,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"","state":"working","age_s":97}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "source is carried and a 200 char detail is cut to 120",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":"quiet: `+strings.Repeat("x", 200)+`","source":"watchdog"}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":"quiet: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","source":"watchdog","ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "detail is cut by codepoint, not byte",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":"`+strings.Repeat("é世🙂", 43)+`"}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":"é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂é世🙂","source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "a worker-posted row has a null source",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":"which approach?"}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":"which approach?","source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "a suppressed re-stamp still refreshes age_s",
		events: []string{
			status(t0+100000-30000, "worker:feat/x#s1-1", `{"state":"blocked","detail":"need a waiver (cycle 7 of 24)"}`),
			status(t0+100000, "worker:feat/x#s1-1", `{"state":"blocked","detail":"need a waiver (cycle 8 of 24)"}`),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"blocked","detail":"need a waiver (cycle 8 of 24)","source":null,"ts":1700000100000,"pr_url":null,"age_s":0,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"blocked","age_s":0}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "detail array is sliced to 120 elements",
		events: []string{
			status(t0+1000, "worker:feat/x", `{"state":"blocked","detail":[`+strings.TrimSuffix(strings.Repeat(`1.0e2,`, 130), ",")+`]}`),
		},
		want: `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"blocked","detail":[1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2,1.0E+2],"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"blocked","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "detail false and null are null",
		events: []string{
			status(t0+1000, "worker:feat/a", `{"state":"working","detail":false}`),
			status(t0+1000, "worker:feat/b", `{"state":"working","detail":null}`),
		},
		want: `[{"from":"worker:feat/a","branch":"feat/a","session":null,"state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b","branch":"feat/b","session":null,"state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"cobalt","color":"royalblue","tmux":"colour68"}]`,
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
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":"ok","source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":98}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want: `[{"from":"worker:feat/a#s1-1","branch":"feat/a","session":"s1-1","state":"done","detail":null,"source":null,"ts":1700000002000,"pr_url":"https://x/1","age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"done","age_s":98}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b#s1-1","branch":"feat/b","session":"s1-1","state":"done","detail":null,"source":null,"ts":1700000003000,"pr_url":false,"age_s":97,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"done","age_s":97}],"name":"cobalt","color":"royalblue","tmux":"colour68"}]`,
	},
	{
		name: "ties on ts pick the last event",
		events: []string{
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+1000, "worker:feat/x#s1-1", state("blocked")),
			status(t0+1000, "worker:feat/x#s2-2", state("done")),
		},
		want: `[{"from":"worker:feat/x#s2-2","branch":"feat/x","session":"s2-2","state":"done","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"blocked","age_s":99},{"session":"s2-2","state":"done","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
	},
	{
		name: "age_s keeps the exponent form of a huge ts",
		events: []string{
			status(t0+1000, "worker:feat/a", state("working")),
			`{"ts":1e30,"crew_id":"c1","kind":"status","from":"worker:feat/b","body":{"state":"working"}}`,
			`{"ts":1.0e3,"crew_id":"c1","kind":"status","from":"worker:feat/c","body":{"state":"working"}}`,
		},
		want: `[{"from":"worker:feat/a","branch":"feat/a","session":null,"state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b","branch":"feat/b","session":null,"state":"working","detail":null,"source":null,"ts":1E+30,"pr_url":null,"age_s":-1E+27,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":-1E+27}],"name":"cobalt","color":"royalblue","tmux":"colour68"},{"from":"worker:feat/c","branch":"feat/c","session":null,"state":"working","detail":null,"source":null,"ts":1.0E+3,"pr_url":null,"age_s":1700000099,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":1700000099}],"name":"ember","color":"orange","tmux":"colour130"}]`,
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
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"ts":1000,"pr_url":null,"age_s":1700000099,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":1700000099}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want: `[{"from":"worker:docs/55-solo#s1-1","branch":"docs/55-solo","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"atlas","color":"blue","tmux":"colour32"},{"from":"worker:eng-6789-stuff#s1-1","branch":"eng-6789-stuff","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage·eng-6789","color":"green","tmux":"colour28"},{"from":"worker:feat/207-thing#s1-1","branch":"feat/207-thing","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage·207","color":"green","tmux":"colour28"},{"from":"worker:wild#s1-1","branch":"wild","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage·wild","color":"green","tmux":"colour28"}]`,
	},
	{
		name: "a branch with a space gets no identity",
		events: []string{
			dispatch(1, `"branch":"has space","name":"sage","color":"green","tmux":"colour28"`),
			status(t0+1000, "worker:has space#s1-1", state("working")),
			status(t0+1000, "worker:tab\tb#s1-1", state("working")),
			status(t0+1000, "worker:feat/y#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/y#s1-1","branch":"feat/y","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"rose","color":"pink","tmux":"colour162"},{"from":"worker:has space#s1-1","branch":"has space","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}]},{"from":"worker:tab\tb#s1-1","branch":"tab\tb","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}]}]`,
	},
	{
		name: "a malformed recorded identity falls back to the hash",
		events: []string{
			dispatch(1, `"branch":"feat/x","name":"Bad Name","color":"green","tmux":"colour28"`),
			dispatch(2, `"branch":"feat/y","name":"sage","color":null,"tmux":"colour28"`),
			status(t0+1000, "worker:feat/x#s1-1", state("working")),
			status(t0+1000, "worker:feat/y#s1-1", state("working")),
		},
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"},{"from":"worker:feat/y#s1-1","branch":"feat/y","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"sage","color":null,"tmux":"colour28"}]`,
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
		want: `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want:   `[{"from":"worker:feat/x","branch":"feat/x","session":null,"state":"working","detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":"working","age_s":99}],"name":"lagoon","color":"darkcyan","tmux":"colour31"}]`,
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
		want:   `[{"from":"worker:feat/a","branch":"feat/a","session":null,"state":null,"detail":null,"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":null,"age_s":99}],"name":"lime","color":"lime","tmux":"colour64"},{"from":"worker:feat/b","branch":"feat/b","session":null,"state":null,"detail":null,"source":null,"ts":2000,"pr_url":null,"age_s":1700000098,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":null,"state":null,"age_s":1700000098}],"name":"cobalt","color":"royalblue","tmux":"colour68"}]`,
	},
	{
		name:    "a non-number ts on the latest event is a type error",
		events:  []string{`{"ts":"1700000000000","crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}`},
		wantErr: "type",
	},
	{
		name: "a computed infinity reads back as a literal",
		events: []string{
			`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"a","tier":Infinity}`,
			`{"ts":Infinity,"crew_id":"c1","kind":"status","from":"worker:a#s1-1","body":{"state":"w"}}`,
		},
		want: `[{"from":"worker:a#s1-1","branch":"a","session":"s1-1","state":"w","detail":null,"source":null,"ts":1.7976931348623157E+308,"pr_url":null,"age_s":-1.7976931348623157E+308,"title":null,"engine":null,"model":null,"tier":1.7976931348623157E+308,"engine_session":null,"sessions":[{"session":"s1-1","state":"w","age_s":-1.7976931348623157E+308}],"name":"orchid","color":"orchid","tmux":"colour169"}]`,
	},
	{
		name: "an overflowing literal in detail reads back as a literal",
		events: []string{
			`{"ts":1700000001000,"crew_id":"c1","kind":"status","from":"worker:a#s1-1","body":{"state":"w","detail":[1e1000000000,-1e1000000000,NaN,1.5,100]}}`,
		},
		want: `[{"from":"worker:a#s1-1","branch":"a","session":"s1-1","state":"w","detail":[1.7976931348623157E+308,-1.7976931348623157E+308,null,1.5,100],"source":null,"ts":1700000001000,"pr_url":null,"age_s":99,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"w","age_s":99}],"name":"orchid","color":"orchid","tmux":"colour169"}]`,
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
		wtErr:     &ExitError{Code: 128, Stderr: "fatal: not a git repository\n"},
		wantErr:   "exit",
		wantWT:    1,
	},
}

func decodeEvents(t *testing.T, lines []string) []jsonv.Value {
	t.Helper()
	events, err := jsonv.DecodeStream(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return events
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
			var panes, wts int
			p := Probes{
				Panes: func() string { panes++; return c.panes },
				Worktrees: func() (string, error) {
					wts++
					if c.wtErr != nil {
						return "", c.wtErr
					}
					return c.worktrees, nil
				},
			}
			got, err := Fold(decodeEvents(t, c.events), crew, now, p)
			switch c.wantErr {
			case "type":
				var te *jsonv.TypeError
				if !errors.As(err, &te) {
					t.Fatalf("err = %v, want a *jsonv.TypeError", err)
				}
				if panes != 0 {
					t.Errorf("Panes called %d times after a failed base, want 0", panes)
				}
				return
			case "exit":
				var ee *ExitError
				if !errors.As(err, &ee) || ee != c.wtErr {
					t.Fatalf("err = %v, want %v", err, c.wtErr)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if s := string(jsonv.Append(nil, got, jsonv.Options{})); s != c.want {
					t.Errorf("got\n %s\nwant\n %s", s, c.want)
				}
			}
			if panes != 1 {
				t.Errorf("Panes called %d times, want 1", panes)
			}
			if wts != c.wantWT {
				t.Errorf("Worktrees called %d times, want %d", wts, c.wantWT)
			}
		})
	}
}

func TestEngineCommands(t *testing.T) {
	for cmd, want := range map[string]bool{
		"claude": true, ".claude-wrapped": true, "codex": true, ".codex-wrapped": true,
		"cursor-agent": true, "node": true, ".node": true, "pi": true, "pi-wrapped": true,
		"..claude": false, "claude-wrapped-wrapped": false, "Claude": false, "bash": false,
		"": false, "-wrapped": false, "node ": false,
	} {
		if got := isEngineCmd(cmd); got != want {
			t.Errorf("isEngineCmd(%q) = %t, want %t", cmd, got, want)
		}
	}
}

// TestEngineCommandsMatchCrewSh guards EngineCommands against drift from the
// case pattern of _is_engine_cmd until crew.sh's copy is deleted. The nix
// build sandbox holds only ./crew, so the script is absent there.
func TestEngineCommandsMatchCrewSh(t *testing.T) {
	src, err := os.ReadFile(crewScript)
	if err != nil {
		t.Skipf("%s not present: %v", crewScript, err)
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
		t.Fatalf("no _is_engine_cmd case pattern found in %s", crewScript)
	}
	if !slices.Equal(got, EngineCommands) {
		t.Errorf("EngineCommands drifted from crew.sh:\n bash: %v\n go:   %v", got, EngineCommands)
	}
}
