// Package bus locates a repo's crew bus, resolves the caller's crew id and
// reads the event log with the semantics of crew.sh and `jq -s`.
package bus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// ErrNotRepo means the directory is not inside a git repository.
var ErrNotRepo = errors.New("not in a git repo")

// ErrNoLog means the log is missing or not a regular file, which crew.sh's
// `[ -f "$log" ]` treats alike: callers take the absent-log path.
var ErrNoLog = errors.New("no event log")

// OpenError means the log is a regular file that could not be opened or read.
type OpenError struct {
	Path string
	Err  error
}

func (e *OpenError) Error() string { return fmt.Sprintf("cannot read %s: %v", e.Path, e.Err) }
func (e *OpenError) Unwrap() error { return e.Err }

// DecodeError means the log is not a valid JSON stream.
type DecodeError struct {
	Path string
	Err  error
}

func (e *DecodeError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }
func (e *DecodeError) Unwrap() error { return e.Err }

// Kind is an event's `kind`. An unrecognised value is kept as-is.
type Kind string

const (
	KindStatus    Kind = "status"
	KindMsg       Kind = "msg"
	KindDispatch  Kind = "dispatch"
	KindResume    Kind = "resume"
	KindNudge     Kind = "nudge"
	KindNudgeWait Kind = "nudge_wait"
	KindReap      Kind = "reap"
	KindRelease   Kind = "release"
	KindClaim     Kind = "claim"
	KindReclaim   Kind = "reclaim"
)

// State is a status event's `body.state`.
type State string

const (
	StateWorking State = "working"
	StateBlocked State = "blocked"
	StatePROpen  State = "pr_open"
	StateDone    State = "done"
	StateFailed  State = "failed"
	StateExited  State = "exited"
)

// Terminal reports whether a session in this state is finished, as crew.sh's
// `["done","failed","exited"] | index(...)` does.
func (s State) Terminal() bool {
	switch s {
	case StateDone, StateFailed, StateExited:
		return true
	case StateWorking, StateBlocked, StatePROpen:
		return false
	}
	return false
}

// Event is one value of the log. A log line may be any JSON value, because jq
// slurps a bare `5` or `null` too. Raw always holds the value as read, with
// every field and the original key order, so it re-encodes byte-faithfully;
// folds that must reproduce jq's type errors work from it. The typed fields are
// conveniences for object events: a string field that is absent or not a string
// reads as "", an absent TS or Body as null, and a non-object value leaves all
// of them empty.
type Event struct {
	TS      jsonv.Value
	CrewID  string
	Kind    Kind
	From    string
	To      string
	Branch  string
	Session string
	Body    jsonv.Value
	Raw     jsonv.Value
}

func newEvent(raw jsonv.Value) Event {
	e := Event{Raw: raw}
	if raw.Kind() != jsonv.KindObject {
		return e
	}
	e.TS, _ = raw.Get("ts")
	e.Body, _ = raw.Get("body")
	e.CrewID = stringField(raw, "crew_id")
	e.Kind = Kind(stringField(raw, "kind"))
	e.From = stringField(raw, "from")
	e.To = stringField(raw, "to")
	e.Branch = stringField(raw, "branch")
	e.Session = stringField(raw, "session")
	return e
}

func stringField(obj jsonv.Value, key string) string {
	v, _ := obj.Get(key)
	s, _ := v.AsString()
	return s
}

// ReadEvents reads the whole log as `jq -s` does. A missing log, or one that is
// not a regular file (stat follows symlinks, like `[ -f ]`), is ErrNoLog.
func ReadEvents(path string) ([]Event, error) {
	evs, err := ReadEventsTolerant(path)
	if err != nil {
		return nil, err
	}
	return evs, nil
}

// ReadEventsTolerant is ReadEvents with `jq -r`'s tolerance for a torn tail:
// on a decode error it still returns the events parsed before the break,
// alongside the *DecodeError, so a best-effort reader (the crews arm's id
// scan) keeps the well-formed prefix the way `jq ... 2>/dev/null || true`
// did. Every other outcome is identical.
func ReadEventsTolerant(path string) ([]Event, error) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, ErrNoLog
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, &OpenError{Path: path, Err: err}
	}
	defer func() { _ = f.Close() }()
	vs, decErr := jsonv.DecodeStreamPrefix(f)
	evs := make([]Event, len(vs))
	for i, v := range vs {
		evs[i] = newEvent(v)
	}
	if decErr != nil {
		var syn *jsonv.SyntaxError
		if errors.As(decErr, &syn) {
			return evs, &DecodeError{Path: path, Err: decErr}
		}
		return nil, &OpenError{Path: path, Err: decErr}
	}
	return evs, nil
}

// Paths is where a repo keeps its crew bus.
type Paths struct {
	Common string // absolute git common dir, identical from a checkout and its worktrees
	Dir    string // Common + "/crew"
	Log    string // Dir + "/events.jsonl"
}

func newPaths(common string) Paths {
	dir := common + "/crew"
	return Paths{Common: common, Dir: dir, Log: dir + "/events.jsonl"}
}

// CrewDir is the per-crew directory for id. Validate untrusted ids with ValidCrewID first.
func (p Paths) CrewDir(id string) string { return p.Dir + "/crews/" + id }

// Locate resolves the bus of the repo containing cwd, as crew.sh's preamble does.
func Locate(ctx context.Context, cwd string) (Paths, error) {
	common := gitOutput(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == "" {
		return Paths{}, ErrNotRepo
	}
	return newPaths(common), nil
}

// gitOutput is `$(git -C cwd args... 2>/dev/null || true)`: stderr dropped,
// failure and trailing newlines gone.
func gitOutput(ctx context.Context, cwd string, args ...string) string {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// ValidCrewID mirrors crew.sh's `*[!A-Za-z0-9._-]* | -* | . | ..` rejection. It
// also rejects "", which crew.sh checks for separately before that case.
func ValidCrewID(id string) bool {
	if id == "" || id == "." || id == ".." || id[0] == '-' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// CrewID is crew.sh's `_crew_id`: the first `^crew_id:` line of the checkout's
// WORKER_TASK.md wins, then env CREW_ID, else "". The field is
// `cut -d' ' -f2`: a line with no space is returned whole, "crew_id:  x" gives
// an empty field, and a CR from CRLF is kept.
func CrewID(ctx context.Context, cwd string) string {
	if top := gitOutput(ctx, cwd, "rev-parse", "--show-toplevel"); top != "" {
		if id := taskCrewID(top + "/WORKER_TASK.md"); id != "" {
			return id
		}
	}
	return os.Getenv("CREW_ID")
}

// taskCrewID returns "" when the file is not a regular file, cannot be read or
// has no usable id: `[ -f ]` and the `|| true` after the pipeline make all of
// those fall through to the env.
func taskCrewID(path string) string {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "crew_id:") {
			continue
		}
		_, rest, found := strings.Cut(line, " ")
		if !found {
			return line
		}
		field, _, _ := strings.Cut(rest, " ")
		return field
	}
	return ""
}
