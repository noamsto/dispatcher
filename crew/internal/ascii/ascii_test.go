package ascii

import "testing"

func TestIsDigits(t *testing.T) {
	for in, want := range map[string]bool{"": false, "1a": false, "-1": false, " 1": false, "0x1": false,
		"1.0": false, "١": false, "0": true, "007": true, "1700000000000": true} {
		if got := IsDigits(in); got != want {
			t.Errorf("IsDigits(%q) = %v, want %v", in, got, want)
		}
	}
}
