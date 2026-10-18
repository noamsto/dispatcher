// Package ascii holds byte-level predicates over ASCII text.
package ascii

// IsDigits reports whether s is a non-empty run of ASCII digits: the shell's
// `case "$s" in ''|*[!0-9]*)` test, inverted.
func IsDigits(s string) bool {
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
