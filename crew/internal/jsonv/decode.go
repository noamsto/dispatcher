package jsonv

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxDepth is jq's parser stack limit. An open object costs two slots while
// one of its values is being parsed (the object and its pending key).
const maxDepth = 10000

// SyntaxError is a parse failure. Its text is not jq's wording.
type SyntaxError struct {
	Offset int
	Msg    string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("%s at offset %d", e.Msg, e.Offset) }

// DecodeStream parses every JSON value in r, as `jq -s` reads a file:
// values may be concatenated or span lines, and input with no values yields none.
func DecodeStream(r io.Reader) ([]Value, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	p := parser{data: bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))}
	var vs []Value
	for {
		p.skipSpace()
		if p.pos == len(p.data) {
			return vs, nil
		}
		v, err := p.value(0)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
}

type parser struct {
	data []byte
	pos  int
}

func (p *parser) fail(msg string) error { return &SyntaxError{Offset: p.pos, Msg: msg} }

func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

// next skips whitespace and returns the next byte without consuming it.
func (p *parser) next() (byte, error) {
	p.skipSpace()
	if p.pos == len(p.data) {
		return 0, p.fail("unfinished JSON term")
	}
	return p.data[p.pos], nil
}

// value parses one value; depth is the number of parser stack slots in use.
func (p *parser) value(depth int) (Value, error) {
	c, err := p.next()
	if err != nil {
		return Value{}, err
	}
	switch c {
	case '{':
		return p.object(depth)
	case '[':
		return p.array(depth)
	case '"':
		s, err := p.str()
		return Value{kind: KindString, s: s}, err
	}
	return p.literal()
}

func (p *parser) array(depth int) (Value, error) {
	if depth >= maxDepth {
		return Value{}, p.fail("exceeds depth limit for parsing")
	}
	p.pos++
	arr := Value{kind: KindArray}
	if c, err := p.next(); err != nil {
		return Value{}, err
	} else if c == ']' {
		p.pos++
		return arr, nil
	}
	for {
		v, err := p.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		arr.a = append(arr.a, v)
		c, err := p.next()
		if err != nil {
			return Value{}, err
		}
		switch c {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return arr, nil
		default:
			return Value{}, p.fail("expected separator between values")
		}
	}
}

func (p *parser) object(depth int) (Value, error) {
	if depth >= maxDepth {
		return Value{}, p.fail("exceeds depth limit for parsing")
	}
	p.pos++
	obj := Value{kind: KindObject}
	if c, err := p.next(); err != nil {
		return Value{}, err
	} else if c == '}' {
		p.pos++
		return obj, nil
	}
	for {
		if c, err := p.next(); err != nil {
			return Value{}, err
		} else if c != '"' {
			return Value{}, p.fail("object keys must be strings")
		}
		key, err := p.str()
		if err != nil {
			return Value{}, err
		}
		if c, err := p.next(); err != nil {
			return Value{}, err
		} else if c != ':' {
			return Value{}, p.fail("expected separator between values")
		}
		p.pos++
		v, err := p.value(depth + 2)
		if err != nil {
			return Value{}, err
		}
		obj.Set(key, v)
		c, err := p.next()
		if err != nil {
			return Value{}, err
		}
		switch c {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return obj, nil
		default:
			return Value{}, p.fail("expected separator between values")
		}
	}
}

// str parses a string starting at its opening quote. Escapes are decoded first
// and invalid UTF-8 repaired afterwards, so the repair sees jq's byte stream.
func (p *parser) str() (string, error) {
	p.pos++
	var buf []byte
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return validUTF8(buf), nil
		case c < 0x20:
			return "", p.fail("control characters from U+0000 through U+001F must be escaped")
		case c != '\\':
			buf = append(buf, c)
			p.pos++
			continue
		}
		var err error
		if buf, err = p.escape(buf); err != nil {
			return "", err
		}
	}
	return "", p.fail("unfinished string")
}

// escape decodes the escape sequence at p.pos (a backslash) onto buf.
func (p *parser) escape(buf []byte) ([]byte, error) {
	if p.pos+1 == len(p.data) {
		return nil, p.fail("unfinished string")
	}
	c := p.data[p.pos+1]
	p.pos += 2
	switch c {
	case '"', '\\', '/':
		return append(buf, c), nil
	case 'b':
		return append(buf, '\b'), nil
	case 'f':
		return append(buf, '\f'), nil
	case 'n':
		return append(buf, '\n'), nil
	case 'r':
		return append(buf, '\r'), nil
	case 't':
		return append(buf, '\t'), nil
	case 'u':
		r, ok := p.hex4()
		if !ok {
			return nil, p.fail("invalid \\uXXXX escape")
		}
		switch {
		case r >= 0xd800 && r < 0xdc00:
			lo, ok := p.lowSurrogate()
			if !ok {
				return nil, p.fail("invalid \\uXXXX\\uXXXX surrogate pair escape")
			}
			r = 0x10000 + (r-0xd800)<<10 + lo - 0xdc00
		case r >= 0xdc00 && r < 0xe000:
			r = utf8.RuneError
		}
		return utf8.AppendRune(buf, r), nil
	}
	p.pos--
	return nil, p.fail("invalid escape")
}

func (p *parser) hex4() (rune, bool) {
	if p.pos+4 > len(p.data) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	p.pos += 4
	return rune(n), true
}

// lowSurrogate consumes the \uDC00-\uDFFF escape that must follow a high surrogate.
func (p *parser) lowSurrogate() (rune, bool) {
	if p.pos+2 > len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
		return 0, false
	}
	p.pos += 2
	lo, ok := p.hex4()
	return lo, ok && lo >= 0xdc00 && lo < 0xe000
}

// literal parses a bare token: true, false, null, or a number. jq ends a token
// only at whitespace, a structural character or a quote.
func (p *parser) literal() (Value, error) {
	start := p.pos
	for p.pos < len(p.data) && !strings.ContainsRune(" \t\r\n[]{},:\"", rune(p.data[p.pos])) {
		p.pos++
	}
	tok := string(p.data[start:p.pos])
	if tok == "" {
		return Value{}, p.fail("expected a value")
	}
	switch tok {
	case "null":
		return Value{}, nil
	case "true":
		return Bool(true), nil
	case "false":
		return Bool(false), nil
	}
	if v, ok := parseNumber(tok); ok {
		return v, nil
	}
	if strings.ContainsRune("tfn", rune(tok[0])) {
		return Value{}, p.fail("invalid literal")
	}
	return Value{}, p.fail("invalid numeric literal")
}

// parseNumber reads a finite literal, keeping its canonical text, or one of
// decNumber's Infinity and NaN spellings, which jq turns into plain doubles.
func parseNumber(tok string) (Value, bool) {
	if d, ok := parseDecimal(tok); ok {
		if !d.isZero() && d.adjusted() > maxAdjusted {
			return infinity(d.neg), true
		}
		f, _ := strconv.ParseFloat(tok, 64)
		return Value{kind: KindNumber, n: f, s: d.String()}, true
	}
	neg := tok[0] == '-'
	name := tok
	if neg || tok[0] == '+' {
		name = tok[1:]
	}
	name = strings.ToLower(name)
	if name == "inf" || name == "infinity" {
		return infinity(neg), true
	}
	if payload, ok := strings.CutPrefix(strings.TrimPrefix(name, "s"), "nan"); ok && strings.Trim(payload, "0123456789") == "" {
		return Num(math.NaN()), true
	}
	return Value{}, false
}

func infinity(neg bool) Value {
	if neg {
		return Num(math.Inf(-1))
	}
	return Num(math.Inf(1))
}
