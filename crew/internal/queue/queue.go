// Package queue records tasks a dispatcher has filed but not yet dispatched, so
// the roster can draw them. Rows go to their own `queue:<crew>` sink rather than
// a hold kind: every hold consumer reads a hold as maturing by time or fleet
// capacity (matured.jq treats a kind with no resets_at as due at once), so a
// queued task filed as a hold would be reported due on every pass.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

const (
	// Usage is what Run prints for an action it does not have.
	Usage = "crew: queue add|list|drop"
	// AddUsage and its siblings are the action's line for a grammar error.
	AddUsage  = "crew: queue add [--crew ID] [--ref R] [--branch B] [--after T]... <title...>"
	ListUsage = "crew: queue list [--crew ID] [--json]"
	DropUsage = "crew: queue drop <id> [--crew ID]"

	listHeader  = "id\tref\tbranch\tafter\ttitle"
	exitFailure = 1
)

var (
	digitsRe = regexp.MustCompile(`^[0-9]+$`)
	runRe    = regexp.MustCompile(`[0-9]+`)
)

// Options is everything Run reads beyond the bus: how to resolve the crew the
// action defaults to, the clock, and the sender check's probes.
type Options struct {
	CrewID func() string
	Clock  clock.Clock
	Sender sender.Probes // zero checks nothing
}

// Request is one `crew queue` call: the action and the fields that action
// reads. Crew is the action's --crew, "" for the caller's own.
type Request struct {
	Action       string
	JSON         bool
	Crew         string
	ID           string
	Ref, Branch  string
	After, Title []string
}

// Entry is one queued task, and the body of its add row.
type Entry struct {
	ID    string   `json:"id"`
	Task  Task     `json:"task"`
	After []string `json:"after"`
}

// Task is what was filed; Ref and Branch are null when empty.
type Task struct {
	Ref    *string `json:"ref"`
	Branch *string `json:"branch"`
	Title  string  `json:"title"`
}

// row is one `queue:<crew>` bus row. Field order is the row's byte order and
// `ts` a json.Number so the minted digits print as written.
type row struct {
	TS     json.Number `json:"ts"`
	CrewID string      `json:"crew_id"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Kind   string      `json:"kind"`
	Body   string      `json:"body"`
}

type dropBody struct {
	ID      string `json:"id"`
	Dropped bool   `json:"dropped"`
}

// rowBody is the union of an add body and a drop body. A body whose id is not a
// string fails to decode, which skips the row.
type rowBody struct {
	ID      string   `json:"id"`
	Task    *Task    `json:"task"`
	After   []string `json:"after"`
	Dropped bool     `json:"dropped"`
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run executes one queue action and returns its exit status.
func Run(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	switch req.Action {
	case "add":
		return add(ctx, req, paths, stdout, stderr, o)
	case "list":
		return list(req, paths, stdout, stderr, o)
	case "drop":
		return drop(ctx, req, paths, stderr, o)
	}
	say(stderr, "%s\n", Usage)
	return exitFailure
}

type dispatch struct {
	ts     int64
	branch string
	closes []string
}

// stamped is an add row's entry with the row's ts.
type stamped struct {
	ts int64
	Entry
}

// Outstanding is the crew's queued tasks, oldest add first: every add row of
// the crew's own dispatcher that no drop row or later dispatch has cleared.
func Outstanding(events []bus.Event, crew string) []Entry {
	adds, dropped, dispatches := fold(events, crew)
	out := []Entry{}
	for _, a := range adds {
		if dropped[a.ID] {
			continue
		}
		if slices.ContainsFunc(dispatches, func(d dispatch) bool {
			return d.ts >= a.ts && dispatchClears(a.Entry, d)
		}) {
			continue
		}
		out = append(out, a.Entry)
	}
	return out
}

func fold(events []bus.Event, crew string) (adds []stamped, dropped map[string]bool, dispatches []dispatch) {
	dropped = map[string]bool{}
	for _, e := range events {
		if e.CrewID != crew {
			continue
		}
		doc, _ := e.Doc.(map[string]any)
		ts, ok := number(doc["ts"])
		if !ok {
			continue
		}
		switch {
		case e.Kind == "dispatch":
			if e.Branch == "" {
				continue
			}
			dispatches = append(dispatches, dispatch{ts: ts, branch: e.Branch, closes: alsoCloses(doc["also_closes"])})
		case e.Kind == "msg" && e.To == "queue:"+crew && e.From == "dispatcher:"+crew:
			text, _ := doc["body"].(string)
			var b rowBody
			if json.Unmarshal([]byte(text), &b) != nil {
				continue
			}
			switch {
			case b.Dropped:
				dropped[b.ID] = true
			case b.Task != nil:
				if b.After == nil {
					b.After = []string{}
				}
				adds = append(adds, stamped{ts: ts, Entry: Entry{ID: b.ID, Task: *b.Task, After: b.After}})
			}
		}
	}
	return adds, dropped, dispatches
}

func number(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	return int64(f), err == nil
}

// alsoCloses is a dispatch row's also_closes as digit strings: items may be
// numbers or digit strings.
func alsoCloses(v any) []string {
	items, _ := v.([]any)
	var out []string
	for _, it := range items {
		switch x := it.(type) {
		case json.Number:
			out = append(out, x.String())
		case string:
			if digitsRe.MatchString(x) {
				out = append(out, x)
			}
		}
	}
	return out
}

// issueOf is the first digit run after the `type/` prefix of a branch, the rule
// rosterrender draws issue labels with.
func issueOf(branch string) string {
	if _, rest, ok := strings.Cut(branch, "/"); ok {
		branch = rest
	}
	return runRe.FindString(branch)
}

// refNumber is the issue number of a `#N` or `N` ref.
func refNumber(ref string) (string, bool) {
	n := strings.TrimPrefix(ref, "#")
	return n, digitsRe.MatchString(n)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func dispatchClears(e Entry, d dispatch) bool {
	branch, ref := deref(e.Task.Branch), deref(e.Task.Ref)
	if branch != "" && branch == d.branch {
		return true
	}
	if n, ok := refNumber(ref); ok {
		return issueOf(d.branch) == n || slices.Contains(d.closes, n)
	}
	return ref != "" && ref == d.branch
}

// sameTask reports whether two entries name one task: equal non-empty
// branches, equal refs (`#N` ≡ `N`, a branch ref verbatim), or a numeric ref
// equal to the other side's branch issue.
func sameTask(a, b Entry) bool {
	ab, bb := deref(a.Task.Branch), deref(b.Task.Branch)
	ar, br := deref(a.Task.Ref), deref(b.Task.Ref)
	if ab != "" && ab == bb {
		return true
	}
	an, aNum := refNumber(ar)
	bn, bNum := refNumber(br)
	switch {
	case aNum && bNum && an == bn:
		return true
	case !aNum && !bNum && ar != "" && ar == br:
		return true
	case aNum && bb != "" && issueOf(bb) == an:
		return true
	case bNum && ab != "" && issueOf(ab) == bn:
		return true
	}
	return false
}

// sanitize replaces control characters with a space so a TSV column and the
// terminal stay intact.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func clean(s string) string { return strings.TrimSpace(sanitize(s)) }

func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func add(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok {
		return exitFailure
	}
	title := clean(strings.Join(req.Title, " "))
	ref, branch := clean(req.Ref), clean(req.Branch)
	if title == "" {
		say(stderr, "crew: queue add: a title is required\n")
		return exitFailure
	}
	if ref == "" && branch == "" {
		say(stderr, "crew: queue add: --ref or --branch is required\n")
		return exitFailure
	}
	if !mayPost(ctx, "add", crew, stderr, o) {
		return exitFailure
	}
	after := []string{}
	for _, t := range req.After {
		if t = clean(t); t != "" {
			after = append(after, t)
		}
	}

	events, err := bus.ReadEvents(paths.Log)
	if err != nil && !errors.Is(err, bus.ErrNoLog) {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: queue: %s: %s\n", paths.Log, msg)
		return code
	}
	fresh := Entry{Task: Task{Ref: orNull(ref), Branch: orNull(branch)}}
	for _, e := range Outstanding(events, crew) {
		if sameTask(e, fresh) {
			say(stdout, "%s\n", e.ID)
			return 0
		}
	}
	// A successor re-filing a task dispatched since its note was written would
	// otherwise leave a ghost no later dispatch clears.
	_, _, dispatches := fold(events, crew)
	for _, d := range dispatches {
		if dispatchClears(fresh, d) {
			say(stderr, "crew: queue add: already dispatched as %s; not queued\n", d.branch)
			return 0
		}
	}

	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: queue add: %v\n", err)
		return exitFailure
	}
	// Computed once, before the builder: FitLine calls it repeatedly while
	// shrinking the title, and an id minted inside would stop matching its own
	// row's ts.
	ts := strconv.FormatInt(o.Clock.RealMS(), 10)
	id := ts + "-" + strconv.Itoa(os.Getpid())
	line := bus.FitLine(func(title string) string {
		return encode(row{
			TS: json.Number(ts), CrewID: crew, From: "dispatcher:" + crew, To: "queue:" + crew, Kind: "msg",
			Body: encode(Entry{ID: id, Task: Task{Ref: orNull(ref), Branch: orNull(branch), Title: title}, After: after}),
		})
	}, title)
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: queue add: %v\n", err)
		return exitFailure
	}
	say(stdout, "%s\n", id)
	return 0
}

func list(req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok {
		return exitFailure
	}
	events, err := bus.ReadEvents(paths.Log)
	if err != nil && !errors.Is(err, bus.ErrNoLog) {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: queue: %s: %s\n", paths.Log, msg)
		return code
	}
	entries := Outstanding(events, crew)
	if req.JSON {
		say(stdout, "%s\n", encode(entries))
		return 0
	}
	say(stdout, "%s\n", listHeader)
	for _, e := range entries {
		say(stdout, "%s\t%s\t%s\t%s\t%s\n", sanitize(e.ID), dash(sanitize(deref(e.Task.Ref))),
			dash(sanitize(deref(e.Task.Branch))), dash(sanitize(strings.Join(e.After, ","))), sanitize(e.Task.Title))
	}
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func drop(ctx context.Context, req Request, paths bus.Paths, stderr io.Writer, o Options) int {
	if req.ID == "" {
		say(stderr, "%s\n", DropUsage)
		return exitFailure
	}
	crew, ok := resolveCrew(req.Crew, stderr, o)
	if !ok || !mayPost(ctx, "drop", crew, stderr, o) {
		return exitFailure
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: queue drop: %v\n", err)
		return exitFailure
	}
	// Always appended: the bus is append-only, so an unknown id is a no-op by
	// construction, since the fold only drops ids it has seen added.
	line := bus.FitLine(func(b string) string {
		return encode(row{
			TS:     json.Number(strconv.FormatInt(o.Clock.RealMS(), 10)),
			CrewID: crew, From: "dispatcher:" + crew, To: "queue:" + crew, Kind: "msg", Body: b,
		})
	}, encode(dropBody{ID: req.ID, Dropped: true}))
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: queue drop: %v\n", err)
		return exitFailure
	}
	return 0
}

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

func mayPost(ctx context.Context, sub, crew string, stderr io.Writer, o Options) bool {
	reason := sender.Check(ctx, "dispatcher:"+crew, crew, o.Sender)
	if reason != "" {
		say(stderr, "crew: queue %s: refusing sender 'dispatcher:%s' — %s\n", sub, crew, reason)
	}
	return reason == ""
}

// encode is compact JSON with `<`, `>` and `&` left raw: bus.FitLine measures
// the encoded line, so an escaped `<` would move the shrink point.
func encode(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}
