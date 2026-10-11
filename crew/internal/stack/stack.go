// Package stack is `crew stack cut`, the recorded cut point of a stacked child,
// and `crew stack align`, the read-only plan for rebasing a whole stack from its
// top PR down to the default branch (#984, #989;
// DISPATCHER_PROTOCOL.md → "Rebase coordination").
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
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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
	usage = "usage: crew stack cut [--record] <child-branch|PR#> [<oid>]\n" +
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

// Run is crew.sh's `stack)` arm.
func Run(ctx context.Context, args []string, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	sub, rest := "", args
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	var (
		record          bool
		child, oid, top string
	)
	switch sub {
	case "cut":
		if len(rest) > 0 && rest[0] == "--record" {
			record, rest = true, rest[1:]
		}
		switch len(rest) {
		case 1:
			child = rest[0]
		case 2:
			// The arm only took an oid with --record; `cut child oid` is usage.
			if record {
				child, oid = rest[0], rest[1]
			}
		}
	case "align":
		if len(rest) == 1 {
			top = rest[0]
		}
	}
	if child == "" && top == "" {
		_, _ = fmt.Fprint(stderr, usage)
		return 1
	}
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
	if s.p.Gh == nil || s.p.Append == nil || s.p.Now == nil {
		s.p = s.p.withDefaults()
	}
	if top == "" {
		err = s.cut(ctx, record, child, oid)
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
	out, err := jqrun.Run(newestProg, stringValues(lines), 0, map[string]jsonv.Value{
		"crew": jsonv.Str(s.crew), "child": jsonv.Str(branch), "df": jsonv.Str(df), "sf": jsonv.Str(sf),
	}, jqrun.WithJQFromJSON())
	if err != nil {
		// jq itself failing is the arm's `_stack_bus_fail`; its empty output is
		// the patched "" (newest.jq).
		return "", errBus
	}
	v, _ := out.AsString()
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
	out, err := jqrun.Run(recordProg, stringValues(lines), 0, map[string]jsonv.Value{
		"crew": jsonv.Str(s.crew), "b": jsonv.Str(branch),
	}, jqrun.WithJQFromJSON())
	if err != nil {
		return "", "", errBus
	}
	parent = jqString(out, "p")
	cut = jqString(out, "c")
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

// isNumber is the arm's `case "$x" in ”|*[!0-9]*)` test, inverted: non-empty
// and all digits.
func isNumber(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func stringValues(lines []string) []jsonv.Value {
	out := make([]jsonv.Value, len(lines))
	for i, l := range lines {
		out[i] = jsonv.Str(l)
	}
	return out
}

// isFalse is the arm's `[ "$(jq -r '.k' …)" != false ]`: only a JSON false reads
// as false. A missing key prints jq's "null" and a string spelled "false" is not
// a JSON false, so both are refused.
func isFalse(v jsonv.Value, key string) bool {
	f, ok := v.Get(key)
	return ok && f.Kind() == jsonv.KindFalse
}

// trimOne is bash's `${x#pat}`: one leading occurrence, not every one.
func trimOne(s, prefix string) string { return strings.TrimPrefix(s, prefix) }

func member(key string, v jsonv.Value) jsonv.Member { return jsonv.Member{Key: key, Val: v} }

func jsonvAppend(v jsonv.Value) []byte { return jsonv.Append(nil, v, jsonv.Options{}) }

// jqString is `jq -r '.k // empty'` over one object: a string is itself, null
// and an absent key are "", anything else is the text jq would print.
func jqString(v jsonv.Value, key string) string {
	f, ok := v.Get(key)
	if !ok || f.IsNull() {
		return ""
	}
	if s, ok := f.AsString(); ok {
		return s
	}
	return jqTostring(f)
}

// jqTostring is `jq '.k | tostring'`.
func jqTostring(v jsonv.Value) string {
	switch v.Kind() {
	case jsonv.KindNull:
		return "null"
	case jsonv.KindString:
		s, _ := v.AsString()
		return s
	case jsonv.KindNumber:
		return v.NumberText()
	case jsonv.KindTrue:
		return "true"
	case jsonv.KindFalse:
		return "false"
	case jsonv.KindArray, jsonv.KindObject:
		return string(jsonv.Append(nil, v, jsonv.Options{}))
	}
	// Every jsonv.Kind is cased above, so this tail is unreachable; the compiler
	// needs a return and `exhaustive` forbids a default (rate/burn.go, same shape).
	return ""
}

// bashQuote is the `%q` of the arm's refusal: bash's C-locale quoting, which
// gitguard already implements.
func bashQuote(s string) string { return gitguard.Quote(s) }

// parseJSON is the arm's `jq -r … <<<"$json"`: output gh did not print as JSON
// reads as no field at all, which every caller refuses.
func parseJSON(s string) (jsonv.Value, bool) {
	v, err := jsonv.ParseOne(s)
	if err != nil || v.Kind() != jsonv.KindObject {
		return jsonv.Null(), false
	}
	return v, true
}
