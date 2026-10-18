// Package inbox is `crew inbox`: every msg of one crew addressed to one agent,
// as `jq -c` lines, plus the delivered marks (#290) reading them raises so a
// later `await` does not hand them back.
//
// The arm ran `jq -c 'select(...)'` inside `$(...)`, so this port prints the
// rows it keeps through jsonval (value-equal to the source row, number
// literals included) and never starts a jq. Only the marks merge is jq,
// and it lives in internal/marks beside the helpers still writing the same
// file.
package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

// exitType is jq's status for a bus it could not parse, and for a row it could
// not index.
const exitType = 5

// Options is everything Run reads beyond the bus: whether $JQ_COLORS was
// invalid (jq warns once at startup even with its stdout on a pipe, and the arm
// had no colour to lose), how to resolve the crew the arm defaults to
// (`_crew_id`), and how to put the buffered stdout on the wire.
type Options struct {
	JQColorsInvalid bool
	CrewID          func() string
	Flush           func() error
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Request is the arm's validated arguments: the agent, then `[crew] [--since TS]
// [--from SENDER] [--undelivered]`. An empty Crew takes the caller's.
type Request struct {
	Me, Crew, Since, From string
	Undelivered           bool
}

// Run is the arm: the crew defaulting to the caller's, a log that
// is not a regular file exiting 0 before `--since` is even validated, and the
// msgs printed before the marks are raised.
//
// --undelivered keeps only the msgs past this session's marks and holds the
// marks lock from reading them to recording them, so concurrent --undelivered
// readers of one session split a msg between them at most once. `crew await`
// reads the marks once before its poll loop by design, so an await already
// polling can still return a msg an --undelivered reader printed meanwhile.
func Run(q Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	me, crew, since := q.Me, q.Crew, q.Since
	if crew == "" {
		crew = o.CrewID()
	}
	events, err := bus.ReadEventsTolerant(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return 0
	}
	// The integer check sits where the arm put it: after the log test, before
	// jq starts, so a missing log exits 0 with neither line and a bad TS costs
	// no $JQ_COLORS warning.
	if since != "" && !isDigits(since) {
		say(stderr, "crew: --since must be an integer ms timestamp\n")
		return 1
	}
	if o.JQColorsInvalid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	// A torn or corrupt tail keeps the events parsed before the break: `jq -c`
	// had already printed them, and its parse error lands after the last line.
	var decode *bus.DecodeError
	if err != nil && !errors.As(err, &decode) {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: inbox: %s: %s\n", paths.Log, msg)
		return code
	}

	var held any
	if q.Undelivered {
		unlock, err := marks.Lock(paths.Dir, crew, me)
		if err != nil {
			say(stderr, "crew: inbox: cannot lock delivered marks: %v\n", err)
			return 1
		}
		defer unlock()
		held = marks.Read(paths.Dir, crew, me)
	}

	// `.crew_id` is jq's index: an object either carries the msg or does not,
	// null indexes to null, and any other value is a type error — one stderr
	// line each, and jq's exit status is the *last* input's outcome rather than
	// a sticky flag (the rule internal/log documents).
	failed := false
	var selected []map[string]any
	for _, ev := range events {
		switch v := ev.Doc.(type) {
		case map[string]any:
			failed = false
			if !selects(v, crew, me, since, q.From) {
				continue
			}
			selected = append(selected, v)
		case nil:
			failed = false
		default:
			say(stderr, "crew: inbox: %s: cannot index %s with \"crew_id\"\n", paths.Log, typeName(v))
			failed = true
		}
	}
	printed := selected
	if q.Undelivered {
		var err error
		if printed, err = pastMarks(selected, held); err != nil {
			say(stderr, "crew: inbox: %v\n", err)
			return 1
		}
	}
	for _, v := range printed {
		_ = jsonval.Encode(stdout, v, false)
	}
	// A session that has read its inbox has been handed these msgs, so a later
	// await must not return them again (#290) — including the prefix of a bus
	// whose tail then failed to parse, which is all the arm recorded too.
	//
	// The rows have to be on the wire first: the arm's `printf` under `set -e`
	// died before `_await_record` when the write failed, and a mark raised for
	// rows nobody received makes an await skip them for good. main.go's flush
	// reports the failure, as the arm's printf error line did.
	if o.Flush != nil {
		if err := o.Flush(); err != nil {
			return 1
		}
	}
	if strings.HasPrefix(me, "worker:") && len(printed) > 0 {
		recorded := make([]any, len(printed))
		for i, v := range printed {
			recorded[i] = v
		}
		if q.Undelivered {
			marks.RecordHeld(paths.Dir, crew, me, recorded)
		} else {
			marks.Record(paths.Dir, crew, me, recorded)
		}
	}
	if decode != nil {
		say(stderr, "crew: inbox: %s: %v\n", paths.Log, decode.Err)
		return exitType
	}
	if failed {
		return exitType
	}
	return 0
}

// isDigits is what the arm's `--since` case test accepts: a non-empty run of
// digits, which is all jq's `--argjson` needs for an integer ms timestamp.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// selects is the arm's select: the crew, the kind and the addressee are
// member tests, `.from == $from` only runs with --from, and `.ts > $since` only
// runs with --since. Every test is jq's `==`, so a non-string value matches
// nothing — the empty crew of a caller with no id to default to included (#874).
func selects(v map[string]any, crew, me, since, from string) bool {
	if !member(v, "crew_id", crew) || !member(v, "kind", "msg") {
		return false
	}
	if from != "" && !member(v, "from", from) {
		return false
	}
	if to, ok := v["to"].(string); !ok || (to != me && to != "*") {
		return false
	}
	if since == "" {
		return true
	}
	return greaterThanNumber(v["ts"], since)
}

// pastMarks keeps the msgs newer than this session's mark for their sender:
// `.ts > ($marks[.from] // 0)`, run by jq so a string mark a hand-edited file
// left outranks a number ts as `_unread_scan` has it. A msg whose `from` is not
// a string is dropped first — it has no mark to compare with, and record.jq
// would fail the whole write over it.
func pastMarks(rows []map[string]any, held any) ([]map[string]any, error) {
	var named []map[string]any
	input := []any{}
	for _, v := range rows {
		if _, ok := v["from"].(string); ok {
			named = append(named, v)
			input = append(input, v)
		}
	}
	if len(named) == 0 {
		return nil, nil
	}
	p, err := jqrun.CompileAny("map(.ts > ($marks[.from] // 0))", 0, map[string]any{"marks": held})
	if err != nil {
		return nil, err
	}
	keep, err := p.RunAny(input)
	if err != nil {
		return nil, err
	}
	flags, _ := keep.([]any)
	var out []map[string]any
	for i, k := range flags {
		if k == true {
			out = append(out, named[i])
		}
	}
	return out, nil
}

// member is jq's `.key == s`: an absent key indexes to null, and only a string
// equals the argument.
func member(v map[string]any, key, s string) bool {
	t, ok := v[key].(string)
	return ok && t == s
}

// typeName is jq's name for a value `.crew_id` cannot index.
func typeName(v any) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	}
	return "array"
}

// greaterThanNumber is jq's `.v > $since` for a $since that is a digit string,
// i.e. a non-negative number: jq's sort order puts null and booleans below every
// number and strings, arrays and objects above it.
func greaterThanNumber(v any, since string) bool {
	switch x := v.(type) {
	case string, []any, map[string]any:
		return true
	case json.Number:
		return decimalGreater(x.String(), since)
	case float64:
		// gojq's form for NaN and the infinities, which carry no literal text.
		// NaN is equal to nothing; a --since too big for a double parses to
		// +Inf, which is where jq's clamp lands too.
		return x > parseSince(since)
	}
	return false
}

// parseSince is jq's `--argjson since <digits>`: the arm has already refused
// anything but digits, so the only way out of range is past a double, and jq
// reads that as infinite.
func parseSince(since string) float64 {
	f, err := strconv.ParseFloat(since, 64)
	if err != nil || math.IsInf(f, 1) {
		return math.Inf(1)
	}
	return f
}

// decimalGreater reports whether the number literal a is greater than the
// non-negative integer literal b, on the text rather than through a double: jq
// compares literals exactly, so 18446744073709551617 beats
// 18446744073709551616, a 40-digit fraction beats 1, and 1e999999999 beats 5.
// It never expands an exponent: the work is the length of the two texts.
func decimalGreater(a, b string) bool {
	neg, digits, mag := decimal(a)
	b = strings.TrimLeft(b, "0")
	// Zero loses to every non-negative b, and so does a negative literal. A text
	// decimal cannot read is zero.
	if neg || digits == "" {
		return false
	}
	if b == "" {
		return true // a is positive, b is zero
	}
	if mag != int64(len(b)) {
		return mag > int64(len(b))
	}
	for i := 0; i < len(digits) || i < len(b); i++ {
		x, y := byte('0'), byte('0')
		if i < len(digits) {
			x = digits[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// decimal splits a finite number literal into its sign, its significant digits
// (leading zeros stripped) and the power of ten their first digit sits at, so
// "-0.0012e3" is (true, "12", 1): 1.2, which loses to any b of two digits or
// more and beats a one-digit b only digit for digit.
func decimal(tok string) (neg bool, digits string, mag int64) {
	i := 0
	if i < len(tok) && (tok[i] == '-' || tok[i] == '+') {
		neg = tok[i] == '-'
		i++
	}
	var (
		sb    strings.Builder
		point int64 // digits before the decimal point
		frac  bool
	)
	for ; i < len(tok); i++ {
		switch c := tok[i]; {
		case c == '.':
			frac = true
		case c >= '0' && c <= '9':
			if !frac {
				point++
			}
			sb.WriteByte(c)
		case c == 'e' || c == 'E':
			return finish(sb.String(), point, exponent(tok[i+1:]), neg)
		default:
			return false, "", 0
		}
	}
	return finish(sb.String(), point, 0, neg)
}

// expCap saturates an exponent's accumulation. A literal the bus parsed stays
// within decNumber's Emax, so a real one never reaches it; it keeps a
// hand-edited `1e999999999999999999999` from overflowing an int64.
const expCap = 1 << 40

// exponent reads a decimal exponent with its sign, saturating past expCap.
func exponent(tok string) int64 {
	i := 0
	sign := int64(1)
	if i < len(tok) && (tok[i] == '-' || tok[i] == '+') {
		if tok[i] == '-' {
			sign = -1
		}
		i++
	}
	var e int64
	for ; i < len(tok) && tok[i] >= '0' && tok[i] <= '9'; i++ {
		if e < expCap {
			e = e*10 + int64(tok[i]-'0')
		}
	}
	return sign * e
}

// finish is decimal's tail: strip the leading zeros and report the digits with
// the power of ten their first digit sits at, which is all a compare against an
// integer needs.
func finish(raw string, point, exp int64, neg bool) (bool, string, int64) {
	trimmed := strings.TrimLeft(raw, "0")
	if trimmed == "" {
		return neg, "", 0
	}
	return neg, trimmed, point + exp - int64(len(raw)-len(trimmed))
}
