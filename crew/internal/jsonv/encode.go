package jsonv

import (
	"io"
	"strings"
)

// Options selects jq's output style. Indent is the default pretty form (two
// spaces); Colors enables -C colouring with the given palette.
type Options struct {
	Indent bool
	Colors *Palette
}

// Palette holds the SGR escape sequence for each colour slot, in jq's JQ_COLORS
// order: null, false, true, numbers, strings, arrays, objects, object keys.
type Palette [8]string

const (
	colKey = 7
	reset  = "\x1b[0m"
)

// DefaultPalette is jq 1.8.2's.
func DefaultPalette() Palette {
	return Palette{
		"\x1b[0;90m", "\x1b[0;39m", "\x1b[0;39m", "\x1b[0;39m",
		"\x1b[0;32m", "\x1b[1;39m", "\x1b[1;39m", "\x1b[1;34m",
	}
}

// ParseJQColors reads a JQ_COLORS value as jq does: up to eight colon-separated
// fields of digits and semicolons override the palette in order, a trailing empty
// field is ignored and an inner empty one gives a bare ESC[m. Any other
// character rejects the whole value, which yields the defaults and false.
func ParseJQColors(s string) (Palette, bool) {
	p := DefaultPalette()
	for i := 0; i < len(p) && s != ""; i++ {
		var field string
		field, s, _ = strings.Cut(s, ":")
		if strings.Trim(field, "0123456789;") != "" {
			return DefaultPalette(), false
		}
		p[i] = "\x1b[" + field + "m"
	}
	return p, true
}

// Encode writes v as jq would print it, without a trailing newline.
func Encode(w io.Writer, v Value, opts Options) error {
	_, err := w.Write(Append(nil, v, opts))
	return err
}

// Append appends v's jq encoding to b.
func Append(b []byte, v Value, opts Options) []byte {
	e := encoder{b: b, opts: opts}
	e.value(v, 0)
	return e.b
}

type encoder struct {
	b    []byte
	opts Options
}

func (e *encoder) start(col int) {
	if e.opts.Colors != nil {
		e.b = append(e.b, e.opts.Colors[col]...)
	}
}

func (e *encoder) end() {
	if e.opts.Colors != nil {
		e.b = append(e.b, reset...)
	}
}

func (e *encoder) token(col int, text string) {
	e.start(col)
	e.b = append(e.b, text...)
	e.end()
}

func (e *encoder) newline(level int) {
	if !e.opts.Indent {
		return
	}
	e.b = append(e.b, '\n')
	for range level {
		e.b = append(e.b, ' ', ' ')
	}
}

func (e *encoder) value(v Value, level int) {
	col := int(v.kind)
	switch v.kind {
	case KindNull:
		e.token(col, "null")
	case KindFalse:
		e.token(col, "false")
	case KindTrue:
		e.token(col, "true")
	case KindNumber:
		if v.s != "" {
			e.token(col, v.s)
		} else {
			e.token(col, FormatComputed(v.n))
		}
	case KindString:
		e.quoted(col, v.s)
	case KindArray:
		e.array(v, level)
	case KindObject:
		e.object(v, level)
	}
}

func (e *encoder) array(v Value, level int) {
	col := int(KindArray)
	if len(v.a) == 0 {
		e.token(col, "[]")
		return
	}
	e.token(col, "[")
	for i, x := range v.a {
		if i > 0 {
			e.token(col, ",")
		}
		e.newline(level + 1)
		e.value(x, level+1)
	}
	e.newline(level)
	e.token(col, "]")
}

func (e *encoder) object(v Value, level int) {
	col := int(KindObject)
	if len(v.m) == 0 {
		e.token(col, "{}")
		return
	}
	e.token(col, "{")
	for i, m := range v.m {
		if i > 0 {
			e.token(col, ",")
		}
		e.newline(level + 1)
		e.quoted(colKey, m.Key)
		e.token(col, ":")
		if e.opts.Indent {
			e.b = append(e.b, ' ')
		}
		e.value(m.Val, level+1)
	}
	e.newline(level)
	e.token(col, "}")
}

func (e *encoder) quoted(col int, s string) {
	e.start(col)
	e.b = append(e.b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			e.b = append(e.b, '\\', '"')
		case '\\':
			e.b = append(e.b, '\\', '\\')
		case '\b':
			e.b = append(e.b, '\\', 'b')
		case '\f':
			e.b = append(e.b, '\\', 'f')
		case '\n':
			e.b = append(e.b, '\\', 'n')
		case '\r':
			e.b = append(e.b, '\\', 'r')
		case '\t':
			e.b = append(e.b, '\\', 't')
		default:
			if c < 0x20 || c == 0x7f {
				const hex = "0123456789abcdef"
				e.b = append(e.b, '\\', 'u', '0', '0', hex[c>>4], hex[c&15])
			} else {
				e.b = append(e.b, c)
			}
		}
	}
	e.b = append(e.b, '"')
	e.end()
}
