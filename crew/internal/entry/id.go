package entry

import "io"

// noCrewID is the `id` arm's refusal, verbatim. `crew stack` keeps the same text
// for its own arm: it is the answer a session acts on when it has no id.
const noCrewID = "crew: no crew id — CREW_ID unset and no WORKER_TASK.md crew_id; " +
	"run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, " +
	"or 'crew new' to start one\n"

// IDOptions is everything RunID reads beyond the writers.
type IDOptions struct {
	// CrewID is `_crew_id`: the cwd worktree's WORKER_TASK.md crew_id, then
	// $CREW_ID, then "".
	CrewID func() string
}

// RunID is crew.sh's `id` arm: print this session's crew id, or the refusal.
// Read-only: it never mints one (#29).
func RunID(stdout, stderr io.Writer, o IDOptions) int {
	if id := o.CrewID(); id != "" {
		say(stdout, "%s\n", id)
		return 0
	}
	say(stderr, "%s", noCrewID)
	return exitFailure
}
