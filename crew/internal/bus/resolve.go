package bus

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// PathsFor is the bus layout of the repo whose absolute git common dir is common,
// with no state root: its crews resolve as layout 1 only.
func PathsFor(common string) Paths {
	dir := common + "/crew"
	return Paths{Common: common, Dir: dir, Log: dir + "/events.jsonl"}
}

// WithState is p resolving layout-2 crews under the state root state.
func (p Paths) WithState(state string) Paths {
	p.State = state
	return p
}

// StatePaths is the bus outside a repo: only layout-2 crews resolve.
func StatePaths(state string) Paths { return Paths{State: state} }

// CrewsDir is the directory holding one subdirectory per crew, for code that
// enumerates a repo's crews.
func (p Paths) CrewsDir() string { return p.Dir + "/crews" }

// CrewDir is the per-crew directory for id. Validate untrusted ids with ValidCrewID first.
func (p Paths) CrewDir(id string) string { return p.CrewsDir() + "/" + id }

// Crew is where one crew keeps its state: the log its events go to, its own
// directory, the repo-local directory (artifacts, grants, repo locks) and the
// root its crew-scoped stores (marks, holds, locks) live under.
type Crew struct {
	ID      string
	Log     string
	Dir     string
	RepoDir string // "" for a layout-2 crew resolved outside a repo
	State   string // layout 1: the repo's bus dir; layout 2: Dir
	Layout  int    // 1 or 2
}

// InvalidCrewIDError means an id fails ValidCrewID.
type InvalidCrewIDError struct{ ID string }

func (e *InvalidCrewIDError) Error() string { return fmt.Sprintf("invalid crew id %q", e.ID) }

// Crew classifies id, in order: invalid; a crews/<id>/meta.json under the state
// root is layout 2; an archive/<id>/meta.json is *ArchivedError; an id a repo
// index lists is *IndexOnlyError; anything else is layout 1, which needs a
// repo. The state-root steps run only when p.State is set. Nothing is created
// or written on any branch.
func (p Paths) Crew(id string) (Crew, error) {
	if !ValidCrewID(id) {
		return Crew{}, &InvalidCrewIDError{ID: id}
	}
	if p.State != "" {
		if dir := StoreDir(p.State, id); isFile(metaPath(dir)) {
			return Crew{ID: id, Log: dir + "/events.jsonl", Dir: dir, RepoDir: p.Dir, State: dir, Layout: 2}, nil
		}
		if isFile(metaPath(ArchiveDir(p.State, id))) {
			return Crew{}, &ArchivedError{ID: id}
		}
		if IndexListed(p.State, id) {
			return Crew{}, &IndexOnlyError{ID: id, Live: liveIDs(p.State)}
		}
	}
	if p.Common == "" {
		return Crew{}, ErrNotRepo
	}
	return Crew{ID: id, Log: p.Log, Dir: p.CrewDir(id), RepoDir: p.Dir, State: p.Dir, Layout: 1}, nil
}

// ErrNoCrew means no source named a crew id.
var ErrNoCrew = errors.New("no crew context — pass --crew <id>")

// Lookup is where ResolveCrewID looks, in order: Flag, the task doc and env
// (env first when EnvFirst), then Window. A nil Window is never called.
type Lookup struct {
	Flag     string
	EnvFirst bool
	Window   func(context.Context) string
}

// ResolveCrewID returns the crew id for cwd. A non-empty id that fails
// ValidCrewID is returned alongside *InvalidCrewIDError so a caller with its
// own message keeps the id; nothing found is ErrNoCrew.
func ResolveCrewID(ctx context.Context, cwd string, l Lookup) (string, error) {
	id := l.Flag
	if id == "" {
		if l.EnvFirst {
			id = CrewIDEnvFirst(ctx, cwd)
		} else {
			id = CrewID(ctx, cwd)
		}
	}
	if id == "" && l.Window != nil {
		id = l.Window(ctx)
	}
	switch {
	case id == "":
		return "", ErrNoCrew
	case !ValidCrewID(id):
		return id, &InvalidCrewIDError{ID: id}
	}
	return id, nil
}

// WindowCrewID reads @crew_id of the window holding $TMUX_PANE, "" when there
// is no pane, no client or tmux fails.
func WindowCrewID(getenv func(string) string, c tmux.Client) func(context.Context) string {
	return func(ctx context.Context) string {
		pane := getenv("TMUX_PANE")
		if pane == "" || c == nil {
			return ""
		}
		return strings.TrimRight(tmux.Output(ctx, c, "display-message", "-p", "-t", pane, "#{@crew_id}"), "\n")
	}
}

// RepoTag is the realpath of a git common dir, the key a repo is known by
// outside its own checkout.
func RepoTag(common string) (string, error) {
	real, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}
