// Package stack is `crew stack cut`, the recorded cut point of a stacked child,
// and `crew stack align`, the read-only plan for rebasing a whole stack from its
// top PR down to the default branch (#984, #989;
// the dispatcher:stacked skill).
//
// The trust rule is why the command is more than a `git rev-parse` away: only
// the dispatcher-written kinds are consulted (a dispatch row with a base_oid,
// and a stack_cut row), never a worker's status/msg rows; every branch name that
// reaches a refspec, a fetch or a directive is validated first; and every oid
// comes from our own anchored fetch, never from gh's headRefOid.
//
// Like `reap`, Run changes the process's working directory to the trusted cwd
// before any crew-id, gh or bus read, so a tampered worker worktree cannot
// steer which crew, PR or rows decide the cut (#633).
package stack

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// The arm's jq folds; each file's header documents its patches.
var (
	//go:embed newest.jq
	newestProg string
	//go:embed record.jq
	recordProg string
)

const (
	// Usage is the two lines every stack grammar refusal prints.
	Usage = "usage: crew stack cut [--record] <child-branch|PR#> [<oid>]\n" +
		"       crew stack align <top-PR#|branch>\n"
	noCrewID = "crew: no crew id — CREW_ID unset and no WORKER_TASK.md crew_id; " +
		"run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, " +
		"or 'crew new' to start one\n"
	// busFailure is the arm's `_stack_bus_fail` line.
	busFailure = "crew: cannot read the bus\n"
)

// oidRe is the arm's `^[0-9a-f]{40}([0-9a-f]{24})?$`: a full sha1 or sha256 oid,
// lowercase hex, nothing else. A short oid, a ref name and an uppercase spell
// are all refused before git sees them.
var oidRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// safeNameRe is `_stack_safe_name`'s second gate: a name that is safe to put in
// a directive a human or an agent will paste into a shell.
var safeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// errRefused marks a refusal already printed on stderr; errBus marks the log
// that would not open, which Run reports with the arm's one line.
var (
	errRefused = errors.New("refused")
	errBus     = errors.New("bus")
)

// stack is one `crew stack`: the parsed argv plus everything both subcommands
// read (the trusted cwd, the crew id, the bus, the memoised fetches).
type stackCmd struct {
	paths          bus.Paths
	stdout, stderr io.Writer
	o              wtgit.Opts
	p              Probes
	crew           string
	lines          []string
	logRead        bool
	logErr         bool
	fetched        map[string]string
	defaultBranch  string
}

// Request is a parsed `crew stack` command line: Top names an align, and
// otherwise Child (with Record and Oid) names a cut.
type Request struct {
	Record bool
	Child  string
	Oid    string
	Top    string
}

// Run is crew.sh's `stack)` arm.
func Run(ctx context.Context, req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	child, oid, top := req.Child, req.Oid, req.Top
	if oid != "" && !oidRe.MatchString(oid) {
		_, _ = fmt.Fprintf(stderr, "crew: '%s' is not a full lowercase hex commit oid\n", oid)
		return 1
	}

	// Before any crew-id, gh or bus read: a tampered worker worktree as cwd
	// must not steer which crew, PR or rows decide the cut (#633).
	dir, err := wtgit.TrustedCwd(paths.Common, cwd, stderr)
	if err == nil {
		err = os.Chdir(dir)
		cwd = dir
	}
	if err != nil {
		if !errors.Is(err, gitguard.ErrRefused) {
			_, _ = fmt.Fprintf(stderr, "crew: stack: %v\n", err)
		}
		return 1
	}
	crew := bus.CrewID(ctx, cwd)
	if crew == "" {
		_, _ = fmt.Fprint(stderr, noCrewID)
		return 1
	}

	s := &stackCmd{
		paths: paths, stdout: stdout, stderr: stderr, o: wtgit.Opts{Stderr: stderr},
		p: o.Probes, crew: crew, fetched: map[string]string{},
	}
	if s.p.Gh == nil || s.p.Append == nil || s.p.Clock.Now == nil {
		s.p = s.p.withDefaults()
	}
	if top == "" {
		err = s.cut(ctx, req.Record, child, oid)
	} else {
		err = s.align(ctx, top)
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errBus):
		_, _ = fmt.Fprint(stderr, busFailure)
		return 1
	case errors.Is(err, errRefused):
		return 1
	default:
		_, _ = fmt.Fprintf(stderr, "crew: stack: %v\n", err)
		return 1
	}
}

// busLines is `$stack_src`: the log's lines, or nothing when there is no log
// file (`[ -f "$stack_src" ] || stack_src=/dev/null`). A regular file that will
// not open is the arm's jq failure, and the arm's jq failure was `_stack_bus_fail`.
func (s *stackCmd) busLines() ([]string, error) {
	if s.logRead {
		if s.logErr {
			return nil, errBus
		}
		return s.lines, nil
	}
	s.logRead = true
	if st, err := os.Stat(s.paths.Log); err != nil || !st.Mode().IsRegular() {
		return nil, nil
	}
	data, err := os.ReadFile(s.paths.Log)
	if err != nil {
		s.logErr = true
		return nil, errBus
	}
	s.lines = splitLines(string(data))
	return s.lines, nil
}

// splitLines is `jq -R`'s record split: a trailing newline ends the last
// record and does not add an empty one.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// newest is `_stack_newest <branch> <dispatch field> <stack_cut field>`: the
// newest dispatcher-written value of that pair, "" when there is none.
func (s *stackCmd) newest(branch, df, sf string) (string, error) {
	lines, err := s.busLines()
	if err != nil {
		return "", err
	}
	out, err := jqrun.RunAny(newestProg, stringValues(lines), 0, map[string]any{
		"crew": s.crew, "child": branch, "df": df, "sf": sf,
	}, jqrun.WithJQFromJSON())
	if err != nil {
		// jq itself failing is the arm's `_stack_bus_fail`; its empty output is
		// the patched "" (newest.jq).
		return "", errBus
	}
	v, _ := out.(string)
	return v, nil
}

// recordedCut is `_stack_recorded_cut`: the recorded cut, only when it is a
// full oid.
func (s *stackCmd) recordedCut(branch string) (string, error) {
	c, err := s.newest(branch, "base_oid", "cut")
	if err != nil {
		return "", err
	}
	if !oidRe.MatchString(c) {
		return "", nil
	}
	return c, nil
}

// recordedParent is `_stack_recorded_parent`.
func (s *stackCmd) recordedParent(branch string) (string, error) {
	return s.newest(branch, "base", "parent")
}

// record is `_stack_align_record`: one layer's parent and cut, from the newest
// row carrying both. A cut that is not a full oid is no cut, and a parent no
// directive can carry is a refusal.
func (s *stackCmd) record(branch string) (parent, cut string, err error) {
	lines, err := s.busLines()
	if err != nil {
		return "", "", err
	}
	out, err := jqrun.RunAny(recordProg, stringValues(lines), 0, map[string]any{
		"crew": s.crew, "b": branch,
	}, jqrun.WithJQFromJSON())
	if err != nil {
		return "", "", errBus
	}
	m, _ := out.(map[string]any)
	parent, _ = m["p"].(string)
	cut, _ = m["c"].(string)
	if !oidRe.MatchString(cut) {
		cut = ""
	}
	if parent != "" {
		if err := s.safeName(parent, "the recorded parent of "+branch); err != nil {
			return "", "", err
		}
	}
	return parent, cut, nil
}

// safeName is `_stack_safe_name`: unless the name is safe to put in a directive,
// it is a refusal.
func (s *stackCmd) safeName(name, what string) error {
	if wtgit.PlainBranchName(context.Background(), name, s.o) && safeNameRe.MatchString(name) {
		return nil
	}
	return s.refuse("crew: %s has a branch name unsafe to put in a directive: %s\n", what, bashQuote(name))
}

// refuse prints the arm's refusal line and returns the error that maps to exit
// 1 with stdout untouched.
func (s *stackCmd) refuse(format string, args ...any) error {
	_, _ = fmt.Fprintf(s.stderr, format, args...)
	return errRefused
}

// short is bash's `${x:0:12}`: the first twelve bytes of an oid, whole when it
// is shorter.
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func stringValues(lines []string) []any {
	out := make([]any, len(lines))
	for i, l := range lines {
		out[i] = l
	}
	return out
}

// trimOne is bash's `${x#pat}`: one leading occurrence, not every one.
func trimOne(s, prefix string) string { return strings.TrimPrefix(s, prefix) }

// bashQuote is the `%q` of the arm's refusal: bash's C-locale quoting, which
// gitguard already implements.
func bashQuote(s string) string { return gitguard.Quote(s) }

// decodeGh is the arm's `jq -r … <<<"$json"`: output gh did not print as the
// expected JSON object reads as no field at all, which every caller refuses.
func decodeGh(out string, v any) bool { return json.Unmarshal([]byte(out), v) == nil }

// marshalRow encodes a row as one line without escaping <, > or &, as jq -c
// does.
func marshalRow(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}
