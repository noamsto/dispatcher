package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// These are the bus-driven `identity:` cases tests/crew.bats used to drive by
// shelling out to `crew identity` (#1044). Each runs the arm itself — Run over a
// real bus log — and pins the stdout and exit code the case compared with jq.
// The cases that assert nothing but the subcommand's own contract (its keys, its
// determinism, a tmux that is not there) are in crew/identity_cli_test.go.

// armBus is a bus log holding rows, in the shape Run reads it.
func armBus(t *testing.T, rows ...string) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bus.Paths{Dir: dir, Log: path}
}

// ask is `crew identity <branch> <crew>`: the line the arm printed and its exit
// code. No tmux is wired in, as a window read that fails names nothing, and no
// CREW_IDENTITY_AVOID is read.
func ask(t *testing.T, paths bus.Paths, branch, crew string) (string, int) {
	t.Helper()
	var out, errOut strings.Builder
	code := Run(t.Context(), Request{Branch: branch, Crew: crew}, paths, &out, &errOut,
		Options{CrewID: func() string { return crew }})
	if errOut.String() != "" {
		t.Fatalf("identity %s wrote to stderr: %q", branch, errOut.String())
	}
	return out.String(), code
}

// dispatchRow is a `dispatch` row of crew for branch, stamped with the identity
// the arm would have recorded; pass no identity for a row that recorded none.
func dispatchRow(ts int, crew, branch string, identity ...string) string {
	row := fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q`, ts, crew, branch)
	if len(identity) > 0 {
		return row + "," + identity[0] + "}"
	}
	return row + "}"
}

// statusRow is the `worker:` status of branch's worker, as `crew status` appends it.
func statusRow(ts int, crew, branch, state string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"status","from":"worker:%s#s1-1","body":{"state":%q}}`,
		ts, crew, branch, state)
}

func TestTwoLiveBranchesOnOneHashSlotGetDifferentCodenames(t *testing.T) {
	const crew = "c1"
	// The case's two branches hash to one pool slot, so one of them has to move.
	a, b := "feat/33-thing", "feat/38-thing"
	if Slot(a) != Slot(b) {
		t.Fatalf("the fixture's branches no longer share a hash slot: %d %d", Slot(a), Slot(b))
	}
	own := names[Slot(a)]
	next := names[(Slot(a)+1)%poolSize]

	paths := armBus(t, dispatchRow(1, crew, a))

	outA, codeA := ask(t, paths, a, crew)
	outB, codeB := ask(t, paths, b, crew)
	if codeA != 0 || codeB != 0 {
		t.Fatalf("codes = %d %d, want 0 0", codeA, codeB)
	}
	// a holds the shared slot's codename; b steps to the next one, so its name and
	// its tmux colour both differ.
	if outA != pool(t, own)+"\n" {
		t.Errorf("identity %s = %s, want %s", a, outA, pool(t, own))
	}
	if outB != pool(t, next)+"\n" {
		t.Errorf("identity %s = %s, want %s", b, outB, pool(t, next))
	}
}

func TestRecordedIdentityOutsideTheCodenameGrammarIsNotReplayed(t *testing.T) {
	const crew = "c1"
	// #470: the three rows the case planted, each carrying a recorded value
	// outside the grammar — a `$(…)` name, a `#(…)` tmux colour, a name with a
	// trailing newline. Replay of any of them would put that text on stdout.
	paths := armBus(t,
		dispatchRow(1, crew, "feat/33-thing", `"name":"$(touch pwned)","color":"green","tmux":"colour28"`),
		dispatchRow(2, crew, "feat/34-thing", `"name":"nova","color":"green","tmux":"red#(touch pwned)"`),
		dispatchRow(3, crew, "feat/35-thing", `"name":"nova\n","color":"green","tmux":"colour28"`),
	)
	// Each branch is answered with its pool identity: the name the arm emits is a
	// pool codename and its tmux key a `colour<N>`, the case's two regexes.
	for _, branch := range []string{"feat/33-thing", "feat/34-thing", "feat/35-thing"} {
		out, code := ask(t, paths, branch, crew)
		if code != 0 || out != fallback(branch)+"\n" {
			t.Errorf("identity %s = %d %q, want 0 %q", branch, code, out, fallback(branch))
		}
	}
}

func TestARecordedNameAnotherLiveWorkerHoldsIsNotReused(t *testing.T) {
	const crew, branch = "c1", "feat/2-b"
	const nova = `"name":"nova","color":"magenta","tmux":"colour127"`
	const recorded = `{"name":"nova","color":"magenta","tmux":"colour127"}` + "\n"

	t.Run("a live holder of the name pushes us off it", func(t *testing.T) {
		paths := armBus(t,
			dispatchRow(1, crew, "feat/1-a", nova),
			dispatchRow(2, crew, branch, nova),
			statusRow(3, crew, "feat/1-a", "working"))
		out, code := ask(t, paths, branch, crew)
		if code != 0 || out != fallback(branch)+"\n" {
			t.Errorf("identity %s = %d %q, want 0 %q", branch, code, out, fallback(branch))
		}
	})
	t.Run("once that worker finishes the recorded name is ours again", func(t *testing.T) {
		paths := armBus(t,
			dispatchRow(1, crew, "feat/1-a", nova),
			dispatchRow(2, crew, branch, nova),
			statusRow(3, crew, "feat/1-a", "working"),
			statusRow(4, crew, "feat/1-a", "done"))
		out, code := ask(t, paths, branch, crew)
		if code != 0 || out != recorded {
			t.Errorf("identity %s = %d %q, want 0 %q", branch, code, out, recorded)
		}
	})
}

func TestOfTwoLiveBranchesSharingARecordedNameTheEarlierKeepsIt(t *testing.T) {
	const crew = "c1"
	const nova = `"name":"nova","color":"magenta","tmux":"colour127"`
	const recorded = `{"name":"nova","color":"magenta","tmux":"colour127"}` + "\n"
	// Two dispatches of one codename, no statuses: both are live, so the later
	// dispatch yields the name to the earlier one.
	paths := armBus(t,
		dispatchRow(1, crew, "feat/1-a", nova),
		dispatchRow(2, crew, "feat/2-b", nova),
	)

	if out, code := ask(t, paths, "feat/1-a", crew); code != 0 || out != recorded {
		t.Errorf("identity feat/1-a = %d %q, want 0 %q", code, out, recorded)
	}
	if out, code := ask(t, paths, "feat/2-b", crew); code != 0 || out != fallback("feat/2-b")+"\n" {
		t.Errorf("identity feat/2-b = %d %q, want 0 %q", code, out, fallback("feat/2-b"))
	}
}
