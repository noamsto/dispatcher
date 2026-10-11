package gitguard

import (
	"fmt"
	"strings"
)

// Quote renders s as bash 5.3's `printf %q` does under LC_ALL=C. The C locale
// is deliberate: every non-ASCII byte prints as an octal escape, so a config
// value cannot smuggle terminal control sequences into an operator's screen.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return ansiCQuote(s)
		}
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(" !\"$&'()*,;<>?[\\]^`{|}", c) >= 0 ||
			c == '#' && i == 0 ||
			c == '~' && (i == 0 || s[i-1] == '=' || s[i-1] == ':') {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

func ansiCQuote(s string) string {
	var b strings.Builder
	b.WriteString("$'")
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1b:
			b.WriteString(`\E`)
		case '\'', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('\'')
	return b.String()
}
