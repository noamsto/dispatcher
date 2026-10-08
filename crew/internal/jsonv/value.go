package jsonv

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kind is a JSON value's type. The constants are in jq's sort order, and the
// first seven double as indexes into a Palette.
type Kind uint8

const (
	KindNull Kind = iota
	KindFalse
	KindTrue
	KindNumber
	KindString
	KindArray
	KindObject
)

// String returns jq's `type` name, so both booleans are "boolean".
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindFalse, KindTrue:
		return "boolean"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	case KindObject:
		return "object"
	}
	return "invalid"
}

// Member is one object entry.
type Member struct {
	Key string
	Val Value
}

// Value is an immutable-by-convention JSON value whose objects keep insertion
// order. The zero Value is null. Copies share array and object storage, so
// Set, Delete and Push mutate every copy; build fresh values where that matters.
type Value struct {
	kind Kind
	n    float64
	s    string // string content, or the canonical text of a number literal
	a    []Value
	m    []Member
}

// TypeError is a jq runtime type error. Callers map it to jq's exit status 5.
type TypeError struct{ Msg string }

func (e *TypeError) Error() string { return e.Msg }

// TypeErrorf builds a *TypeError for a jq program port to return.
func TypeErrorf(format string, args ...any) error {
	return &TypeError{Msg: fmt.Sprintf(format, args...)}
}

func Null() Value { return Value{} }

func Bool(b bool) Value {
	if b {
		return Value{kind: KindTrue}
	}
	return Value{kind: KindFalse}
}

// Num is a computed number: it prints as jq prints arithmetic results.
func Num(f float64) Value { return Value{kind: KindNumber, n: f} }

// Str repairs invalid UTF-8 the way jq does when it builds a string.
func Str(s string) Value { return Value{kind: KindString, s: validUTF8([]byte(s))} }

func Array(vs ...Value) Value { return Value{kind: KindArray, a: vs} }

// Object builds an object; a repeated key keeps its first position and last value.
func Object(ms ...Member) Value {
	o := Value{kind: KindObject, m: make([]Member, 0, len(ms))}
	for _, m := range ms {
		o.Set(m.Key, m.Val)
	}
	return o
}

func (v Value) Kind() Kind   { return v.kind }
func (v Value) IsNull() bool { return v.kind == KindNull }

// Truthy is jq's truthiness: everything except null and false.
func (v Value) Truthy() bool { return v.kind != KindNull && v.kind != KindFalse }

// Or is jq's `v // def`.
func (v Value) Or(def Value) Value {
	if v.Truthy() {
		return v
	}
	return def
}

func (v Value) AsString() (string, bool) {
	if v.kind != KindString {
		return "", false
	}
	return v.s, true
}

func (v Value) AsFloat() (float64, bool) {
	if v.kind != KindNumber {
		return 0, false
	}
	return v.n, true
}

// Len is the element count of an array or object, and 0 for anything else.
func (v Value) Len() int {
	switch v.kind {
	case KindArray:
		return len(v.a)
	case KindObject:
		return len(v.m)
	case KindNull, KindFalse, KindTrue, KindNumber, KindString:
	}
	return 0
}

// Elems returns the array's elements. The slice is shared; do not modify it.
func (v Value) Elems() []Value { return v.a }

// At returns the i-th array element.
func (v Value) At(i int) (Value, bool) {
	if v.kind != KindArray || i < 0 || i >= len(v.a) {
		return Value{}, false
	}
	return v.a[i], true
}

// Push appends to an array.
func (v *Value) Push(x Value) {
	if v.kind != KindArray {
		panic("jsonv: Push on " + v.kind.String())
	}
	v.a = append(v.a, x)
}

// Members returns the object's entries in order. The slice is shared; do not modify it.
func (v Value) Members() []Member { return v.m }

// Get looks up an object member.
func (v Value) Get(key string) (Value, bool) {
	for _, m := range v.m {
		if m.Key == key {
			return m.Val, true
		}
	}
	return Value{}, false
}

// Set replaces a member in place, or appends it when the key is new.
func (v *Value) Set(key string, x Value) {
	if v.kind != KindObject {
		panic("jsonv: Set on " + v.kind.String())
	}
	for i := range v.m {
		if v.m[i].Key == key {
			v.m[i].Val = x
			return
		}
	}
	v.m = append(v.m, Member{Key: key, Val: x})
}

// Delete removes a member, keeping the order of the rest.
func (v *Value) Delete(key string) {
	if v.kind != KindObject {
		panic("jsonv: Delete on " + v.kind.String())
	}
	for i := range v.m {
		if v.m[i].Key == key {
			v.m = append(v.m[:i], v.m[i+1:]...)
			return
		}
	}
}

// Index is jq's `.key`: null yields null, an object yields the member or null,
// anything else is a TypeError.
func (v Value) Index(key string) (Value, error) {
	switch v.kind {
	case KindNull:
		return Value{}, nil
	case KindObject:
		x, _ := v.Get(key)
		return x, nil
	case KindFalse, KindTrue, KindNumber, KindString, KindArray:
	}
	return Value{}, TypeErrorf("Cannot index %s with %q", v.kind, key)
}

// SliceString is jq's `.[from:to]`: strings are cut by codepoint, arrays by
// element, null stays null, and any other type is a TypeError.
func (v Value) SliceString(from, to int) (Value, error) {
	switch v.kind {
	case KindNull:
		return Value{}, nil
	case KindString:
		runes := []rune(v.s)
		from, to = clampSlice(from, to, len(runes))
		return Str(string(runes[from:to])), nil
	case KindArray:
		from, to = clampSlice(from, to, len(v.a))
		return Array(v.a[from:to:to]...), nil
	case KindFalse, KindTrue, KindNumber, KindObject:
	}
	return Value{}, TypeErrorf("Cannot index %s with object", v.kind)
}

func clampSlice(from, to, n int) (int, int) {
	if from < 0 {
		from += n
	}
	if to < 0 {
		to += n
	}
	from = min(max(from, 0), n)
	to = min(max(to, from), n)
	return from, to
}

// validUTF8 replaces invalid UTF-8 as jq's jvp_utf8_next does: a bad lead or
// continuation byte is one U+FFFD, a truncated sequence at the end is one
// U+FFFD for all its remaining bytes, and a lead byte with valid continuations
// that decodes to an overlong, surrogate or out-of-range code point is consumed
// whole.
func validUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[:size])
			b = b[size:]
			continue
		}
		sb.WriteRune(utf8.RuneError)
		b = b[invalidRun(b):]
	}
	return sb.String()
}

// invalidRun is how many bytes jq swallows into the one U+FFFD for the invalid
// sequence at the start of b.
func invalidRun(b []byte) int {
	want := 0
	switch c := b[0]; {
	case c >= 0xf0 && c <= 0xf4:
		want = 4
	case c >= 0xe0 && c <= 0xef:
		want = 3
	case c >= 0xc2 && c <= 0xdf:
		want = 2
	default:
		return 1
	}
	if want > len(b) {
		return len(b)
	}
	for i := 1; i < want; i++ {
		if b[i]&0xc0 != 0x80 {
			return i
		}
	}
	return want
}
