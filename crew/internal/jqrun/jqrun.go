// Package jqrun runs crew's original crew.sh jq programs through gojq, the
// pure-Go jq library, instead of hand-translating each program into Go.
//
// The contract is value-equal JSON (#821), so gojq's alphabetical object keys
// are fine; the bus still decodes with jsonv (it accepts jq's NaN extension,
// which encoding/json rejects) and main.go still encodes with jsonv (canonical
// pretty output and $JQ_COLORS).
package jqrun

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"

	"github.com/itchyny/gojq"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// Run executes prog like `jq -s -c prog`: input is the slurped event array,
// vars become $name string/JSON variables, and the bare `now` builtin is
// rewritten to $now = nowSec so folds are deterministic. The program must
// emit exactly one value; a gojq runtime error (jq's type error) is returned
// as an error, which main maps to the jq-failure exit status.
func Run(prog string, input []jsonv.Value, now float64, vars map[string]jsonv.Value) (jsonv.Value, error) {
	names := make([]string, 0, len(vars)+1)
	values := make([]any, 0, len(vars)+1)
	names = append(names, "$now")
	values = append(values, now)
	for name := range vars {
		names = append(names, "$"+name)
	}
	sort.Strings(names[1:])
	for _, name := range names[1:] {
		v, err := toGo(vars[name[1:]])
		if err != nil {
			return jsonv.Value{}, err
		}
		values = append(values, v)
	}

	q, err := gojq.Parse(injectNow(prog))
	if err != nil {
		return jsonv.Value{}, fmt.Errorf("jq program: %w", err)
	}
	code, err := gojq.Compile(q, gojq.WithVariables(names))
	if err != nil {
		return jsonv.Value{}, fmt.Errorf("jq program: %w", err)
	}
	in := make([]any, len(input))
	for i, ev := range input {
		v, err := toGo(ev)
		if err != nil {
			return jsonv.Value{}, err
		}
		in[i] = v
	}

	v, ok := code.Run(in, values...).Next()
	if !ok {
		return jsonv.Value{}, fmt.Errorf("jq program produced no output")
	}
	if err, isErr := v.(error); isErr {
		return jsonv.Value{}, err
	}
	return fromGo(v)
}

// injectNow rewrites the bare `now` builtin to `$now`, on word boundaries so
// words like "last-known" in program comments survive.
func injectNow(prog string) string {
	var b []byte
	for i := 0; i < len(prog); {
		if len(prog)-i >= 3 && prog[i:i+3] == "now" &&
			(i == 0 || !isWordByte(prog[i-1])) &&
			(i+3 == len(prog) || !isWordByte(prog[i+3])) {
			b = append(b, "$now"...)
			i += 3
			continue
		}
		b = append(b, prog[i])
		i++
	}
	return string(b)
}

func isWordByte(c byte) bool {
	return c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// toGo converts a jsonv value to gojq's representation. Number literals go
// over as json.Number of their canonical text: gojq normalizes those lazily,
// so pass-through output keeps the literal verbatim (jq 1.7+ semantics) and
// big integers order exactly, while arithmetic still computes in doubles.
// Computed numbers (NaN, infinities, decoded values beyond decNumber's
// range) carry no text and go over as float64.
func toGo(v jsonv.Value) (any, error) {
	switch v.Kind() {
	case jsonv.KindNull:
		return nil, nil
	case jsonv.KindTrue:
		return true, nil
	case jsonv.KindFalse:
		return false, nil
	case jsonv.KindNumber:
		if text := v.NumberText(); text != "" {
			return json.Number(text), nil
		}
		f, _ := v.AsFloat()
		return f, nil
	case jsonv.KindString:
		s, _ := v.AsString()
		return s, nil
	case jsonv.KindArray:
		out := make([]any, 0, v.Len())
		for _, e := range v.Elems() {
			g, err := toGo(e)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case jsonv.KindObject:
		out := make(map[string]any, v.Len())
		for _, m := range v.Members() {
			g, err := toGo(m.Val)
			if err != nil {
				return nil, err
			}
			out[m.Key] = g
		}
		return out, nil
	default:
		return nil, fmt.Errorf("jqrun: cannot convert kind %v", v.Kind())
	}
}

// fromGo converts gojq's output back. Numbers rebuild from their exact text
// where gojq has one (passed-through literals, big integers, integer
// results); computed doubles become computed values. gojq objects are
// map[string]any (order lost), so members are emitted sorted by key:
// output is deterministic and, under the value-equal contract, key order is
// free.
func fromGo(v any) (jsonv.Value, error) {
	switch t := v.(type) {
	case nil:
		return jsonv.Null(), nil
	case bool:
		return jsonv.Bool(t), nil
	case float64:
		return jsonv.Num(t), nil
	case int:
		return numText(strconv.FormatInt(int64(t), 10))
	case int64:
		return numText(strconv.FormatInt(t, 10))
	case uint64:
		return numText(strconv.FormatUint(t, 10))
	case *big.Int:
		return numText(t.String())
	case *big.Float:
		return numText(t.Text('g', -1))
	case json.Number:
		return numText(t.String())
	case string:
		return jsonv.Str(t), nil
	case []any:
		out := make([]jsonv.Value, len(t))
		for i, e := range t {
			ev, err := fromGo(e)
			if err != nil {
				return jsonv.Value{}, err
			}
			out[i] = ev
		}
		return jsonv.Array(out...), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		members := make([]jsonv.Member, len(keys))
		for i, k := range keys {
			val, err := fromGo(t[k])
			if err != nil {
				return jsonv.Value{}, err
			}
			members[i] = jsonv.Member{Key: k, Val: val}
		}
		return jsonv.Object(members...), nil
	default:
		return jsonv.Value{}, fmt.Errorf("jqrun: unexpected output type %T", v)
	}
}

// numText rebuilds a jsonv number from gojq's number text. gojq only emits
// literals its own parser produced, so ParseNumber accepts every input here.
func numText(text string) (jsonv.Value, error) {
	if v, ok := jsonv.ParseNumber(text); ok {
		return v, nil
	}
	return jsonv.Value{}, fmt.Errorf("jqrun: cannot rebuild number %q", text)
}
