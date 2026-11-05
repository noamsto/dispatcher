// Package hold is `crew hold`: a queued dispatch parked on a quota window, so a
// successor session can resume it without a human. Records go to the synthetic
// sink `hold:<crew>`, matching `retro`/`metrics` — `watch`'s `to==$me or to=="*"`
// predicate matches neither, so a hold cannot wake or pollute the inbox of the
// dispatcher that wrote it, while `crew log` still shows it.
//
// The read side (`list`, `due`, `park`) runs the arm's own jq, embedded and
// handed to jqrun. It is the one implementation of `_hold_outstanding`'s fold
// since #935 ported `roster-render`, the helper's last bash caller, and
// `Outstanding` is how that command reaches it too.
//
// The write side (`add`, `release`) cannot be jq at all. The row's `body` is
// `{...} | tostring`, and gojq returns object keys sorted where jq keeps
// construction order — and `body` is a *string*, so `jq -S` cannot sort it back
// and `crew log` prints it verbatim. It is built from structs whose field order
// is that construction order, encoded without HTML escaping, and appended
// through bus.Append/bus.FitLine.
package hold

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

//go:embed outstanding.jq
var outstandingProg string

//go:embed matured.jq
var maturedProg string

//go:embed park.jq
var parkProg string

//go:embed render.jq
var renderProg string

//go:embed parked.jq
var parkedProg string

const (
	// Usage is what the arm prints for an action it does not have.
	Usage = "crew: hold add|list|due|park|release"
	// ListUsage and its siblings are the action's line for a grammar error.
	ListUsage    = "crew: hold list [--crew ID] [--json]"
	DueUsage     = "crew: hold due [--crew ID] [--json [--with-parked]]"
	ParkUsage    = "crew: hold park <default> [--crew ID]"
	ReleaseUsage = "crew: hold release <id> [--crew ID]"
	// renderHeader is `_hold_render`'s printf, matching the fold's columns.
	renderHeader = "id\tengine\twindow\tresets_at\tref\tbranch\ttitle"
	// parkedHeader heads the block of fleet holds `due` lists as parked.
	parkedHeader = "parked\treason\tbranch\ttitle"
	// exitType is jq's status for a fold that fails on a row it cannot use.
	exitType    = 5
	exitFailure = 1
)

// Options is everything Run reads beyond the bus: how to resolve the crew the
// arm defaults to (`_crew_id`), the clock, and $CREW_CLOCK — the test-only
// virtual clock `_clock_now` reads seconds from.
type Options struct {
	CrewID func() string
	Clock  clock.Clock
	Sender sender.Probes // the sender check's reads; zero checks nothing
	// FleetDue is the machine-wide fleet queue's verdict on one crew's fleet holds
	// (fleet.Due, wired in main.go). record asks it to persist the demotion clocks.
	// nil: no fleet hold is ever due.
	FleetDue func(ctx context.Context, common, crew string, record bool) (FleetVerdict, error)
	// FleetBudget is the fleet's weight budget; `add --fleet` refuses a hold
	// heavier than it. nil: no check.
	FleetBudget func(ctx context.Context) (int, error)
	// EnrollRepo registers the repo's git common dir in the fleet registry, which
	// dispatch only writes after its fleet gate: `add --fleet` calls it so the
	// queue sees a hold placed in a repo that was never dispatched from. nil: skip.
	EnrollRepo func(common string) error
}

// FleetVerdict is one crew's share of an evaluation: Due holds at most one id
// machine-wide; Parked the crew's holds parked as "demoted", "oversize" or
// "ineligible".
type FleetVerdict struct {
	Due    []string
	Parked []ParkedHold
}

type ParkedHold struct{ ID, Reason string }

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// AddRequest is `crew hold add`'s flags and title words. Engine is the engine
// whose quota is being waited on and Agent the engine the task will be
// dispatched to (task.engine): both are required and kept distinct. A Fleet hold
// waits on the machine-wide queue instead, so it takes no Engine, Window or
// ResetsAt; Prio is its queue priority.
type AddRequest struct {
	Engine, Window, ResetsAt, Agent, Ref, Branch, Tier, Model, Effort string
	Plan, MCP, Shape, Spec, Prio                                      string
	Draft, Fleet                                                      bool
	Title                                                             []string
}

// Request is one `crew hold` call: the action and the fields that action reads.
// Crew is the action's --crew, "" for the caller's own. WithParked is `due
// --json --with-parked`: the `{"due","parked"}` object where --json alone prints
// the due array.
type Request struct {
	Action     string
	JSON       bool
	WithParked bool
	Crew       string
	Default    string
	ID         string
	Add        AddRequest
}

// Run is the arm's `case "$holdsub"`. `--help` never reaches here: the crew.sh
// preamble answers `crew hold --help` and `crew hold <action> --help` before the
// delegation arm runs.
func Run(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	switch req.Action {
	case "add":
		return add(ctx, req, paths, stdout, stderr, o)
	case "list":
		return list(req, paths, stdout, stderr, o)
	case "due":
		return due(ctx, req, paths, stdout, stderr, o)
	case "park":
		return park(req, paths, stdout, stderr, o)
	case "release":
		return release(ctx, req, paths, stdout, stderr, o)
	default:
		say(stderr, "%s\n", Usage)
		return exitFailure
	}
}

// folds runs one of the arm's jq programs over a bus, mapping a failure to the
// arm's one stderr line and status.
type folds struct {
	paths  bus.Paths
	stderr io.Writer
}

// call is one fold — the arm's one jq process.
func (f *folds) call(prog string, in []any, vars map[string]any) (any, int) {
	// `jq -s` always hands the program an array; a nil slice would reach gojq as
	// `null`, which every fold's `.[]` reads as a type error instead.
	if in == nil {
		in = []any{}
	}
	out, err := jqrun.RunAny(prog, in, 0, vars)
	if err != nil {
		say(f.stderr, "crew: hold: %s: %v\n", f.paths.Log, err)
		return nil, exitType
	}
	return out, 0
}

// outstanding is the arm's `[ -f "$log" ]` and its strict `jq -s` read (no
// torn-tail tolerance) around the embedded fold: `[]` and no jq at all where the
// log is not a regular file — `due` and `list` depend on that unambiguous
// empty-array read — and otherwise the fold over the decoded events.
func (f *folds) outstanding(crew string) (any, int) {
	events, err := bus.ReadEvents(f.paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return []any{}, 0
	}
	if err != nil {
		msg, code := bus.JQFailure(err)
		say(f.stderr, "crew: hold: %s: %s\n", f.paths.Log, msg)
		return nil, code
	}
	docs := make([]any, len(events))
	for i, ev := range events {
		docs[i] = ev.Doc
	}
	return f.call(outstandingProg, docs, holdVars(crew))
}

// Outstanding is the `_hold_outstanding` fold over events the caller already
// read, in gojq's form (`bus.Event.Doc`): the adds to `hold:<crew>` with no
// matching release, one per `crew hold list --json` row. `roster-render` reads
// it for the hexagons beside the workers.
func Outstanding(events []any, crew string) (any, error) {
	return jqrun.RunAny(outstandingProg, events, 0, holdVars(crew))
}

// holdVars are the fold's two `--arg`s.
func holdVars(crew string) map[string]any {
	return map[string]any{
		"crew": crew,
		"to":   "hold:" + crew,
	}
}

func list(req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	jsonOut := req.JSON
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok {
		return exitFailure
	}
	f := &folds{paths: paths, stderr: stderr}
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

func due(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	jsonOut := req.JSON
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok {
		return exitFailure
	}
	f := &folds{paths: paths, stderr: stderr}
	holds, code := f.outstanding(crew)
	if code != 0 {
		return code
	}
	// `--argjson now "$(_clock_now_f)"`: seconds, fractional unless the virtual
	// clock is set. The compare is jq's, so a hold maturing this second is due.
	matured, code := f.call(maturedProg, elems(holds), map[string]any{"now": o.Clock.NowF()})
	if code != 0 {
		return code
	}
	// Every run evaluates the whole machine's queue, this crew's fleet holds or
	// not, so it runs even with none; only a dispatcher's own run may record it.
	var verdict FleetVerdict
	if o.FleetDue != nil {
		record := sender.Check(ctx, "dispatcher:"+crew, crew, o.Sender) == ""
		var err error
		if verdict, err = o.FleetDue(ctx, paths.Common, crew, record); err != nil {
			say(stderr, "crew: hold due: fleet queue: %v\n", err)
			return exitType
		}
	}
	fleetDue, parked := fleetRows(elems(holds), verdict)
	// Non-nil, so an empty set prints as `[]`.
	ready := append([]any{}, elems(matured)...)
	ready = append(ready, fleetDue...)
	if jsonOut {
		// Bare array unless asked: a stream from an older build reads this output
		// through the binary the entry now points at.
		if req.WithParked {
			print_(stdout, map[string]any{"due": ready, "parked": parked})
		} else {
			print_(stdout, ready)
		}
	} else {
		if code = render(f, stdout, ready); code != 0 {
			return code
		}
		if len(parked) > 0 {
			if code = table(f, stdout, parkedHeader, parkedProg, parked); code != 0 {
				return code
			}
		}
	}
	// Exits 0 when any hold is due, 1 when none — including a missing log, since
	// the outstanding fold already answered `[]` for that case. Parked holds do
	// not count.
	if len(ready) > 0 {
		return 0
	}
	return exitFailure
}

// fleetRows splits this crew's outstanding fleet holds by the queue's verdict:
// the ones due now, and the parked ones with their reason added (on a copy, so
// the fold's own rows stay as read).
func fleetRows(holds []any, v FleetVerdict) (due, parked []any) {
	due, parked = []any{}, []any{}
	for _, h := range holds {
		row, _ := h.(map[string]any)
		if wait, _ := row["wait"].(map[string]any); wait["kind"] != "fleet" {
			continue
		}
		id, _ := row["id"].(string)
		if slices.Contains(v.Due, id) {
			due = append(due, h)
		}
		if i := slices.IndexFunc(v.Parked, func(p ParkedHold) bool { return p.ID == id }); i >= 0 {
			withReason := maps.Clone(row)
			withReason["reason"] = v.Parked[i].Reason
			parked = append(parked, withReason)
		}
	}
	return due, parked
}

func park(req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	// The arm validates the default before it reads a crew or a log: empty or
	// non-digit first, then the positive test — where a value outside int64 fails
	// bash's `test` and lands in the same branch.
	defStr := req.Default
	badDefault := func() int {
		say(stderr, "crew: hold park <default> must be a positive integer number of seconds\n")
		return exitFailure
	}
	if !ascii.IsDigits(defStr) {
		return badDefault()
	}
	def, err := strconv.ParseInt(defStr, 10, 64)
	if err != nil || def <= 0 {
		return badDefault()
	}
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok {
		return exitFailure
	}
	f := &folds{paths: paths, stderr: stderr}
	holds, code := f.outstanding(crew)
	if code != 0 {
		return code
	}
	out, code := f.call(parkProg, elems(holds), map[string]any{
		"default": float64(def),
		"now":     o.Clock.NowF(),
	})
	if code != 0 {
		return code
	}
	print_(stdout, out)
	return 0
}

func render(f *folds, stdout io.Writer, holds any) int {
	return table(f, stdout, renderHeader, renderProg, holds)
}

// table is `_hold_render`: the header goes out before the rows' jq starts, so a
// fold that then fails still leaves it on stdout.
func table(f *folds, stdout io.Writer, header, prog string, holds any) int {
	say(stdout, "%s\n", header)
	rows, code := f.call(prog, elems(holds), nil)
	if code != 0 {
		return code
	}
	// The `join("\n")` patch (report.jq's) means one value for jqrun; an empty
	// string is zero rows, and `jq -r` printed no bytes for those.
	if s, _ := rows.(string); s != "" {
		say(stdout, "%s\n", s)
	}
	return 0
}

func add(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	fl := req.Add
	title := strings.Join(fl.Title, " ")

	if !fl.Fleet && fl.Prio != "" {
		say(stderr, "crew: hold add: --prio needs --fleet\n")
		return exitFailure
	}
	if fl.Fleet && (fl.Engine != "" || fl.Window != "" || fl.ResetsAt != "") {
		say(stderr, "crew: hold add: --fleet takes no --engine, --window or --resets-at\n")
		return exitFailure
	}
	// Reject a missing required field by name. The order is the arm's, and it
	// decides which line a doubly-incomplete call prints.
	for _, req := range []struct {
		flag, val string
		quota     bool
	}{
		{"--engine", fl.Engine, true}, {"--window", fl.Window, true}, {"--resets-at", fl.ResetsAt, true},
		{"--agent", fl.Agent, false}, {"--ref", fl.Ref, false}, {"--branch", fl.Branch, false},
		{"--tier", fl.Tier, false}, {"--model", fl.Model, false}, {"--effort", fl.Effort, false},
	} {
		if req.val == "" && (!fl.Fleet || !req.quota) {
			say(stderr, "crew: hold add: %s is required\n", req.flag)
			return exitFailure
		}
	}
	if title == "" {
		say(stderr, "crew: hold add: a title is required\n")
		return exitFailure
	}
	if fl.Fleet {
		if !checkFleetFlags(&fl, stderr) {
			return exitFailure
		}
	} else if !checkResetsAt(fl.ResetsAt, stderr, o) {
		return exitFailure
	}
	if fl.Spec != "" {
		fh, err := os.Open(fl.Spec)
		if err != nil {
			say(stderr, "crew: hold add: --spec file '%s' is not readable\n", fl.Spec)
			return exitFailure
		}
		_ = fh.Close()
	}
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok || !mayPost(ctx, "add", crew, stderr, o) {
		return exitFailure
	}
	if fl.Fleet && o.FleetBudget != nil {
		budget, err := o.FleetBudget(ctx)
		if err != nil {
			say(stderr, "crew: hold add: %v\n", err)
			return exitFailure
		}
		if w := tierWeights[fl.Tier]; w > budget {
			say(stderr, "crew: hold add: --fleet: a %s hold weighs %d, more than fleetBudget %d, so it could never be released\n", fl.Tier, w, budget)
			return exitFailure
		}
	}
	if fl.Fleet && o.EnrollRepo != nil {
		if err := o.EnrollRepo(paths.Common); err != nil {
			say(stderr, "crew: hold add: --fleet: cannot register %s in the fleet registry: %v\n", paths.Common, err)
			return exitFailure
		}
	}
	// A `hold add` with no --spec would otherwise fail its append on a repo with
	// no bus dir yet — mirrors `status`/`msg`'s own mkdir -p.
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: hold add: %v\n", err)
		return exitFailure
	}
	// Milliseconds, not `crew new`'s seconds: the duplicate guard compares `id`
	// against the ms `ts` fields dispatch writes, so copying `crew new` breaks
	// that comparison by 1000x. Real time even under CREW_CLOCK, because the arm
	// minted these with `jq -nc 'now*1000|floor'`.
	ts := strconv.FormatInt(o.Clock.RealMS(), 10)
	id := ts + "-" + strconv.Itoa(os.Getpid())

	specpath := ""
	if fl.Spec != "" {
		if err := os.MkdirAll(filepath.Join(paths.Dir, "holds"), 0o755); err != nil {
			say(stderr, "crew: hold add: %v\n", err)
			return exitFailure
		}
		specpath = filepath.Join(paths.Dir, "holds", id+".md")
		data, err := os.ReadFile(fl.Spec)
		if err != nil {
			say(stderr, "crew: hold add: cannot copy --spec %s: %v\n", fl.Spec, err)
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
		return encode(addRow(crew, id, ts, &fl, specpath, title))
	}, title)
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: hold add: %v\n", err)
		return exitFailure
	}
	say(stdout, "%s\n", id)
	return 0
}

// tierWeights is a fleet hold's weight against the fleet budget: the tier's own,
// as `crew fleet load` counts it.
var tierWeights = map[string]int{"trivial": 1, "standard": 2, "deep": 3}

// checkFleetFlags validates what only a fleet hold takes, and defaults --prio.
func checkFleetFlags(fl *AddRequest, stderr io.Writer) bool {
	if _, ok := tierWeights[fl.Tier]; !ok {
		say(stderr, "crew: hold add: --fleet needs --tier trivial, standard or deep\n")
		return false
	}
	switch fl.Prio {
	case "":
		fl.Prio = "normal"
	case "high", "normal", "low":
	default:
		say(stderr, "crew: hold add: --prio must be high, normal or low\n")
		return false
	}
	return true
}

// checkResetsAt is the quota hold's --resets-at validation.
func checkResetsAt(raw string, stderr io.Writer, o Options) bool {
	// Shape only, never the floor — `refresh-budget.sh` alone owns the 85%/95%
	// judgment. Seconds here, matching `resets_at`'s own unit; `id`/`ts` below are
	// milliseconds, a different clock.
	if !ascii.IsDigits(raw) {
		say(stderr, "crew: hold add: --resets-at must be an integer epoch-seconds timestamp\n")
		return false
	}
	// A default outside int64 makes bash's `test` fail outright, so it lands here
	// too (minus the shell's own diagnostic, which is not ours to invent).
	resets, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		say(stderr, "crew: hold add: --resets-at must be in the future\n")
		return false
	}
	if resets <= o.Clock.Seconds() {
		say(stderr, "crew: hold add: --resets-at must be in the future\n")
		return false
	}
	return true
}

// holdRow is one `hold` bus row. Field order is the row's byte order — the arm
// built it in jq's construction order and `crew log` prints the line verbatim.
// `ts` is a json.Number so the minted digits print as written, never through a
// float64.
type holdRow struct {
	TS     json.Number `json:"ts"`
	CrewID string      `json:"crew_id"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Kind   string      `json:"kind"`
	Body   string      `json:"body"`
}

// holdBody is `add`'s body, releaseBody `release`'s. Both are JSON text inside
// the row's `body` *string*, so their key order is part of the value and `jq -S`
// cannot normalise it; `TestAddRowMatchesTheBuilder` pins the exact text.
type holdBody struct {
	ID   string   `json:"id"`
	Wait any      `json:"wait"` // holdWait or fleetWait
	Task holdTask `json:"task"`
}

type holdWait struct {
	Engine   string `json:"engine"`
	Window   string `json:"window"`
	ResetsAt int64  `json:"resets_at"`
}

type fleetWait struct {
	Kind   string `json:"kind"`
	Weight int    `json:"weight"`
}

type holdTask struct {
	Ref    string `json:"ref"`
	Branch string `json:"branch"`
	Tier   string `json:"tier"`
	// Engine is `task.engine`, the dispatch target; holdWait's engine is the
	// quota being waited on.
	Engine string  `json:"engine"`
	Model  string  `json:"model"`
	Effort string  `json:"effort"`
	Plan   *string `json:"plan"`
	MCP    *string `json:"mcp"`
	Draft  bool    `json:"draft"`
	Shape  *string `json:"shape"`
	Title  string  `json:"title"`
	Spec   *string `json:"spec"`
	// Prio is a fleet hold's queue priority; omitted, so a quota hold's body text
	// is unchanged.
	Prio *string `json:"prio,omitempty"`
}

type releaseBody struct {
	ID       string `json:"id"`
	Released bool   `json:"released"`
}

// addRow is `_build_hold`: every fixed field closes over the enclosing scope and
// the title is the only shrinkable part. Handing Shrink the whole body would
// truncate `id`, `task.branch` and `task.spec` along with the title.
func addRow(crew, id, ts string, fl *AddRequest, specpath, title string) holdRow {
	var wait any
	var prio *string
	if fl.Fleet {
		wait = fleetWait{Kind: "fleet", Weight: tierWeights[fl.Tier]}
		prio = &fl.Prio
	} else {
		// The arm's `--argjson resets_at <digits>`: digitsLiteral is that literal,
		// and int64 holds every value the ParseInt in `add` bounded.
		resets, _ := strconv.ParseInt(digitsLiteral(fl.ResetsAt), 10, 64)
		wait = holdWait{
			Engine:   fl.Engine,
			Window:   fl.Window,
			ResetsAt: resets,
		}
	}
	return holdRow{
		TS:     json.Number(ts),
		CrewID: crew,
		From:   "dispatcher:" + crew,
		To:     "hold:" + crew,
		Kind:   "msg",
		Body: encode(holdBody{
			ID:   id,
			Wait: wait,
			Task: holdTask{
				Ref:    fl.Ref,
				Branch: fl.Branch,
				Tier:   fl.Tier,
				Engine: fl.Agent,
				Model:  fl.Model,
				Effort: fl.Effort,
				Plan:   orNull(fl.Plan),
				MCP:    orNull(fl.MCP),
				Draft:  fl.Draft,
				Shape:  orNull(fl.Shape),
				Title:  title,
				Spec:   orNull(specpath),
				Prio:   prio,
			},
		}),
	}
}

func release(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	hid := req.ID
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok || !mayPost(ctx, "release", crew, stderr, o) {
		return exitFailure
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: hold release: %v\n", err)
		return exitFailure
	}
	// Always appended — the bus is append-only — so an unknown or already
	// released id is a no-op by construction: the outstanding fold excludes any id
	// with a matching release, however many it finds.
	body := encode(releaseBody{ID: hid, Released: true})
	line := bus.FitLine(func(b string) string {
		return encode(holdRow{
			TS:     json.Number(strconv.FormatInt(o.Clock.RealMS(), 10)),
			CrewID: crew,
			From:   "dispatcher:" + crew,
			To:     "hold:" + crew,
			Kind:   "msg",
			Body:   b,
		})
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

// mayPost runs the sender check for the dispatcher-authored rows `add` and
// `release` write, printing the refusal under sub.
func mayPost(ctx context.Context, sub, crew string, stderr io.Writer, o Options) bool {
	reason := sender.Check(ctx, "dispatcher:"+crew, crew, o.Sender)
	if reason != "" {
		say(stderr, "crew: hold %s: refusing sender 'dispatcher:%s' — %s\n", sub, crew, reason)
	}
	return reason == ""
}

// orNull is the arm's `(if $x == "" then null else $x end)`.
func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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

// elems hands a program whose `.` is an array the array's elements: jqrun makes
// `.` the slice it is passed, which is the arm's pipe from one jq to the next.
func elems(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

// JSON is v as compact JSON. Every writer here encodes through it, with `<`, `>`
// and `&` left raw as jq printed them: bus.FitLine measures the encoded line,
// so an escaped `<` would move the shrink point by five bytes.
func JSON(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}

// encode is JSON for a value this package builds: its leaves are strings, bools,
// int64s, json.Number and a fold's own output, all of which encode.
func encode(v any) string {
	b, err := JSON(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func print_(w io.Writer, v any) { say(w, "%s\n", encode(v)) }
