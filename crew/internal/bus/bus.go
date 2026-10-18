// Package bus locates a repo's crew bus, resolves the caller's crew id and
// reads the event log with the semantics of crew.sh and `jq -s`.
package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
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

// jq's own statuses for a log it cannot use.
const (
	exitUnreadable = 2
	exitCorrupt    = 5
)

// JQFailure maps a ReadEvents/ReadEventsTolerant error to how jq would fail on
// the same file: the message it would print, minus the path its caller prints
// itself, and the status it would take — 2 unreadable, 5 corrupt. A nil error
// and ErrNoLog (the arm's `[ -f "$log" ]`, which never starts jq) are not
// failures.
func JQFailure(err error) (string, int) {
	var open *OpenError
	var decode *DecodeError
	switch {
	case err == nil || errors.Is(err, ErrNoLog):
		return "", 0
	case errors.As(err, &open):
		var pe *fs.PathError
		if errors.As(open.Err, &pe) {
			return pe.Err.Error(), exitUnreadable
		}
		return open.Err.Error(), exitUnreadable
	case errors.As(err, &decode):
		// The caller names the path, so the wrapper's copy is dropped.
		return decode.Err.Error(), exitCorrupt
	default:
		return err.Error(), exitCorrupt
	}
}

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
	StateReaped  State = "reaped"
)

// Terminal reports whether a session in this state is finished, as crew.sh's
// `["done","failed","exited"] | index(...)` does, plus reaped (posted only by `crew reap`).
func (s State) Terminal() bool {
	switch s {
	case StateDone, StateFailed, StateExited, StateReaped:
		return true
	case StateWorking, StateBlocked, StatePROpen:
		return false
	}
	return false
}

// Event is one value of the log. A log line may be any JSON value, because jq
// slurps a bare `5` or `null` too. Doc holds the value as read in gojq's form
// (json.Number literals, []any, map[string]any); key order is not kept. The
// typed fields are conveniences for object events: a string field that is absent
// or not a string reads as "", and a non-object value leaves all of them empty.
type Event struct {
	CrewID  string
	Kind    Kind
	From    string
	To      string
	Branch  string
	Session string
	Doc     any
}

func newEvent(doc any) Event {
	e := Event{Doc: doc}
	obj, ok := doc.(map[string]any)
	if !ok {
		return e
	}
	e.CrewID = stringField(obj, "crew_id")
	e.Kind = Kind(stringField(obj, "kind"))
	e.From = stringField(obj, "from")
	e.To = stringField(obj, "to")
	e.Branch = stringField(obj, "branch")
	e.Session = stringField(obj, "session")
	return e
}

func stringField(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return s
}

// decodeValues reads every JSON value in data, in gojq's form. On a syntax error
// or a torn tail it returns the values before it alongside the error. NaN and
// Infinity are not JSON, so a value holding one is a syntax error.
func decodeValues(data []byte) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var vs []any
	for {
		var v any
		switch err := dec.Decode(&v); err {
		case nil:
			vs = append(vs, v)
		case io.EOF:
			return vs, nil
		default:
			return vs, err
		}
	}
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &OpenError{Path: path, Err: err}
	}
	vs, decErr := decodeValues(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	evs := make([]Event, len(vs))
	for i, v := range vs {
		evs[i] = newEvent(v)
	}
	if decErr != nil {
		return evs, &DecodeError{Path: path, Err: decErr}
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

// Toplevel is `git rev-parse --show-toplevel` from cwd, "" outside a checkout.
func Toplevel(ctx context.Context, cwd string) string {
	return gitOutput(ctx, cwd, "rev-parse", "--show-toplevel")
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

// sessionRe is crew.sh's `_is_session_id` pattern, `^s[0-9]+-[0-9]+$`. \A..\z
// rather than ^..$: Go's `$` is end-of-text, which is what bash's ERE means here
// too (no REG_NEWLINE), and it keeps a trailing newline from looking like a session.
var sessionRe = regexp.MustCompile(`\As[0-9]+-[0-9]+\z`)

// IsSessionID is crew.sh's `_is_session_id`: the text after the LAST '#' has the
// sid shape. A '#' inside a branch name (legal in git) does not match, so
// `worker:feat/a#b` is branch-only while `worker:feat/a#b#s1-1` is sessioned.
func IsSessionID(id string) bool {
	return sessionRe.MatchString(id[strings.LastIndex(id, "#")+1:])
}

// CrewID is crew.sh's `_crew_id`: the first `^crew_id:` line of the checkout's
// WORKER_TASK.md wins, then env CREW_ID, else "". The field is
// `cut -d' ' -f2`: a line with no space is returned whole, "crew_id:  x" gives
// an empty field, and a CR from CRLF is kept.
func CrewID(ctx context.Context, cwd string) string {
	if top := Toplevel(ctx, cwd); top != "" {
		if id := taskCrewID(top + "/WORKER_TASK.md"); id != "" {
			return id
		}
	}
	return os.Getenv("CREW_ID")
}

// CrewIDEnvFirst is CrewID with the launcher's env preferred over the checkout:
// for a process started with CREW_ID in its environment and cwd inside a
// worktree its own watched worker can rewrite, the task doc is an argument that
// worker could edit, not the anchor the launcher passed.
func CrewIDEnvFirst(ctx context.Context, cwd string) string {
	if id := os.Getenv("CREW_ID"); id != "" {
		return id
	}
	return CrewID(ctx, cwd)
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
