// Package identity gives each branch a stable codename, colour and tmux
// colour: the one `dispatch` recorded, else the pool slot its name hashes to.
package identity

import (
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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

// At is the pool identity {name, color, tmux} at slot.
func At(slot int) jsonv.Value {
	return jsonv.Object(
		jsonv.Member{Key: "name", Val: jsonv.Str(names[slot])},
		jsonv.Member{Key: "color", Val: jsonv.Str(colors[slot])},
		jsonv.Member{Key: "tmux", Val: jsonv.Str(tmuxc[slot])},
	)
}

// Palette is the `_colors` pool as a value: the roster renderer's D2 program
// looks a row's colour up in it to decide whether the branch box gets a stroke,
// which is why the whole list travels rather than one slot.
func Palette() jsonv.Value {
	out := make([]jsonv.Value, len(colors))
	for i, c := range colors {
		out[i] = jsonv.Str(c)
	}
	return jsonv.Array(out...)
}

// Recorded is the identity of the last well-formed `dispatch` event for
// branch, across every crew. Events that are not objects never match.
func Recorded(events []jsonv.Value, branch string) (jsonv.Value, bool) {
	want, _ := jsonv.Str(branch).AsString()
	var last jsonv.Value
	found := false
	for _, ev := range events {
		if id, ok := recordedIn(ev, want); ok {
			last, found = id, true
		}
	}
	return last, found
}

func recordedIn(ev jsonv.Value, branch string) (jsonv.Value, bool) {
	if ev.Kind() != jsonv.KindObject {
		return jsonv.Value{}, false
	}
	if !stringMember(ev, "kind", func(s string) bool { return s == "dispatch" }) ||
		!stringMember(ev, "branch", func(s string) bool { return s == branch }) ||
		!stringMember(ev, "name", nameRe.MatchString) ||
		!stringMember(ev, "tmux", tmuxRe.MatchString) {
		return jsonv.Value{}, false
	}
	name, _ := ev.Get("name")
	color, _ := ev.Get("color")
	tmux, _ := ev.Get("tmux")
	return jsonv.Object(
		jsonv.Member{Key: "name", Val: name},
		jsonv.Member{Key: "color", Val: color},
		jsonv.Member{Key: "tmux", Val: tmux},
	), true
}

// RecordedAll folds every branch's recorded identity in a single pass: for
// each branch, the identity of its last well-formed `dispatch` event, the
// same predicate Recorded reports per branch. The roster builds its identity
// map from this so a fold is O(events + rows), not O(rows × events) (#821).
// Only branches with a recorded identity appear as keys.
func RecordedAll(events []jsonv.Value) map[string]jsonv.Value {
	last := map[string]jsonv.Value{}
	for _, ev := range events {
		if ev.Kind() != jsonv.KindObject {
			continue
		}
		br, _ := ev.Get("branch")
		branch, isStr := br.AsString()
		if !isStr {
			continue
		}
		if id, ok := recordedIn(ev, branch); ok {
			last[branch] = id
		}
	}
	return last
}

// stringMember reports whether ev[key] is a string satisfying ok.
func stringMember(ev jsonv.Value, key string, ok func(string) bool) bool {
	v, _ := ev.Get(key)
	s, isStr := v.AsString()
	return isStr && ok(s)
}

// For is the recorded identity of branch from a RecordedAll map, else its
// pool identity. A branch that is empty or holds a space, tab or newline is
// never looked up by the roster (the arm's `$(...)` word-split drops it).
func For(recorded map[string]jsonv.Value, branch string) jsonv.Value {
	if id, ok := recorded[branch]; ok {
		return id
	}
	return At(Slot(branch))
}

// Probes is the arm's tmux read, injected so tests never touch a server.
type Probes struct {
	// Windows is `tmux list-windows -a -F '#{@crew_name} #{@bridge_crew_name}'`:
	// every codename a live or mirror window already carries, whitespace-separated.
	// It ran under `2>/dev/null || true`, so a failed read — or no probe at all —
	// names nothing rather than refusing.
	Windows func() (string, error)
}

// remoteHold is how long a `remote:true` dispatch row holds its codename. The
// remote worker's terminal status never reaches this bus, so without a bound the
// name would never free.
const remoteHold = 24 * time.Hour

// Options is everything Run reads beyond the bus: the crew the arm defaults to
// (`_crew_id`), the tmux probe, the environment CREW_IDENTITY_AVOID comes from
// and the clock the remote hold is measured on. A nil Getenv reads no variable
// and a nil Now is the wall clock.
type Options struct {
	CrewID func() string
	Probes Probes
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
func Run(req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
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
		for _, name := range Occupied(busEvents(paths.Log), crew, o.Probes, now().UnixMilli()) {
			say(stdout, "%s\n", name)
		}
		return 0
	}
	return emit(stdout, Assign(busEvents(paths.Log), req.Branch, crew, o.Probes, avoidList(o.Getenv), now().UnixMilli()))
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
func Occupied(rows []bus.Event, crew string, p Probes, nowMS int64) []string {
	occ := occupied(rows, "", crew, nowMS)
	for _, win := range windowNames(p) {
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
func Assign(rows []bus.Event, branch, crew string, p Probes, avoid []string, nowMS int64) jsonv.Value {
	// The recorded name is gated by the bus alone: tmux names join only after it.
	occ := occupied(rows, branch, crew, nowMS)
	for _, name := range avoid {
		occ[name] = true
	}
	if rec, ok := Recorded(rawValues(rows), branch); ok {
		name, _ := rec.Get("name")
		if held, _ := name.AsString(); !occ[held] {
			return rec
		}
	}
	for _, win := range windowNames(p) {
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
	status := map[string]bus.Event{}
	// Each other branch's last dispatch row, in log order as the arm's
	// `group_by(.branch) | map(last)` took it.
	dispatch := map[string]bus.Event{}
	selfts, haveSelf := 0.0, false
	for _, ev := range rows {
		// The arm selected on `.crew_id == $crew`, a `--arg` string: jq's `==` is
		// false against null or a number, so a row that recorded no crew_id belongs
		// to no crew — not even the empty one `crew identity <branch>` resolves to
		// outside a task doc, where ev.CrewID and the crew are both "".
		stored, _ := ev.Raw.Get("crew_id")
		cid, isStr := stored.AsString()
		if !isStr || cid != crew {
			continue
		}
		if ev.Kind == bus.KindStatus {
			if wid, held := workerBranch(ev.From); held {
				if cur, seen := status[wid]; !seen || atLeast(ev.TS, cur.TS) {
					status[wid] = ev
				}
			}
			continue
		}
		if ev.Kind != bus.KindDispatch {
			continue
		}
		// A branch that is not a string keys nothing: the arm's jq died on such a
		// row, taking every occupancy with it.
		key, _ := ev.Raw.Get("branch")
		name, isStr := key.AsString()
		if !isStr {
			continue
		}
		if name == branch {
			if ts, ok := ev.TS.AsFloat(); ok && (!haveSelf || ts > selfts) {
				selfts, haveSelf = ts, true
			}
			continue
		}
		dispatch[name] = ev
	}
	// The arm's `$selfts`, 1e18 when this branch was never dispatched here: every
	// other branch is then older, so all of them hold their names.
	limit := 1e18
	if haveSelf {
		limit = selfts
	}
	occ := map[string]bool{}
	for name, ev := range dispatch {
		if ts, ok := ev.TS.AsFloat(); ok && ts > limit {
			continue // dispatched after us: it yields, we hold
		}
		if st, ok := status[name].Body.Get("state"); ok {
			if s, isStr := st.AsString(); isStr && bus.State(s).Terminal() {
				continue
			}
		}
		if remote, _ := ev.Raw.Get("remote"); remote.Kind() == jsonv.KindTrue {
			if ts, ok := ev.TS.AsFloat(); ok && nowMS-int64(ts) >= remoteHold.Milliseconds() {
				continue
			}
		}
		if held, ok := heldName(ev, name); ok {
			occ[held] = true
		}
	}
	return occ
}

// heldName is the codename a dispatch row holds for its branch: the string it
// stamped, or the pool name the branch hashes to when it recorded none — jq's
// `// ""`, which reads null, false and absent alike. ok is false for a name of any
// other type: bash @tsv'd it into a token no pool holds, so it held nothing.
func heldName(ev bus.Event, branch string) (string, bool) {
	name, _ := ev.Raw.Get("name")
	switch {
	case name.Kind() == jsonv.KindString:
		s, _ := name.AsString()
		if s == "" {
			return names[Slot(branch)], true
		}
		return s, true
	case name.IsNull() || name.Kind() == jsonv.KindFalse: // jq's `// "": both are no name
		return names[Slot(branch)], true
	default:
		return "", false
	}
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
func atLeast(left, right jsonv.Value) bool {
	lt, lok := left.AsFloat()
	rt, rok := right.AsFloat()
	if !rok {
		return true
	}
	return lok && lt >= rt
}

// windowNames is the arm's tmux read flattened to the codenames it lists. The arm
// matched them as whitespace tokens after `tr '\n' ' '`, and a failed read — its
// `2>/dev/null` — names nothing.
func windowNames(p Probes) []string {
	if p.Windows == nil {
		return nil
	}
	out, err := p.Windows()
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

func rawValues(rows []bus.Event) []jsonv.Value {
	out := make([]jsonv.Value, len(rows))
	for i, ev := range rows {
		out[i] = ev.Raw
	}
	return out
}

// emit prints one identity as the arm's `jq -nc` did: compact, on one line.
func emit(w io.Writer, v jsonv.Value) int {
	say(w, "%s\n", jsonv.Append(nil, v, jsonv.Options{}))
	return 0
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
