package entry

import "io"

// noCrewID is the `id` arm's refusal, verbatim. `crew stack` carries the same
// text for its own arm (stack.noCrewID); the two lines are the answer a session
// acts on when it has no id, so neither may drift from the other by accident.
const noCrewID = "crew: no crew id — CREW_ID unset and no WORKER_TASK.md crew_id; " +
	"run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, " +
	"or 'crew new' to start one\n"

// IDOptions is everything RunID reads beyond the writers.
type IDOptions struct {
	// CrewID is `_crew_id`: the cwd worktree's WORKER_TASK.md crew_id, then
	// $CREW_ID, then "". Task-document-first on purpose — the env can be lost
	// across a re-exec, the task doc cannot be rewritten by a worker's own
	// scaffold record.
	CrewID func() string
}

// RunID is crew.sh's `id` arm: print this session's crew id, or the refusal.
// Read-only, it never mints one (#29), and the arm's capture-then-print is what
// normalises the id's trailing newline, so the line printed is the id and a
// newline.
func RunID(stdout, stderr io.Writer, o IDOptions) int {
	if id := o.CrewID(); id != "" {
		say(stdout, "%s\n", id)
		return 0
	}
	say(stderr, "%s", noCrewID)
	return exitFailure
}
