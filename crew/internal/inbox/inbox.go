// Package inbox is `crew inbox`: every msg of one crew addressed to one agent,
// as `jq -c` lines, plus the delivered marks (#290) reading them raises so a
// later `await` does not hand them back.
//
// The arm ran `jq -c 'select(...)'` inside `$(...)`, so this port prints the
// rows it keeps through jsonv (the line is the source row, key order and
// number literals included) and never starts a jq. Only the marks merge is jq,
// and it lives in internal/marks beside the helpers still writing the same
// file.
package inbox

import (
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

// exitType is jq's status for a bus it could not parse, and for a row it could
// not index.
const exitType = 5

// Options is everything Run reads beyond the bus: whether $JQ_COLORS was
// invalid (jq warns once at startup even though its stdout here is a pipe, so
// it never colourises — the arm has no colour and neither does this), and how
// to resolve the crew the arm defaults to (`_crew_id`).
type Options struct {
	JQColorsInvalid bool
	CrewID          func() string
}

// sessionRe is _is_session_id's `^s[0-9]+-[0-9]+$`. \A..\z rather than ^..$:
// Go's `$` is end-of-text, which is what bash's ERE means here too (no
// REG_NEWLINE), and it keeps a trailing newline from looking like a session.
var sessionRe = regexp.MustCompile(`\As[0-9]+-[0-9]+\z`)

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm: the agent, then `[crew] [--since TS]` in either order, the
// crew defaulting to the caller's, a log that is not a regular file exiting 0
// before `--since` is even validated, and the msgs printed before the marks
// are raised.
func Run(args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	me, crew, since, msg := parse(args)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return 1
	}
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

	opts := jsonv.Options{}
	// `.crew_id` is jq's index: an object either carries the msg or does not,
	// null indexes to null, and any other value is a type error — one stderr
	// line each, and jq's exit status is the *last* input's outcome rather than
	// a sticky flag (the rule internal/log documents).
	failed := false
	var printed []jsonv.Value
	for _, ev := range events {
		v := ev.Raw
		switch v.Kind() {
		case jsonv.KindObject:
			failed = false
			if !selects(v, crew, me, since) {
				continue
			}
			printed = append(printed, v)
			_ = jsonv.Encode(stdout, v, opts)
			say(stdout, "\n")
		case jsonv.KindNull:
			failed = false
		case jsonv.KindFalse, jsonv.KindTrue, jsonv.KindNumber, jsonv.KindString, jsonv.KindArray:
			say(stderr, "crew: inbox: %s: cannot index %v with \"crew_id\"\n", paths.Log, v.Kind())
			failed = true
		}
	}
	// A session that has read its inbox has been handed these msgs, so a later
	// await must not return them again (#290) — including the prefix of a bus
	// whose tail then failed to parse, which is all the arm recorded too.
	if strings.HasPrefix(me, "worker:") && len(printed) > 0 {
		marks.Record(paths.Dir, crew, me, printed)
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

// parse reads the arm's arguments: `<agent> [crew] [--since TS]`. crew is the
// last bare word and --since the last --since, as the arm's while loop left
// them; msg is the arm's stderr line when the arguments themselves fail.
func parse(args []string) (me, crew, since, msg string) {
	if len(args) > 0 {
		me = args[0]
		args = args[1:]
	}
	// Only a worker id promises a session: a branch-only one matches no message
	// the caller could be waiting for, and #290's marks are per session.
	if strings.HasPrefix(me, "worker:") && !sessionRe.MatchString(me[strings.LastIndex(me, "#")+1:]) {
		return "", "", "", fmt.Sprintf(
			"crew: inbox: '%s' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message", me)
	}
	for len(args) > 0 {
		if args[0] == "--since" {
			if len(args) < 2 || args[1] == "" {
				return "", "", "", "crew: --since needs a value"
			}
			since, args = args[1], args[2:]
			continue
		}
		crew, args = args[0], args[1:]
	}
	return me, crew, since, ""
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
// member tests, and `.ts > $since` only runs with --since. Every test is jq's
// `==`, so a non-string value matches nothing — the empty crew of a caller
// with no id to default to included (#874).
func selects(v jsonv.Value, crew, me, since string) bool {
	if !member(v, "crew_id", crew) || !member(v, "kind", "msg") {
		return false
	}
	to, _ := v.Get("to")
	s, ok := to.AsString()
	if !ok || (s != me && s != "*") {
		return false
	}
	if since == "" {
		return true
	}
	ts, ok := v.Get("ts")
	if !ok {
		ts = jsonv.Null()
	}
	return greaterThanNumber(ts, since)
}

// member is jq's `.key == s`: an absent key indexes to null, and only a string
// equals the argument.
func member(v jsonv.Value, key, s string) bool {
	x, ok := v.Get(key)
	if !ok {
		return false
	}
	t, isStr := x.AsString()
	return isStr && t == s
}

// greaterThanNumber is jq's `.v > $since` for a $since that is a digit string,
// i.e. a non-negative number: jsonv's Kind constants are jq's sort order, so
// anything above KindNumber beats every number and anything below it loses.
func greaterThanNumber(v jsonv.Value, since string) bool {
	switch v.Kind() {
	case jsonv.KindString, jsonv.KindArray, jsonv.KindObject:
		return true
	case jsonv.KindNull, jsonv.KindFalse, jsonv.KindTrue:
		return false
	case jsonv.KindNumber:
		if text := v.NumberText(); text != "" {
			return decimalGreater(text, since)
		}
		// NaN and the infinities carry no literal text. jq parses an Infinity
		// input as a clamped double and NaN as equal to nothing; a double
		// compare answers both the same way, and a --since too big for one
		// parses to +Inf, which is where jq's clamp lands too.
		f, _ := v.AsFloat()
		return f > parseSince(since)
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
// non-negative integer literal b, on the text rather than through a double:
// jq compares literals exactly, so 18446744073709551617 beats
// 18446744073709551616, a 40-digit fraction beats 1, and 1e999999999 beats 5.
// It is one numeric comparison, not the total-order comparator #861 deleted,
// and it never expands an exponent: the work is the length of the two texts.
func decimalGreater(a, b string) bool {
	neg, digits, mag := decimal(a)
	b = strings.TrimLeft(b, "0")
	// Zero loses to every non-negative b, and so does a negative literal. A
	// text decimal cannot read is treated as zero: jsonv only hands back
	// literals it parsed itself, so nothing reaches here that is not one.
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

// expCap saturates an exponent's accumulation. jsonv only hands back literals
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
