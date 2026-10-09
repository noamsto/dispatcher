// Package hold is `crew hold`: a queued dispatch parked on a quota window, so a
// successor session can resume it without a human. Records go to the synthetic
// sink `hold:<crew>`, matching `retro`/`metrics` — `watch`'s `to==$me or to=="*"`
// predicate matches neither, so a hold cannot wake or pollute the inbox of the
// dispatcher that wrote it, while `crew log` still shows it.
//
// The read side (`list`, `due`, `park`) runs the arm's own jq, embedded and
// handed to jqrun: `_hold_outstanding` stays in bash for `roster-render`, so its
// program is the contract between the two and copying its meaning into Go would
// only create a second thing to drift.
//
// The write side (`add`, `release`) cannot be jq at all. The row's `body` is
// `{...} | tostring`, and gojq returns object keys sorted where jq keeps
// construction order — and `body` is a *string*, so `jq -S` cannot sort it back
// and `crew log` prints it verbatim. It is built with jsonv, whose objects keep
// insertion order, and appended through bus.Append/bus.FitLine.
package hold

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed outstanding.jq
var outstandingProg string

//go:embed matured.jq
var maturedProg string

//go:embed park.jq
var parkProg string

//go:embed render.jq
var renderProg string

const (
	// usageAction is what the arm prints for an action it does not have.
	usageAction = "crew: hold add|list|due|park|release"
	// renderHeader is `_hold_render`'s printf, matching the fold's columns.
	renderHeader = "id\tengine\twindow\tresets_at\tref\tbranch\ttitle"
	// exitType is jq's status for a fold that fails on a row it cannot use.
	exitType    = 5
	exitFailure = 1
)

// Options is everything Run reads beyond the bus: whether $JQ_COLORS was invalid
// (jq warns once per process it starts, and the arm starts one per step, so Go
// prints the line once for the call rather than once per fold), how to resolve
// the crew the arm defaults to (`_crew_id`), the clock, and $CREW_CLOCK — the
// test-only virtual clock `_clock_now` reads seconds from.
type Options struct {
	JQColorsInvalid bool
	CrewID          func() string
	Now             func() time.Time
	CrewClock       string
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm's `case "$holdsub"`. `--help` never reaches here: the crew.sh
// preamble answers `crew hold --help` and `crew hold <action> --help` before the
// delegation arm runs.
func Run(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	action, rest := "", args
	if len(args) > 0 {
		action, rest = args[0], args[1:]
	}
	switch action {
	case "add":
		return add(rest, paths, stdout, stderr, o)
	case "list":
		return list(rest, paths, stdout, stderr, o)
	case "due":
		return due(rest, paths, stdout, stderr, o)
	case "park":
		return park(rest, paths, stdout, stderr, o)
	case "release":
		return release(rest, paths, stdout, stderr, o)
	default:
		say(stderr, "%s\n", usageAction)
		return exitFailure
	}
}

// folds runs one of the arm's jq programs over a bus, warning about a bad
// $JQ_COLORS the way the first jq of the call would and mapping a failure to the
// arm's one stderr line and status.
type folds struct {
	paths  bus.Paths
	stderr io.Writer
	o      Options
	warned bool
}

// call is one fold — the arm's one jq process, including its one $JQ_COLORS
// warning, which the folds copy prints once per invocation rather than once per
// step.
func (f *folds) call(prog string, in []jsonv.Value, vars map[string]jsonv.Value) (jsonv.Value, int) {
	if !f.warned && f.o.JQColorsInvalid {
		f.warned = true
		say(f.stderr, "Failed to set $JQ_COLORS\n")
	}
	out, err := jqrun.Run(prog, in, 0, vars)
	if err != nil {
		say(f.stderr, "crew: hold: %s: %v\n", f.paths.Log, err)
		return jsonv.Value{}, exitType
	}
	return out, 0
}

// outstanding is `_hold_outstanding`: `[]` and no jq at all where the arm's
// `[ -f "$log" ]` fails — `due` and `list` depend on that unambiguous empty-array
// read — and otherwise a strict `jq -s` read (no torn-tail tolerance) through the
// embedded program.
func (f *folds) outstanding(crew string) (jsonv.Value, int) {
	events, err := bus.ReadEvents(f.paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return jsonv.Array(), 0
	}
	if err != nil {
		msg, code := bus.JQFailure(err)
		say(f.stderr, "crew: hold: %s: %s\n", f.paths.Log, msg)
		return jsonv.Value{}, code
	}
	raws := make([]jsonv.Value, len(events))
	for i, ev := range events {
		raws[i] = ev.Raw
	}
	return f.call(outstandingProg, raws, map[string]jsonv.Value{
		"crew": jsonv.Str(crew),
		"to":   jsonv.Str("hold:" + crew),
	})
}

// `list` and `due`: `[--json] [--crew ID]` in either order, as the arm's loop
// left them. msg is the arm's stderr line when the arguments themselves fail.
// jsonFlag is false for `park` and `release`, whose loops have no `--json` case
// and fall through to their `*)` usage branch.
func parseRead(args []string, usage string, jsonFlag bool) (jsonOut bool, hcrew, msg string) {
	for len(args) > 0 {
		switch args[0] {
		case "--json":
			if !jsonFlag {
				return false, "", usage
			}
			jsonOut = true
			args = args[1:]
		case "--crew":
			if len(args) < 2 || args[1] == "" {
				return false, "", "crew: --crew needs a value"
			}
			hcrew, args = args[1], args[2:]
		default:
			return false, "", usage
		}
	}
	return jsonOut, hcrew, ""
}

func list(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	jsonOut, hcrew, msg := parseRead(args, "crew: hold list [--crew ID] [--json]", true)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitFailure
	}
	crew, ok := resolveCrew(hcrew, stderr, o)
	if !ok {
		return exitFailure
	}
	f := &folds{paths: paths, stderr: stderr, o: o}
	holds, code := f.outstanding(crew)
	if code != 0 {
		return code
	}
	if jsonOut {
		print_(stdout, holds)
		return 0
	}
	return render(f, stdout, holds)
}

func due(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	jsonOut, hcrew, msg := parseRead(args, "crew: hold due [--crew ID] [--json]", true)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitFailure
	}
	crew, ok := resolveCrew(hcrew, stderr, o)
	if !ok {
		return exitFailure
	}
	f := &folds{paths: paths, stderr: stderr, o: o}
	holds, code := f.outstanding(crew)
	if code != 0 {
		return code
	}
	// `--argjson now "$(_clock_now_f)"`: seconds, fractional unless the virtual
	// clock is set. The compare is jq's, so a hold maturing this second is due.
	matured, code := f.call(maturedProg, elems(holds), map[string]jsonv.Value{"now": o.clock().NowF()})
	if code != 0 {
		return code
	}
	if jsonOut {
		print_(stdout, matured)
	} else if code = render(f, stdout, matured); code != 0 {
		return code
	}
	// Exits 0 when any hold is matured, 1 when none — including a missing log,
	// since the outstanding fold already answered `[]` for that case.
	if matured.Kind() == jsonv.KindArray && matured.Len() > 0 {
		return 0
	}
	return exitFailure
}

func park(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	// The arm validates the default before it reads a crew or a log: empty or
	// non-digit first, then the positive test — where a value outside int64 fails
	// bash's `test` and lands in the same branch.
	defStr, rest := "", args
	if len(args) > 0 {
		defStr, rest = args[0], args[1:]
	}
	badDefault := func() int {
		say(stderr, "crew: hold park <default> must be a positive integer number of seconds\n")
		return exitFailure
	}
	if !isDigits(defStr) {
		return badDefault()
	}
	def, err := strconv.ParseInt(defStr, 10, 64)
	if err != nil || def <= 0 {
		return badDefault()
	}
	_, hcrew, msg := parseRead(rest, "crew: hold park <default> [--crew ID]", false)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitFailure
	}
	crew, ok := resolveCrew(hcrew, stderr, o)
	if !ok {
		return exitFailure
	}
	f := &folds{paths: paths, stderr: stderr, o: o}
	holds, code := f.outstanding(crew)
	if code != 0 {
		return code
	}
	out, code := f.call(parkProg, elems(holds), map[string]jsonv.Value{
		"default": jsonv.Num(float64(def)),
		"now":     o.clock().NowF(),
	})
	if code != 0 {
		return code
	}
	print_(stdout, out)
	return 0
}

// render is `_hold_render`: the header goes out before the rows' jq starts, so a
// fold that then fails still leaves it on stdout.
func render(f *folds, stdout io.Writer, holds jsonv.Value) int {
	say(stdout, "%s\n", renderHeader)
	rows, code := f.call(renderProg, elems(holds), nil)
	if code != 0 {
		return code
	}
	// The `join("\n")` patch (report.jq's) means one value for jqrun; an empty
	// string is zero rows, and `jq -r` printed no bytes for those.
	if s, _ := rows.AsString(); s != "" {
		say(stdout, "%s\n", s)
	}
	return 0
}

// addFlags is the arm's local variables for `hold add`.
type addFlags struct {
	engine, window, resetsAt, agent, ref, branch, tier, model, effort string
	plan, mcp, shape, spec, hcrew                                     string
	draft                                                             bool
	title                                                             string
}

// valueFlag maps each flag that takes a value to its variable. `--engine` is the
// engine whose quota is being waited on and `--agent` the engine the task will be
// dispatched to (task.engine): both are required and kept distinct.
func (f *addFlags) valueFlag(name string) (*string, bool) {
	for _, fl := range []struct {
		name string
		dst  *string
	}{
		{"--engine", &f.engine}, {"--window", &f.window}, {"--resets-at", &f.resetsAt},
		{"--agent", &f.agent}, {"--ref", &f.ref}, {"--branch", &f.branch},
		{"--tier", &f.tier}, {"--model", &f.model}, {"--effort", &f.effort},
		{"--plan", &f.plan}, {"--mcp", &f.mcp}, {"--shape", &f.shape},
		{"--spec", &f.spec}, {"--crew", &f.hcrew},
	} {
		if fl.name == name {
			return fl.dst, true
		}
	}
	return nil, false
}

func add(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	var fl addFlags
	for len(args) > 0 {
		if args[0] == "--draft" {
			fl.draft = true
			args = args[1:]
			continue
		}
		if dst, ok := fl.valueFlag(args[0]); ok {
			if len(args) < 2 || args[1] == "" {
				say(stderr, "crew: %s needs a value\n", args[0])
				return exitFailure
			}
			*dst = args[1]
			args = args[2:]
			continue
		}
		if strings.HasPrefix(args[0], "-") {
			say(stderr, "crew: hold add: unknown arg '%s'\n", args[0])
			return exitFailure
		}
		break
	}
	fl.title = strings.Join(args, " ")

	// Reject a missing required field by name. The order is the arm's, and it
	// decides which line a doubly-incomplete call prints.
	for _, req := range []struct{ flag, val string }{
		{"--engine", fl.engine}, {"--window", fl.window}, {"--resets-at", fl.resetsAt},
		{"--agent", fl.agent}, {"--ref", fl.ref}, {"--branch", fl.branch},
		{"--tier", fl.tier}, {"--model", fl.model}, {"--effort", fl.effort},
	} {
		if req.val == "" {
			say(stderr, "crew: hold add: %s is required\n", req.flag)
			return exitFailure
		}
	}
	if fl.title == "" {
		say(stderr, "crew: hold add: a title is required\n")
		return exitFailure
	}
	// Shape only, never the floor — `refresh-budget.sh` alone owns the 85%/95%
	// judgment. Seconds here, matching `resets_at`'s own unit; `id`/`ts` below are
	// milliseconds, a different clock.
	if !isDigits(fl.resetsAt) {
		say(stderr, "crew: hold add: --resets-at must be an integer epoch-seconds timestamp\n")
		return exitFailure
	}
	// A default outside int64 makes bash's `test` fail outright, so it lands here
	// too (minus the shell's own diagnostic, which is not ours to invent).
	resets, err := strconv.ParseInt(fl.resetsAt, 10, 64)
	if err != nil {
		say(stderr, "crew: hold add: --resets-at must be in the future\n")
		return exitFailure
	}
	if resets <= o.clock().Seconds() {
		say(stderr, "crew: hold add: --resets-at must be in the future\n")
		return exitFailure
	}
	if fl.spec != "" {
		fh, err := os.Open(fl.spec)
		if err != nil {
			say(stderr, "crew: hold add: --spec file '%s' is not readable\n", fl.spec)
			return exitFailure
		}
		_ = fh.Close()
	}
	crew, ok := resolveCrew(fl.hcrew, stderr, o)
	if !ok {
		return exitFailure
	}
	// A `hold add` with no --spec would otherwise fail its append on a repo with
	// no bus dir yet — mirrors `status`/`msg`'s own mkdir -p.
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: hold add: %v\n", err)
		return exitFailure
	}
	if o.JQColorsInvalid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	// Milliseconds, not `crew new`'s seconds: the duplicate guard compares `id`
	// against the ms `ts` fields dispatch writes, so copying `crew new` breaks
	// that comparison by 1000x. Real time even under CREW_CLOCK, because the arm
	// minted these with `jq -nc 'now*1000|floor'`.
	ts := strconv.FormatInt(o.clock().RealMS(), 10)
	id := ts + "-" + strconv.Itoa(os.Getpid())

	specpath := ""
	if fl.spec != "" {
		if err := os.MkdirAll(filepath.Join(paths.Dir, "holds"), 0o755); err != nil {
			say(stderr, "crew: hold add: %v\n", err)
			return exitFailure
		}
		specpath = filepath.Join(paths.Dir, "holds", id+".md")
		data, err := os.ReadFile(fl.spec)
		if err != nil {
			say(stderr, "crew: hold add: cannot copy --spec %s: %v\n", fl.spec, err)
			return exitFailure
		}
		if err := os.WriteFile(specpath, data, 0o644); err != nil {
			say(stderr, "crew: hold add: cannot copy --spec %s: %v\n", specpath, err)
			return exitFailure
		}
	}

	// Computed once, before the builder: FitLine calls the builder repeatedly
	// while shrinking the title, and an `id` minted inside it would stop matching
	// its own row's `ts` on any shrunk record.
	line := bus.FitLine(func(title string) string {
		return compact(addRow(crew, id, ts, &fl, specpath, title))
	}, fl.title)
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: hold add: %v\n", err)
		return exitFailure
	}
	say(stdout, "%s\n", id)
	return 0
}

// addRow is `_build_hold`: every fixed field closes over the enclosing scope and
// the title is the only shrinkable part. Handing Shrink the whole body would
// truncate `id`, `task.branch` and `task.spec` along with the title.
func addRow(crew, id, ts string, fl *addFlags, specpath, title string) jsonv.Value {
	resets, _ := jsonv.ParseNumber(digitsLiteral(fl.resetsAt))
	return jsonv.Object(
		jsonv.Member{Key: "ts", Val: tsValue(ts)},
		jsonv.Member{Key: "crew_id", Val: jsonv.Str(crew)},
		jsonv.Member{Key: "from", Val: jsonv.Str("dispatcher:" + crew)},
		jsonv.Member{Key: "to", Val: jsonv.Str("hold:" + crew)},
		jsonv.Member{Key: "kind", Val: jsonv.Str("msg")},
		jsonv.Member{Key: "body", Val: jsonv.Str(compact(jsonv.Object(
			jsonv.Member{Key: "id", Val: jsonv.Str(id)},
			jsonv.Member{Key: "wait", Val: jsonv.Object(
				jsonv.Member{Key: "engine", Val: jsonv.Str(fl.engine)},
				jsonv.Member{Key: "window", Val: jsonv.Str(fl.window)},
				jsonv.Member{Key: "resets_at", Val: resets},
			)},
			jsonv.Member{Key: "task", Val: jsonv.Object(
				jsonv.Member{Key: "ref", Val: jsonv.Str(fl.ref)},
				jsonv.Member{Key: "branch", Val: jsonv.Str(fl.branch)},
				jsonv.Member{Key: "tier", Val: jsonv.Str(fl.tier)},
				jsonv.Member{Key: "engine", Val: jsonv.Str(fl.agent)},
				jsonv.Member{Key: "model", Val: jsonv.Str(fl.model)},
				jsonv.Member{Key: "effort", Val: jsonv.Str(fl.effort)},
				jsonv.Member{Key: "plan", Val: orNull(fl.plan)},
				jsonv.Member{Key: "mcp", Val: orNull(fl.mcp)},
				jsonv.Member{Key: "draft", Val: jsonv.Bool(fl.draft)},
				jsonv.Member{Key: "shape", Val: orNull(fl.shape)},
				jsonv.Member{Key: "title", Val: jsonv.Str(title)},
				jsonv.Member{Key: "spec", Val: orNull(specpath)},
			)},
		)))},
	)
}

func release(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	if len(args) == 0 || args[0] == "" {
		say(stderr, "crew: hold release <id> [--crew ID]\n")
		return exitFailure
	}
	hid, args := args[0], args[1:]
	_, hcrew, msg := parseRead(args, "crew: hold release <id> [--crew ID]", false)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitFailure
	}
	crew, ok := resolveCrew(hcrew, stderr, o)
	if !ok {
		return exitFailure
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: hold release: %v\n", err)
		return exitFailure
	}
	if o.JQColorsInvalid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	// Always appended — the bus is append-only — so an unknown or already
	// released id is a no-op by construction: `_hold_outstanding` excludes any id
	// with a matching release, however many it finds.
	body := compact(jsonv.Object(
		jsonv.Member{Key: "id", Val: jsonv.Str(hid)},
		jsonv.Member{Key: "released", Val: jsonv.Bool(true)},
	))
	line := bus.FitLine(func(b string) string {
		return compact(jsonv.Object(
			jsonv.Member{Key: "ts", Val: tsValue(strconv.FormatInt(o.clock().RealMS(), 10))},
			jsonv.Member{Key: "crew_id", Val: jsonv.Str(crew)},
			jsonv.Member{Key: "from", Val: jsonv.Str("dispatcher:" + crew)},
			jsonv.Member{Key: "to", Val: jsonv.Str("hold:" + crew)},
			jsonv.Member{Key: "kind", Val: jsonv.Str("msg")},
			jsonv.Member{Key: "body", Val: jsonv.Str(b)},
		))
	}, body)
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: hold release: %v\n", err)
		return exitFailure
	}
	return 0
}

// resolveCrew is `_hold_crew`: the --crew flag, else this repo's crew, under the
// same id guard `watch`/`stream` apply to a caller-supplied crew.
func resolveCrew(raw string, stderr io.Writer, o Options) (string, bool) {
	crew := raw
	if crew == "" && o.CrewID != nil {
		crew = o.CrewID()
	}
	if crew == "" {
		say(stderr, "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n")
		return "", false
	}
	if !bus.ValidCrewID(crew) {
		say(stderr, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n")
		return "", false
	}
	return crew, true
}

// clock is this call's view of the $CREW_CLOCK pair, shared with `crew await`
// through internal/clock.
func (o Options) clock() clock.Clock {
	return clock.Clock{Now: o.Now, CrewClock: o.CrewClock}
}

// tsValue is the row's `ts`: a number literal, so the digits print as written.
func tsValue(digits string) jsonv.Value {
	v, ok := jsonv.ParseNumber(digits)
	if !ok {
		return jsonv.Num(0)
	}
	return v
}

// orNull is the arm's `(if $x == "" then null else $x end)`.
func orNull(s string) jsonv.Value {
	if s == "" {
		return jsonv.Null()
	}
	return jsonv.Str(s)
}

// digitsLiteral is what `jq --argjson` makes of a digit string: the value, so
// leading zeros are gone and every other digit is kept exactly (a 20-digit
// timestamp stays a 20-digit timestamp).
func digitsLiteral(digits string) string {
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

// isDigits is the arm's digit case (an empty value or any non-digit rejects),
// inverted: a non-empty run of ASCII digits, which is all `--argjson` needs.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// elems hands a program whose `.` is an array the array's elements: jqrun makes
// `.` the slice it is passed, which is the arm's pipe from one jq to the next.
func elems(v jsonv.Value) []jsonv.Value {
	if v.Kind() != jsonv.KindArray {
		return nil
	}
	return v.Elems()
}

func compact(v jsonv.Value) string { return string(jsonv.Append(nil, v, jsonv.Options{})) }

func print_(w io.Writer, v jsonv.Value) { say(w, "%s\n", compact(v)) }
