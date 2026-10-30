// Package jqrun runs crew's original crew.sh jq programs through gojq, the
// pure-Go jq library, instead of hand-translating each program into Go.
//
// The contract is value-equal JSON (#821), so gojq's alphabetical object keys
// are fine. Inputs and variables are in gojq's form: json.Number, float64,
// string, bool, nil, []any or map[string]any.
package jqrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
)

// Option extends the jq language one RunAny compiles against.
type Option struct{ compiler gojq.CompilerOption }

// WithJQFromJSON defines `_jqfromjson` as FromJSON. Invalid, empty or
// non-string input is an error, so `_jqfromjson?` skips the line.
func WithJQFromJSON() Option {
	return Option{gojq.WithFunction("_jqfromjson", 0, 0, func(v any, _ []any) any {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%v cannot be parsed as JSON", v)
		}
		out, err := FromJSON(s)
		if err != nil {
			return err
		}
		return out
	})}
}

// WithStringFunc defines the 0-arity jq function name as f applied to string
// input, for a transform Go owns rather than a jq regex. Non-string input is an
// error.
func WithStringFunc(name string, f func(string) string) Option {
	return Option{gojq.WithFunction(name, 0, 0, func(v any, _ []any) any {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s cannot be applied to %v", name, v)
		}
		return f(s)
	})}
}

// FromJSON decodes s as exactly one JSON text with json.Number literals,
// skipping a leading byte order mark as jq's fromjson does. A Go caller
// classifying text a fold also reads must call it: encoding/json's float64 path
// rejects bodies a fold parses.
func FromJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(s, "\xef\xbb\xbf")))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w (while parsing '%s')", err, s)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected data after the JSON text (while parsing '%s')", s)
	}
	return out, nil
}

// RunAny executes prog like `jq -s -c prog`: input is the elements of the
// slurped event array, vars become $name string/JSON variables, and the `now`
// builtin is frozen to nowSec so folds are deterministic (see freezeNow). The
// program must emit exactly one value; a gojq runtime error (jq's type error)
// is returned as an error, which main maps to the jq-failure exit status. The
// result is in Normalize's form.
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

// CompileAny compiles prog with vars bound and `now` frozen to now, as RunAny
// does, for running over many inputs.
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

// RunAny evaluates the program on input and returns its first output. No
// output is an error, as is a runtime error raised before the first output.
// The result is in Normalize's form.
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
