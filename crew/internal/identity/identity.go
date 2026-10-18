// Package identity gives each branch a stable codename, colour and tmux
// colour: the one `dispatch` recorded, else the pool slot its name hashes to.
package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// The pools are the three the bash library carried (`_names`/`_colors`/`_tmuxc`)
// and the only copy of them left; TestPoolsAreGolden freezes the order, because a
// recorded identity is its slot.
const poolSize = 32

var names = [poolSize]string{
	"sage", "atlas", "nova", "ember", "reef", "iris", "amber", "coral", "moss", "slate", "rust", "plum", "lime", "rose", "sky", "onyx",
	"pine", "lagoon", "indigo", "fern", "bronze", "violet", "khaki", "ash", "brick", "mauve", "tan", "crimson", "fuchsia", "blush", "orchid", "cobalt",
}

var colors = [poolSize]string{
	"green", "blue", "magenta", "orange", "teal", "purple", "yellow", "salmon", "olive", "steel", "rust", "plum", "lime", "pink", "sky", "grey",
	"seagreen", "darkcyan", "indigo", "forestgreen", "darkgoldenrod", "mediumpurple", "darkkhaki", "dimgrey", "indianred", "palevioletred", "peru", "crimson", "mediumvioletred", "hotpink", "orchid", "royalblue",
}

var tmuxc = [poolSize]string{
	"colour28", "colour32", "colour127", "colour130", "colour30", "colour98", "colour136", "colour167", "colour100", "colour67", "colour166", "colour96", "colour64", "colour162", "colour25", "colour244",
	"colour29", "colour31", "colour61", "colour65", "colour94", "colour97", "colour101", "colour102", "colour131", "colour132", "colour137", "colour160", "colour163", "colour168", "colour169", "colour68",
}

var (
	nameRe = regexp.MustCompile(`\A[a-z][a-z0-9-]*\z`)
	tmuxRe = regexp.MustCompile(`\Acolour[0-9]+\z`)
)

var crcTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&(1<<31) != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

// CKsum is POSIX cksum(1): CRC-32 (poly 0x04C11DB7, MSB first, init 0) over
// the data then its length as minimal little-endian bytes, complemented — the
// checksum crew.sh and the phase-status handlers put in their state-file names.
func CKsum(data []byte) uint32 {
	var crc uint32
	step := func(b byte) { crc = crc<<8 ^ crcTable[byte(crc>>24)^b] }
	for _, b := range data {
		step(b)
	}
	for n := uint64(len(data)); n != 0; n >>= 8 {
		step(byte(n))
	}
	return ^crc
}

// Slot is the pool index a branch hashes to.
func Slot(branch string) int { return int(CKsum([]byte(branch)) % poolSize) }

// Identity is a branch's codename, colour and tmux colour.
type Identity struct {
	Name  string `json:"name"`
	Color any    `json:"color"` // passes through as any JSON value, gojq form
	Tmux  string `json:"tmux"`
}

// Value is the identity as the jq programs' input object.
func (id Identity) Value() map[string]any {
	return map[string]any{"name": id.Name, "color": id.Color, "tmux": id.Tmux}
}

// At is the pool identity {name, color, tmux} at slot.
func At(slot int) Identity {
	return Identity{Name: names[slot], Color: colors[slot], Tmux: tmuxc[slot]}
}

// Palette is the `_colors` pool: the roster renderer's D2 program looks a row's
// colour up in it to decide whether the branch box gets a stroke, which is why
// the whole list travels rather than one slot.
func Palette() []string { return slices.Clone(colors[:]) }

// Recorded is the identity of the last well-formed `dispatch` event for
// branch, across every crew. Events that are not objects never match.
func Recorded(events []any, branch string) (Identity, bool) {
	branch = jsonval.ValidUTF8(branch)
	var last Identity
	found := false
	for _, ev := range events {
		if id, ok := recordedIn(ev, branch); ok {
			last, found = id, true
		}
	}
	return last, found
}

func recordedIn(ev any, branch string) (Identity, bool) {
	doc, ok := ev.(map[string]any)
	if !ok {
		return Identity{}, false
	}
	if !stringMember(doc, "kind", func(s string) bool { return s == "dispatch" }) ||
		!stringMember(doc, "branch", func(s string) bool { return s == branch }) ||
		!stringMember(doc, "name", nameRe.MatchString) ||
		!stringMember(doc, "tmux", tmuxRe.MatchString) {
		return Identity{}, false
	}
	return Identity{Name: doc["name"].(string), Color: doc["color"], Tmux: doc["tmux"].(string)}, true
}

// RecordedAll folds every branch's recorded identity in a single pass: for
// each branch, the identity of its last well-formed `dispatch` event, the
// same predicate Recorded reports per branch. The roster builds its identity
// map from this so a fold is O(events + rows), not O(rows × events) (#821).
// Only branches with a recorded identity appear as keys.
func RecordedAll(events []any) map[string]Identity {
	last := map[string]Identity{}
	for _, ev := range events {
		doc, ok := ev.(map[string]any)
		if !ok {
			continue
		}
		branch, isStr := doc["branch"].(string)
		if !isStr {
			continue
		}
		if id, ok := recordedIn(doc, branch); ok {
			last[branch] = id
		}
	}
	return last
}

// stringMember reports whether doc[key] is a string satisfying ok.
func stringMember(doc map[string]any, key string, ok func(string) bool) bool {
	s, isStr := doc[key].(string)
	return isStr && ok(s)
}

// For is the recorded identity of branch from a RecordedAll map, else its
// pool identity. A branch that is empty or holds a space, tab or newline is
// never looked up by the roster (the arm's `$(...)` word-split drops it).
func For(recorded map[string]Identity, branch string) Identity {
	if id, ok := recorded[branch]; ok {
		return id
	}
	return At(Slot(branch))
}

// remoteHold is how long a `remote:true` dispatch row holds its codename. The
// remote worker's terminal status never reaches this bus, so without a bound the
// name would never free.
const remoteHold = 24 * time.Hour

// Options is everything Run reads beyond the bus: the crew the arm defaults to
// (`_crew_id`), tmux, the environment CREW_IDENTITY_AVOID comes from
// and the clock the remote hold is measured on. A nil Getenv reads no variable
// and a nil Now is the wall clock.
type Options struct {
	CrewID func() string
	Tmux   tmux.Client // nil, like a failed read, names no window
	Getenv func(string) string
	Now    func() time.Time
}

// Request is the arm's validated arguments: Hash, when set, is the `--hash`
// form and wins; Occupied, when set, lists the names taken in Crew instead of
// assigning one; otherwise Branch is never empty and Crew "" takes the default.
type Request struct {
	Branch, Crew, Hash string
	Occupied           bool
}

// Run is the `crew identity <branch> [crew]` arm: the `--hash` form, and the
// identity a branch gets.
func Run(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	if req.Hash != "" {
		return emit(stdout, At(Slot(req.Hash)))
	}
	crew := req.Crew
	if crew == "" && o.CrewID != nil {
		crew = o.CrewID()
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	if req.Occupied {
		for _, name := range Occupied(ctx, busEvents(paths.Log), crew, o.Tmux, now().UnixMilli()) {
			say(stdout, "%s\n", name)
		}
		return 0
	}
	return emit(stdout, Assign(ctx, busEvents(paths.Log), req.Branch, crew, o.Tmux, avoidList(o.Getenv), now().UnixMilli()))
}

// avoidList is CREW_IDENTITY_AVOID: the codenames a dispatching host already
// holds elsewhere, comma-separated.
func avoidList(getenv func(string) string) []string {
	if getenv == nil {
		return nil
	}
	return slices.DeleteFunc(strings.Split(getenv("CREW_IDENTITY_AVOID"), ","), func(s string) bool { return s == "" })
}

// Occupied is every raw codename Assign would treat as taken for crew, sorted:
// the bus's live dispatches (a just-dispatched worker with no status yet
// included) and the names stamped on tmux windows, local and bridge.
func Occupied(ctx context.Context, rows []bus.Event, crew string, c tmux.Client, nowMS int64) []string {
	occ := occupied(rows, "", crew, nowMS)
	for _, win := range windowNames(ctx, c) {
		occ[win] = true
	}
	return slices.Sorted(maps.Keys(occ))
}

// Assign is `_identity_assign <branch> <crew>`: the identity a dispatch stamps.
// A branch keeps the identity it recorded unless another live worker holds that
// name; otherwise it takes its hash slot stepped forward to the first codename no
// live worker holds, falling back to the hash slot when all 32 are taken. A branch
// always gets an identity. The avoid names are held elsewhere (another host's
// workers) and count as live workers on the bus.
func Assign(ctx context.Context, rows []bus.Event, branch, crew string, c tmux.Client, avoid []string, nowMS int64) Identity {
	// The recorded name is gated by the bus alone: tmux names join only after it.
	occ := occupied(rows, branch, crew, nowMS)
	for _, name := range avoid {
		occ[name] = true
	}
	if rec, ok := Recorded(docs(rows), branch); ok {
		if !occ[rec.Name] {
			return rec
		}
	}
	for _, win := range windowNames(ctx, c) {
		occ[win] = true
	}
	base := Slot(branch)
	slot := base
	for i := range names {
		slot = (base + i) % poolSize
		if !occ[names[slot]] {
			break
		}
	}
	if occ[names[slot]] {
		slot = base // every codename taken: the hash slot, taken or not
	}
	return At(slot)
}

// occupied is `_identity_bus_occ <branch> <crew>`: the codename of every OTHER
// branch dispatched into the crew no later than this branch's latest dispatch,
// whose newest `worker:` status is not terminal. A `remote:true` row also stops
// holding its name once it is remoteHold old.
func occupied(rows []bus.Event, branch, crew string, nowMS int64) map[string]bool {
	// The newest `worker:` status row per branch, keyed by the branch its id
	// names: a finished worker frees the name it held.
	status := map[string]map[string]any{}
	// Each other branch's last dispatch row, in log order as the arm's
	// `group_by(.branch) | map(last)` took it.
	dispatch := map[string]map[string]any{}
	selfts, haveSelf := 0.0, false
	for _, ev := range rows {
		// The arm selected on `.crew_id == $crew`, a `--arg` string: jq's `==` is
		// false against null or a number, so a row that recorded no crew_id belongs
		// to no crew — not even the empty one `crew identity <branch>` resolves to
		// outside a task doc, where ev.CrewID and the crew are both "".
		doc, isObj := ev.Doc.(map[string]any)
		if !isObj {
			continue
		}
		if cid, isStr := doc["crew_id"].(string); !isStr || cid != crew {
			continue
		}
		if ev.Kind == bus.KindStatus {
			if wid, held := workerBranch(ev.From); held {
				if cur, seen := status[wid]; !seen || atLeast(doc["ts"], cur["ts"]) {
					status[wid] = doc
				}
			}
			continue
		}
		if ev.Kind != bus.KindDispatch {
			continue
		}
		// A branch that is not a string keys nothing: the arm's jq died on such a
		// row, taking every occupancy with it.
		name, isStr := doc["branch"].(string)
		if !isStr {
			continue
		}
		if name == branch {
			if ts, ok := number(doc["ts"]); ok && (!haveSelf || ts > selfts) {
				selfts, haveSelf = ts, true
			}
			continue
		}
		dispatch[name] = doc
	}
	// The arm's `$selfts`, 1e18 when this branch was never dispatched here: every
	// other branch is then older, so all of them hold their names.
	limit := 1e18
	if haveSelf {
		limit = selfts
	}
	occ := map[string]bool{}
	for name, doc := range dispatch {
		ts, hasTS := number(doc["ts"])
		if hasTS && ts > limit {
			continue // dispatched after us: it yields, we hold
		}
		body, _ := status[name]["body"].(map[string]any)
		if s, isStr := body["state"].(string); isStr && bus.State(s).Terminal() {
			continue
		}
		if remote, _ := doc["remote"].(bool); remote && hasTS && nowMS-int64(ts) >= remoteHold.Milliseconds() {
			continue
		}
		if held, ok := heldName(doc, name); ok {
			occ[held] = true
		}
	}
	return occ
}

// heldName is the codename a dispatch row holds for its branch: the string it
// stamped, or the pool name the branch hashes to when it recorded none — jq's
// `// ""`, which reads null, false and absent alike. ok is false for a name of any
// other type: bash @tsv'd it into a token no pool holds, so it held nothing.
func heldName(doc map[string]any, branch string) (string, bool) {
	switch name := doc["name"].(type) {
	case string:
		if name == "" {
			return names[Slot(branch)], true
		}
		return name, true
	case nil: // jq's `// "": null, false and absent are no name
		return names[Slot(branch)], true
	case bool:
		if !name {
			return names[Slot(branch)], true
		}
	}
	return "", false
}

// workerBranch is the fold's `ltrimstr("worker:") | sub("#[^#]*$";"")`: the branch
// a `worker:<branch>#<session>` id names. ok is false for every other sender (a
// role pane, the dispatcher), whose statuses never gate a name.
func workerBranch(from string) (string, bool) {
	branch, found := strings.CutPrefix(from, "worker:")
	if !found {
		return "", false
	}
	if i := strings.LastIndex(branch, "#"); i >= 0 {
		branch = branch[:i]
	}
	return branch, true
}

// atLeast reports whether left's `ts` orders at or after right's, the tie broken
// for the later row as jq's `max_by` breaks it. A `ts` that is not a number is
// jq's null: below every number, so the newest real row wins.
func atLeast(left, right any) bool {
	lt, lok := number(left)
	rt, rok := number(right)
	if !rok {
		return true
	}
	return lok && lt >= rt
}

// number reads a gojq-form value as a float. A literal out of float64 range
// reads as ±Inf, as the jq programs read it.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f, true
	case float64:
		return x, true
	}
	return 0, false
}

// windowNames is every codename a live or mirror window carries, from
// `tmux list-windows -a -F '#{@crew_name} #{@bridge_crew_name}'`. The arm matched
// them as whitespace tokens after `tr '\n' ' '`, and a failed read — its
// `2>/dev/null || true` — names nothing rather than refusing.
func windowNames(ctx context.Context, c tmux.Client) []string {
	if c == nil {
		return nil
	}
	out, err := c.Run(ctx, "list-windows", "-a", "-F", "#{@crew_name} #{@bridge_crew_name}")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// busEvents is the arm's `jq -s … "$log" 2>/dev/null || true` over the bus: no
// log, and one jq could not slurp, are both no rows — the pool fallback, not a
// refusal.
func busEvents(path string) []bus.Event {
	events, err := bus.ReadEvents(path)
	if err != nil {
		return nil
	}
	return events
}

func docs(rows []bus.Event) []any {
	out := make([]any, len(rows))
	for i, ev := range rows {
		out[i] = ev.Doc
	}
	return out
}

// emit prints one identity as the arm's `jq -nc` did: compact, on one line.
func emit(w io.Writer, id Identity) int {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	id.Color = jsonval.Clamp(id.Color)
	_ = enc.Encode(id)
	return 0
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
