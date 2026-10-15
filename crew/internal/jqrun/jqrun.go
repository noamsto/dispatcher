// Package jqrun runs crew's original crew.sh jq programs through gojq, the
// pure-Go jq library, instead of hand-translating each program into Go.
//
// The contract is value-equal JSON (#821), so gojq's alphabetical object keys
// are fine; the bus still decodes with jsonv (it accepts jq's NaN extension,
// which encoding/json rejects) and main.go still encodes with jsonv (canonical
// pretty output and $JQ_COLORS).
//
// Run and Compile take jsonv values; RunAny and CompileAny take gojq's own
// form directly and are the shared core the jsonv entry points adapt.
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

// Option extends the jq language one Run compiles against.
type Option struct{ compiler gojq.CompilerOption }

// WithJQFromJSON defines `_jqfromjson`, jq 1.8.2's fromjson over jsonv's
// decoder. gojq's own fromjson accepts what jq refuses (a high surrogate
// escape with no low one after it) and refuses what jq accepts (`-nan`), so a
// program whose verdict hinges on fromjson failing calls this instead.
func WithJQFromJSON() Option {
	return Option{gojq.WithFunction("_jqfromjson", 0, 0, func(v any, _ []any) any {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%v cannot be parsed as JSON", v)
		}
		parsed, err := jsonv.ParseOne(s)
		if err != nil {
			return fmt.Errorf("%w (while parsing '%s')", err, s)
		}
		return jsonv.ToAny(parsed)
	})}
}

// Run executes prog like `jq -s -c prog`: input is the slurped event array,
// vars become $name string/JSON variables, and the `now` builtin is frozen
// to nowSec so folds are deterministic (see freezeNow). The program must
// emit exactly one value; a gojq runtime error (jq's type error) is returned
// as an error, which main maps to the jq-failure exit status.
func Run(prog string, input []jsonv.Value, now float64, vars map[string]jsonv.Value, opts ...Option) (jsonv.Value, error) {
	p, err := Compile(prog, now, vars, opts...)
	if err != nil {
		return jsonv.Value{}, err
	}
	return p.Run(jsonv.Array(input...))
}

// RunAny is Run over gojq's form: input, the elements of the slurped array,
// and vars must be json.Number, float64, string, bool, nil, []any or
// map[string]any. The result is in Normalize's form.
func RunAny(prog string, input []any, now float64, vars map[string]any, opts ...Option) (any, error) {
	p, err := CompileAny(prog, now, vars, opts...)
	if err != nil {
		return nil, err
	}
	return p.RunAny(input)
}

// Program is a jq program compiled once, with its variables bound, for a
// caller that runs it over many inputs (one per line, like `jq -R`) where
// compiling per input would dominate.
type Program struct {
	code   *gojq.Code
	values []any
}

// Compile is Run's compile step: prog, vars, now and opts as for Run.
func Compile(prog string, now float64, vars map[string]jsonv.Value, opts ...Option) (*Program, error) {
	goVars := make(map[string]any, len(vars))
	for name, v := range vars {
		goVars[name] = jsonv.ToAny(v)
	}
	return CompileAny(prog, now, goVars, opts...)
}

// CompileAny is Compile over gojq's form: vars must be json.Number, float64,
// string, bool, nil, []any or map[string]any.
func CompileAny(prog string, now float64, vars map[string]any, opts ...Option) (*Program, error) {
	names := make([]string, 0, len(vars))
	values := make([]any, 0, len(vars))
	for name := range vars {
		names = append(names, "$"+name)
	}
	sort.Strings(names)
	for _, name := range names {
		values = append(values, vars[name[1:]])
	}

	q, err := gojq.Parse(prog)
	if err != nil {
		return nil, fmt.Errorf("jq program: %w", err)
	}
	freezeNow(q, now)
	compileOpts := []gojq.CompilerOption{gojq.WithVariables(names)}
	for _, o := range opts {
		compileOpts = append(compileOpts, o.compiler)
	}
	code, err := gojq.Compile(q, compileOpts...)
	if err != nil {
		return nil, fmt.Errorf("jq program: %w", err)
	}
	return &Program{code: code, values: values}, nil
}

// Run evaluates the program on input and returns its first output. No
// output is an error, as is a runtime error raised before the first output.
func (p *Program) Run(input jsonv.Value) (jsonv.Value, error) {
	out, err := p.RunAny(jsonv.ToAny(input))
	if err != nil {
		return jsonv.Value{}, err
	}
	return fromGo(out)
}

// RunAny is Run over gojq's form: input must be json.Number, float64, string,
// bool, nil, []any or map[string]any. The result is in Normalize's form.
func (p *Program) RunAny(input any) (any, error) {
	v, ok := p.code.Run(input, p.values...).Next()
	if !ok {
		return nil, fmt.Errorf("jq program produced no output")
	}
	if err, isErr := v.(error); isErr {
		return nil, err
	}
	return Normalize(v)
}

// Normalize maps a gojq output to json.Number (exact text of integers and
// big numbers), float64 (computed doubles, including NaN and infinities),
// string, bool, nil, []any and map[string]any.
func Normalize(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, float64, json.Number, string:
		return t, nil
	case int:
		return json.Number(strconv.FormatInt(int64(t), 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(t, 10)), nil
	case uint64:
		return json.Number(strconv.FormatUint(t, 10)), nil
	case *big.Int:
		return json.Number(t.String()), nil
	case *big.Float:
		return json.Number(t.Text('g', -1)), nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			n, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			n, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	default:
		return nil, fmt.Errorf("jqrun: unexpected output type %T", v)
	}
}

// freezeNow shadows gojq's builtin `now` by prepending `def now: <now>;` to
// the parsed program. gojq resolves a bare call by taking the last matching
// FuncDef, so this replaces the builtin but yields to any `def now:` the
// program itself declares. Only the parsed AST changes, never the source
// text, so string literals and comments keep their `now` verbatim.
func freezeNow(q *gojq.Query, now float64) {
	body := &gojq.Query{Term: &gojq.Term{
		Type:   gojq.TermTypeNumber,
		Number: strconv.FormatFloat(now, 'g', -1, 64),
	}}
	q.FuncDefs = append([]*gojq.FuncDef{{Name: "now", Body: body}}, q.FuncDefs...)
}

// fromGo converts Normalize's output back. Numbers rebuild from their exact
// text where there is one (passed-through literals, big integers, integer
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
