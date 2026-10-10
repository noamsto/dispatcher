// Package identity gives each branch a stable codename, colour and tmux
// colour: the one `dispatch` recorded, else the pool slot its name hashes to.
package identity

import (
	"regexp"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// The pools mirror _names/_colors/_tmuxc in adapters/core/crew.sh; a test
// guards them against drift.
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
