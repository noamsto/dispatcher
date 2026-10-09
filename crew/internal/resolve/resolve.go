// Package resolve is `crew resolve-target`: what a target names on the bus —
// the branch, codename, host and crew behind a `#N`/`N`, a Linear id, a branch,
// a codename or a `worker:<branch>#<session>` id — folded from the latest
// `dispatch` row of each branch by the arm's own jq (resolve.jq).
//
// The fold reads with the arm's tolerance rather than the shared fold's: the
// bash helper ran jq under `2>/dev/null || true`, so a torn line is skipped and
// a missing, unreadable or wholly corrupt bus is simply "nothing matches". These
// two arms therefore never exit 2 or 5 off the bus; 2 is ambiguity alone.
//
// The rows are unauthenticated (workers can append to the bus): a caller that
// acts on the result still gates on the dispatcher-written worktree record.
package resolve

import (
	"fmt"
	"io"
	"os"
	"strings"

	_ "embed"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed resolve.jq
var program string

const (
	exitFailure   = 1
	exitAmbiguous = 2
)

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Row is one branch the target names: the arm's four `@tsv` columns, still in
// the escaped form `@tsv` wrote (it escapes any tab or newline inside a field,
// which is why splitting on a raw tab is exact).
type Row struct {
	Branch string
	Name   string
	Host   string
	CrewID string
}

// line is the arm's stdout: the four columns as `jq -r` printed them.
func (r Row) line() string {
	return strings.Join([]string{r.Branch, r.Name, r.Host, r.CrewID}, "\t")
}

// Rows is the log `_resolve_target` reads: `jq -R` with `fromjson?`, so a line
// that does not decode to exactly one value is skipped and its neighbours still
// resolve. A missing, non-regular or unreadable log is no rows at all, which is
// the arm's `[ -f "$log" ]` miss and its swallowed open failure.
func Rows(path string) []jsonv.Value {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rows []jsonv.Value
	for _, line := range strings.Split(string(data), "\n") {
		vs, err := jsonv.DecodeStream(strings.NewReader(line))
		if err != nil || len(vs) != 1 {
			continue
		}
		rows = append(rows, vs[0])
	}
	return rows
}

// Resolve is `_resolve_target <target> <crew>`: one Row per branch the target
// names, newest `dispatch` row per branch, in the fold's branch-sorted order —
// the order the ambiguity list and `where`'s branch join are printed in. A fold
// that fails on this bus is the arm's swallowed jq failure: no rows.
func Resolve(rows []jsonv.Value, target, crew string) []Row {
	out, err := jqrun.Run(program, rows, 0, map[string]jsonv.Value{
		"t": jsonv.Str(target),
		"c": jsonv.Str(crew),
	})
	if err != nil || out.Kind() != jsonv.KindArray {
		return nil
	}
	var matched []Row
	for _, v := range out.Elems() {
		s, ok := v.AsString()
		if !ok {
			continue
		}
		f := strings.Split(s, "\t")
		if len(f) != 4 {
			continue
		}
		matched = append(matched, Row{Branch: f[0], Name: f[1], Host: f[2], CrewID: f[3]})
	}
	return matched
}

// Run is the `resolve-target` arm.
func Run(argv []string, paths bus.Paths, stdout, stderr io.Writer) int {
	crew, target, msg := parse(argv)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitFailure
	}
	rows := Resolve(Rows(paths.Log), target, crew)
	switch len(rows) {
	case 0:
		say(stderr, "crew: resolve-target: no worker matches '%s'\n", target)
		return exitFailure
	case 1:
		say(stdout, "%s\n", rows[0].line())
		return 0
	default:
		branches := make([]string, len(rows))
		for i, r := range rows {
			branches[i] = r.Branch
		}
		// `paste -sd, -` then `sed 's/,/, /g'`: the sed spaces every comma in the
		// joined line, one inside a branch name included (a legal ref character).
		say(stderr, "crew: resolve-target: ambiguous target '%s' — matches: %s; pass a branch\n",
			target, strings.ReplaceAll(strings.Join(branches, ","), ",", ", "))
		return exitAmbiguous
	}
}

// parse is the arm's flag loop, in its order: a second positional is refused
// where it appears, not at the end. Unlike `where`, this arm has no `_crew_id`
// default — `--crew` or nothing.
func parse(args []string) (crew, target, msg string) {
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--crew":
			if len(args) < 2 || args[1] == "" {
				return "", "", "crew: resolve-target: --crew needs an id"
			}
			crew, args = args[1], args[2:]
		case strings.HasPrefix(arg, "--"):
			return "", "", fmt.Sprintf("crew: resolve-target: unknown flag '%s'", arg)
		default:
			if target != "" {
				return "", "", "crew: resolve-target: one target only"
			}
			target, args = arg, args[1:]
		}
	}
	if target == "" {
		return "", "", "usage: crew resolve-target <target> [--crew ID]"
	}
	return crew, target, ""
}
