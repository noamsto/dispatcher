// Package invisible drops format characters that terminals render as nothing.
// It mirrors crew/internal/invisible, pinned to it by the shared
// testdata/cases.json fixture and crew's drift test (dash is a separate module
// and cannot import it). The policy is emoji-safe: joiners and variation
// selectors survive where they build an emoji and are dropped anywhere else.
package invisible

import "strings"

const (
	zwj    = 0x200d
	vs15   = 0xfe0e
	vs16   = 0xfe0f
	keycap = 0x20e3
)

// hiddenAlways lists the code points that are hidden in every context.
func hiddenAlways(r rune) bool {
	switch {
	case r == 0x00ad, r == 0x034f, r == 0x061c, r == 0x115f, r == 0x1160,
		r >= 0x17b4 && r <= 0x17b5,
		r >= 0x180b && r <= 0x180f,
		r == 0x200b, r == 0x200c,
		r >= 0x200e && r <= 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x2060 && r <= 0x206f,
		r == 0x3164,
		r >= 0xfe00 && r <= 0xfe0d,
		r == 0xfeff, r == 0xffa0,
		r >= 0xfff0 && r <= 0xfffb,
		r >= 0x1bca0 && r <= 0x1bca3,
		r >= 0x1d173 && r <= 0x1d17a,
		r >= 0xe0000 && r <= 0xe0fff:
		return true
	}
	return false
}

// emojiBase reports whether r can start or continue an emoji sequence.
func emojiBase(r rune) bool {
	switch {
	case r == 0x00a9, r == 0x00ae, r == 0x203c, r == 0x2049, r == 0x2122, r == 0x2139,
		r >= 0x2194 && r <= 0x21aa,
		r >= 0x231a && r <= 0x23ff,
		r == 0x24c2,
		r >= 0x25aa && r <= 0x25fe,
		r >= 0x2600 && r <= 0x27bf,
		r >= 0x2934 && r <= 0x2935,
		r >= 0x2b05 && r <= 0x2b55,
		r == 0x3030, r == 0x303d, r == 0x3297, r == 0x3299,
		r >= 0x1f000 && r <= 0x1faff:
		return true
	}
	return false
}

// selectorKept reports whether the variation selector at rs[i] belongs to an
// emoji presentation or a keycap.
func selectorKept(rs []rune, i int) bool {
	if i == 0 {
		return false
	}
	prev := rs[i-1]
	if emojiBase(prev) {
		return true
	}
	return (prev >= '0' && prev <= '9' || prev == '#' || prev == '*') &&
		i+1 < len(rs) && rs[i+1] == keycap
}

// hidden decides from the original neighbours, so one pass suffices.
func hidden(rs []rune, i int) bool {
	r := rs[i]
	switch {
	case hiddenAlways(r):
		return true
	case r == vs15, r == vs16:
		return !selectorKept(rs, i)
	case r == zwj:
		if i == 0 || i+1 == len(rs) || !emojiBase(rs[i+1]) {
			return true
		}
		prev := rs[i-1]
		return !emojiBase(prev) && (prev != vs16 || !selectorKept(rs, i-1))
	}
	return false
}

// Drop removes every hidden code point from s.
func Drop(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if !hidden(rs, i) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
