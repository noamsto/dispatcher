// The bus is append-only, and this is the only place that appends to it: the
// `_bus_append` write plus the two helpers that keep a line short enough to
// survive it (`_fit_line` and `_shrink`). They are ports of the crew.sh helpers
// of the same name, which stay there while `status`, `msg` and `reply` call
// them; the drift guards are `hold: the Go row's title is _shrink's own output
// at the keep it implies` in crew.bats and TestCrewShLineContract here.
package bus

import (
	"os"
	"strings"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// LineMax is `_LINE_MAX`: the byte cap one bus line may reach. It is a cap on
// the line, not on the text inside it, which is why FitLine re-measures the
// encoded row instead of cutting the text to a computed length.
const LineMax = 4096

// elided is `_ELIDED` — a leading space, a three-byte ellipsis, and the marker
// that says a leaf was cut rather than a record lost.
const elided = " …[elided]"

// Append writes line to the end of the bus log, one write. It is `_bus_append`:
// if the log's last byte is not a newline, one is prefixed so the new row cannot
// join a torn fragment and corrupt two records into one line.
//
// The byte is read twice around a second stat, as the helper does: a lone
// non-newline also happens when a concurrent writer's line is only partly
// visible, so only a size that stays put counts as a torn line — otherwise a
// healthy log would occasionally gain a blank line.
func Append(path, line string) error {
	var prefix string
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		if b, ok := lastByte(path, st.Size()); ok && b != '\n' {
			if st2, err := os.Stat(path); err == nil && st2.Size() == st.Size() {
				prefix = "\n"
			}
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(prefix + line + "\n")); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// lastByte reads the byte at size-1, the helper's `dd bs=1 skip=size-1 count=1`.
// A failed read is the helper's empty `$(...)`: no newline is prefixed.
func lastByte(path string, size int64) (byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	n, _ := f.ReadAt(b[:], size-1)
	return b[0], n == 1
}

// FitLine is `_fit_line`: it returns the line build(text) builds, with text
// shortened until the ENCODED line fits LineMax. It re-measures each pass rather
// than computing a cut from the input length, because JSON escaping is nonlinear
// (a control byte becomes six characters). The proportional guess is floored at a
// 3/4 step so every pass strictly shrinks and the loop terminates.
//
// n is bytes (`wc -c`) and keep characters (`${#full}`): the two differ exactly
// where the text is not ASCII, which is what the guard in crew.bats pins.
func FitLine(build func(text string) string, full string) string {
	line := build(full)
	keep := utf8.RuneCountInString(full)
	for {
		n := len(line)
		if n <= LineMax || keep == 0 {
			return line
		}
		step := keep * LineMax / n
		if three := keep * 3 / 4; step >= three {
			step = three
		}
		keep = step
		line = build(Shrink(full, keep))
	}
}

// Shrink is `_shrink`: shorten text to roughly keep characters. A sink body is
// itself JSON, and cutting it as a blob ends the string mid-object — the bus line
// stays valid but the body no longer parses, so a reader loses the whole record.
// So a text that parses as JSON shortens each long string leaf and re-encodes,
// which keeps the record's shape and its short keys intact; anything else gets
// the blob cut.
//
// The branch test is `jq -e 'type=="object" or type=="array"'` read off the whole
// stream: jq's `-e` is the LAST output's truthiness, and its `jq -c walk(...)`
// then rewrites every value it parsed, one per line. A parse error, or no value at
// all, is jq's exit 2 and 4 — the blob branch either way.
func Shrink(text string, keep int) string {
	if vs, err := jsonv.DecodeStream(strings.NewReader(text)); err == nil && len(vs) > 0 {
		container := false
		switch vs[len(vs)-1].Kind() {
		case jsonv.KindObject, jsonv.KindArray:
			container = true
		case jsonv.KindNull, jsonv.KindFalse, jsonv.KindTrue, jsonv.KindNumber, jsonv.KindString:
		}
		if container {
			out := make([]string, len(vs))
			for i, v := range vs {
				out[i] = string(jsonv.Append(nil, shrinkLeaves(v, keep), jsonv.Options{}))
			}
			return strings.Join(out, "\n")
		}
	}
	r := []rune(text)
	if keep > len(r) {
		keep = len(r)
	}
	return string(r[:keep]) + elided
}

// shrinkLeaves is jq's `walk(f)`: f applied to every value post-order, object key
// order kept (jq rebuilds objects from keys_unsorted).
func shrinkLeaves(v jsonv.Value, keep int) jsonv.Value {
	switch v.Kind() {
	case jsonv.KindArray:
		out := make([]jsonv.Value, 0, v.Len())
		for _, e := range v.Elems() {
			out = append(out, shrinkLeaves(e, keep))
		}
		return jsonv.Array(out...)
	case jsonv.KindObject:
		ms := make([]jsonv.Member, 0, len(v.Members()))
		for _, m := range v.Members() {
			ms = append(ms, jsonv.Member{Key: m.Key, Val: shrinkLeaves(m.Val, keep)})
		}
		return jsonv.Object(ms...)
	case jsonv.KindString:
		s, _ := v.AsString()
		r := []rune(s)
		if len(r) > keep {
			return jsonv.Str(string(r[:keep]) + elided)
		}
	case jsonv.KindNull, jsonv.KindFalse, jsonv.KindTrue, jsonv.KindNumber:
	}
	return v
}
