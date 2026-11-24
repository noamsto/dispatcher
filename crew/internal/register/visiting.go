package register

import (
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

var originRe = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9_-]*\z`)

// RunVisiting is `crew register --visiting <origin>`: the remote half of a
// layout-2 dispatch, and the only creator of a visiting crew. It writes the
// crew's meta.json and nothing else: the dispatcher is not on this host.
func RunVisiting(origin string, paths bus.Paths, stderr io.Writer, o Options) int {
	crew := o.CrewID()
	if crew == "" {
		say(stderr, "crew: CREW_ID unset and no WORKER_TASK.md crew_id — run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one\n")
		return exitFailure
	}
	if !bus.ValidCrewID(crew) {
		say(stderr, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n")
		return exitFailure
	}
	err := visit(origin, crew, paths, o)
	var create *errCreate
	switch {
	case err == nil:
		return 0
	case errors.As(err, &create):
		say(stderr, "crew: register: %v\n", err)
	default:
		say(stderr, "crew: %v\n", err)
	}
	return exitFailure
}

func visit(origin, crew string, paths bus.Paths, o Options) error {
	switch {
	case !originRe.MatchString(origin):
		return &errCreate{fmt.Errorf("invalid origin host %q", origin)}
	case paths.State == "":
		return &errCreate{errors.New("needs XDG_STATE_HOME or HOME")}
	}
	bc, err := paths.Crew(crew)
	switch {
	case err == nil && bc.Layout == 2:
		return checkVisiting(paths.State, crew, origin)
	case err == nil && legacy(paths, bc):
		return &errCreate{fmt.Errorf("crew %s already runs at layout 1 in this clone — finish its workers before dispatching it here at layout 2", crew)}
	case err != nil && !errors.Is(err, bus.ErrNotRepo):
		return err
	}
	return createVisiting(paths.State, crew, origin, o)
}

// createVisiting creates the visiting meta. A lost race re-reads the winner's.
func createVisiting(state, crew, origin string, o Options) error {
	m := bus.Meta{ID: crew, Created: o.Clock.Now().UnixMilli(), HomeHost: origin, Visiting: true, CreatedBy: "remote-dispatch"}
	err := bus.CreateMeta(state, m)
	switch {
	case errors.Is(err, bus.ErrMetaExists):
		return checkVisiting(state, crew, origin)
	case err != nil:
		return &errCreate{err}
	}
	return nil
}

// checkVisiting accepts an existing meta only when it is this origin's visiting crew.
func checkVisiting(state, crew, origin string) error {
	m, err := bus.ReadMeta(state, crew)
	switch {
	case err != nil:
		return &errCreate{err}
	case !m.Visiting:
		return &errCreate{fmt.Errorf("crew %s is this host's own crew (home_host %s), not a visiting one", crew, m.HomeHost)}
	case m.HomeHost != origin:
		return &errCreate{fmt.Errorf("crew %s on this host is visiting from %s, not %s", crew, m.HomeHost, origin)}
	}
	return nil
}
