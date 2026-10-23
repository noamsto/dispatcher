package stall

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// The #908 sender check at the watchdog's own door. Which stamps allow which id
// is the sender package's tables (TestCheck, TestWatch); these are the wiring:
// a refusal costs no probes, no samples and no bus row, and keeps the exit code
// and the stderr the ported cases pinned; a pane watching itself still posts.

// leadX and roleX are the reasons the sender package prints for a pane stamped
// as a lead, and as a role, asked to speak as another id.
const (
	leadX = "this pane is stamped as the lead of feat/x, which posts only as worker:feat/x " +
		"or worker:feat/x#s<session> ($CREW_WORKER_ID)"
	roleX = "this pane is stamped as role:feat/x:reviewer, the only id it posts as"
)

// paneRow is one `list-panes -a -F` line: pid, branch, role, crew, window, pane.
func paneRow(pid int, branch, role, crew, window, pane string) string {
	return fmt.Sprintf("%d\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\n", pid, branch, role, crew, window, pane)
}

// anchoredAs is the check's view of a caller sitting in a pane stamped for
// branch/role, plus any further panes it can see.
func anchoredAs(branch, role string, panes ...string) sender.Probes {
	self := paneRow(os.Getpid(), branch, role, "c1", "@1", "%1")
	return sender.Probes{
		Tmux: &tmuxtest.Fake{Out: map[string]string{"list-panes": self + strings.Join(panes, "")}},
		Proc: &proctest.Table{},
	}
}

func TestRunRefusesForeignSenders(t *testing.T) {
	for _, tc := range []struct {
		name, branch, role, target, pane, reason string
		panes                                     []string
	}{
		{
			name:   "a stamped lead cannot watch as another branch",
			branch: "feat/x", role: "lead", target: "worker:feat/y#s1-1", pane: "%1",
			reason: leadX,
		},
		{
			name:   "a stamped lead cannot watch a role pane as itself",
			branch: "feat/x", role: "lead", target: "role:feat/y:reviewer", pane: "%1",
			reason: leadX,
		},
		{
			name:   "a stamped lead cannot target a pane outside its window",
			branch: "feat/x", role: "lead", target: "worker:feat/x#s1-1", pane: "%2",
			panes:  []string{paneRow(999999, "feat/y", "lead", "c1", "@2", "%2")},
			reason: "pane %2 is outside this pane's window",
		},
		{
			name:   "a stamped role pane watches only as its own role",
			branch: "feat/x", role: "reviewer", target: "worker:feat/x#s1-1", pane: "%1",
			reason: roleX,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			o := h.options()
			o.Sender = anchoredAs(tc.branch, tc.role, tc.panes...)
			code := Run(h.ctx, Request{Target: tc.target, Pane: ptr(tc.pane), Engine: ptr("claude"),
				MaxLife: ptr("3")}, h.paths, &h.stderr, o)
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			want := fmt.Sprintf("crew: stall-watch: refusing sender '%s' on pane %s: %s\n",
				tc.target, tc.pane, tc.reason)
			if h.stderr.String() != want {
				t.Errorf("stderr %q, want %q", h.stderr.String(), want)
			}
			if f.newProbes != 0 || f.samples != 0 {
				t.Errorf("probes %d samples %d, want the check to run before either", f.newProbes, f.samples)
			}
			if _, err := os.Stat(h.paths.Log); !os.IsNotExist(err) {
				t.Errorf("bus log exists (err %v), want nothing posted", err)
			}
		})
	}
}

// A pane watching its own branch is the case the check must never catch: the
// anchor matches, the pane is in the window, and the watch runs.
func TestRunWatchesItsOwnPane(t *testing.T) {
	_, rows := watchRun(t, watchScript{
		frames: []string{"prompt_select.txt"},
		anchor: anchoredAs("feat/x", "lead"),
		req:    Request{Target: "worker:feat/x#s1-1", Pane: ptr("%1"), MaxLife: ptr("3")},
	})
	want := []string{"worker:feat/x#s1-1|blocked|watchdog|prompt: interactive prompt in pane %1 — " +
		"worker is waiting on input nobody can give"}
	if !slices.Equal(rows, want) {
		t.Errorf("rows\n %q\nwant\n %q", rows, want)
	}
}
