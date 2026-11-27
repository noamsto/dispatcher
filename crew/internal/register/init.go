package register

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// InitResult is what `crew init` prints: where the crew lives and whether this
// call created its meta.json.
type InitResult struct {
	ID      string `json:"id"`
	Dir     string `json:"dir"`
	Log     string `json:"log"`
	Created bool   `json:"created"`
}

// Init is `crew init`: create crew's layout-2 directory with its meta.json and
// an empty events.jsonl, and claim nothing (no pid, pane or engine file; that
// is register's job). It is idempotent: an existing meta.json is left as it
// is and an existing log is never truncated. Callers use it so they never
// mkdir under the crew state dir themselves.
func Init(crew string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	if crew == "" {
		crew = o.CrewID()
	}
	if crew == "" {
		say(stderr, "crew: init: needs a crew id (usage: crew init [<id>])\n")
		return exitFailure
	}
	if !bus.ValidCrewID(crew) {
		say(stderr, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n")
		return exitFailure
	}
	bc, err := paths.Crew(crew)
	switch {
	case err == nil && bc.Layout != 2 && legacy(paths, bc):
		say(stderr, "crew: init: crew '%s' already exists at layout 1\n", crew)
		return exitFailure
	case err != nil && !errors.Is(err, bus.ErrNotRepo):
		say(stderr, "crew: init: %v\n", err)
		return exitFailure
	}
	if paths.State == "" {
		say(stderr, "crew: init: needs XDG_STATE_HOME or HOME\n")
		return exitFailure
	}
	m, err := newMeta(paths, crew, o)
	if err != nil {
		say(stderr, "crew: init: %v\n", err)
		return exitFailure
	}
	m.CreatedBy = "init"
	created := true
	if err := bus.CreateMeta(paths.State, m); errors.Is(err, bus.ErrMetaExists) {
		created = false
	} else if err != nil {
		say(stderr, "crew: init: %v\n", err)
		return exitFailure
	}
	bc, err = paths.Crew(crew)
	if err != nil || bc.Layout != 2 {
		say(stderr, "crew: init: crew '%s' did not resolve to a layout-2 dir after creation (%v)\n", crew, err)
		return exitFailure
	}
	dir, log := bc.Dir, bc.Log
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		say(stderr, "crew: init: %v\n", err)
		return exitFailure
	}
	if err := f.Close(); err != nil {
		say(stderr, "crew: init: %v\n", err)
		return exitFailure
	}
	out, _ := json.Marshal(InitResult{ID: crew, Dir: dir, Log: log, Created: created})
	_, _ = stdout.Write(append(out, '\n'))
	return 0
}
