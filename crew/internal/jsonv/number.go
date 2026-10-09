package jsonv

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// expLimit keeps exponent arithmetic away from int overflow; it is far beyond
// maxAdjusted, so such literals are infinite either way.
const expLimit = 1 << 40

// maxAdjusted is decNumber's Emax: a literal whose adjusted exponent is larger
// overflows to infinity, which jq then prints as a double.
const maxAdjusted = 999999999

// decimal is a number literal as decNumber holds it: ±digits×10^exp, with
// leading zeros stripped from digits and trailing zeros kept.
type decimal struct {
	neg    bool
	digits string
	exp    int
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// parseDecimal accepts decNumber's finite grammar, which is looser than JSON:
// a sign, leading zeros, a bare leading or trailing dot.
func parseDecimal(tok string) (decimal, bool) {
	var d decimal
	i := 0
	if i < len(tok) && (tok[i] == '-' || tok[i] == '+') {
		d.neg = tok[i] == '-'
		i++
	}
	start := i
	for i < len(tok) && isDigit(tok[i]) {
		i++
	}
	intPart := tok[start:i]
	frac := ""
	if i < len(tok) && tok[i] == '.' {
		i++
		fs := i
		for i < len(tok) && isDigit(tok[i]) {
			i++
		}
		frac = tok[fs:i]
	}
	if intPart == "" && frac == "" {
		return decimal{}, false
	}
	exp := 0
	if i < len(tok) && (tok[i] == 'e' || tok[i] == 'E') {
		e, err := strconv.Atoi(tok[i+1:])
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return decimal{}, false
		}
		exp = max(min(e, expLimit), -expLimit)
		i = len(tok)
	}
	if i != len(tok) {
		return decimal{}, false
	}
	d.digits = strings.TrimLeft(intPart+frac, "0")
	if d.digits == "" {
		d.digits = "0"
	}
	d.exp = exp - len(frac)
	return d, true
}

// String is decNumber's to-scientific-string, which is what jq prints for a
// literal it has not done arithmetic on.
func (d decimal) String() string {
	var sb strings.Builder
	if d.neg {
		sb.WriteByte('-')
	}
	n := len(d.digits)
	adjusted := d.adjusted()
	switch {
	case d.exp == 0:
		sb.WriteString(d.digits)
	case d.exp < 0 && adjusted >= -6:
		if n > -d.exp {
			sb.WriteString(d.digits[:n+d.exp])
			sb.WriteByte('.')
			sb.WriteString(d.digits[n+d.exp:])
		} else {
			sb.WriteString("0.")
			sb.WriteString(strings.Repeat("0", -d.exp-n))
			sb.WriteString(d.digits)
		}
	default:
		sb.WriteString(d.digits[:1])
		if n > 1 {
			sb.WriteByte('.')
			sb.WriteString(d.digits[1:])
		}
		sb.WriteByte('E')
		if adjusted >= 0 {
			sb.WriteByte('+')
		}
		sb.WriteString(strconv.Itoa(adjusted))
	}
	return sb.String()
}

func (d decimal) adjusted() int { return d.exp + len(d.digits) - 1 }

func (d decimal) isZero() bool { return d.digits == "0" }

// FormatComputed prints a double the way jq prints arithmetic results:
// shortest round-trip digits, exponential only when the decimal point falls
// outside [-3, ndigits+15].
func FormatComputed(x float64) string {
	switch {
	case math.IsNaN(x):
		return "null"
	case math.IsInf(x, 1):
		return "1.7976931348623157e+308"
	case math.IsInf(x, -1):
		return "-1.7976931348623157e+308"
	case x == 0:
		if math.Signbit(x) {
			return "-0"
		}
		return "0"
	}
	sci := strconv.FormatFloat(math.Abs(x), 'e', -1, 64)
	mant, exp, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, _ := strconv.Atoi(exp)
	decpt := e + 1
	var sb strings.Builder
	if x < 0 {
		sb.WriteByte('-')
	}
	switch {
	case decpt <= -4 || decpt > len(digits)+15:
		sb.WriteString(digits[:1])
		if len(digits) > 1 {
			sb.WriteByte('.')
			sb.WriteString(digits[1:])
		}
		sb.WriteByte('e')
		if e < 0 {
			sb.WriteByte('-')
			e = -e
		} else {
			sb.WriteByte('+')
		}
		if e < 10 {
			sb.WriteByte('0')
		}
		sb.WriteString(strconv.Itoa(e))
	case decpt <= 0:
		sb.WriteString("0.")
		sb.WriteString(strings.Repeat("0", -decpt))
		sb.WriteString(digits)
	case decpt >= len(digits):
		sb.WriteString(digits)
		sb.WriteString(strings.Repeat("0", decpt-len(digits)))
	default:
		sb.WriteString(digits[:decpt])
		sb.WriteByte('.')
		sb.WriteString(digits[decpt:])
	}
	return sb.String()
}
