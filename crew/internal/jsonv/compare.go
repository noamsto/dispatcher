package jsonv

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// Compare is jq's total order: null < false < true < numbers < strings <
// arrays < objects. It returns -1, 0 or 1.
func Compare(a, b Value) int {
	if a.kind != b.kind {
		return cmp.Compare(a.kind, b.kind)
	}
	switch a.kind {
	case KindNull, KindFalse, KindTrue:
		return 0
	case KindNumber:
		return compareNumbers(a, b)
	case KindString:
		return strings.Compare(a.s, b.s)
	case KindArray:
		return slices.CompareFunc(a.a, b.a, Compare)
	case KindObject:
		return compareObjects(a, b)
	}
	return 0
}

// Equal is jq's `==`.
func (v Value) Equal(w Value) bool { return Compare(v, w) == 0 }

// compareNumbers treats NaN as below every number, itself included, as jq
// does. Two literals compare exactly; anything else compares as doubles.
func compareNumbers(a, b Value) int {
	switch {
	case math.IsNaN(a.n):
		return -1
	case math.IsNaN(b.n):
		return 1
	}
	if a.s != "" && b.s != "" && a.n == b.n {
		x, _ := parseDecimal(a.s)
		y, _ := parseDecimal(b.s)
		return compareDecimals(x, y)
	}
	return cmp.Compare(a.n, b.n)
}

// compareObjects orders by sorted key list first, then by the values in key order.
func compareObjects(a, b Value) int {
	ma, mb := sortedMembers(a), sortedMembers(b)
	if c := slices.CompareFunc(ma, mb, func(x, y Member) int { return strings.Compare(x.Key, y.Key) }); c != 0 {
		return c
	}
	for i := range ma {
		if c := Compare(ma[i].Val, mb[i].Val); c != 0 {
			return c
		}
	}
	return 0
}

func sortedMembers(o Value) []Member {
	ms := slices.Clone(o.m)
	slices.SortFunc(ms, func(x, y Member) int { return strings.Compare(x.Key, y.Key) })
	return ms
}

type keyed struct{ key, val Value }

func keyAll(vs []Value, key func(Value) (Value, error)) ([]keyed, error) {
	ks := make([]keyed, len(vs))
	for i, v := range vs {
		k, err := key(v)
		if err != nil {
			return nil, err
		}
		ks[i] = keyed{k, v}
	}
	return ks, nil
}

func sortKeyed(ks []keyed) {
	slices.SortStableFunc(ks, func(a, b keyed) int { return Compare(a.key, b.key) })
}

// SortBy is jq's sort_by: a stable sort by key, returning a new slice.
func SortBy(vs []Value, key func(Value) (Value, error)) ([]Value, error) {
	ks, err := keyAll(vs, key)
	if err != nil {
		return nil, err
	}
	sortKeyed(ks)
	out := make([]Value, len(ks))
	for i, k := range ks {
		out[i] = k.val
	}
	return out, nil
}

// GroupBy is jq's group_by: groups in key order, each in input order.
func GroupBy(vs []Value, key func(Value) (Value, error)) ([][]Value, error) {
	ks, err := keyAll(vs, key)
	if err != nil {
		return nil, err
	}
	sortKeyed(ks)
	var groups [][]Value
	for i, k := range ks {
		if i == 0 || Compare(ks[i-1].key, k.key) != 0 {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], k.val)
	}
	return groups, nil
}

// MaxBy is jq's max_by: the last element among those with the largest key,
// or null for no elements.
func MaxBy(vs []Value, key func(Value) (Value, error)) (Value, error) {
	return extremeBy(vs, key, func(c int) bool { return c >= 0 })
}

// MinBy is jq's min_by: the first element among those with the smallest key,
// or null for no elements.
func MinBy(vs []Value, key func(Value) (Value, error)) (Value, error) {
	return extremeBy(vs, key, func(c int) bool { return c < 0 })
}

func extremeBy(vs []Value, key func(Value) (Value, error), replace func(int) bool) (Value, error) {
	ks, err := keyAll(vs, key)
	if err != nil {
		return Value{}, err
	}
	if len(ks) == 0 {
		return Value{}, nil
	}
	best := ks[0]
	for _, k := range ks[1:] {
		if replace(Compare(k.key, best.key)) {
			best = k
		}
	}
	return best.val, nil
}

// Unique is jq's unique: sorted, keeping the first of each run of equal values.
func Unique(vs []Value) []Value {
	sorted := slices.Clone(vs)
	slices.SortStableFunc(sorted, Compare)
	return slices.CompactFunc(sorted, func(a, b Value) bool { return Compare(a, b) == 0 })
}
