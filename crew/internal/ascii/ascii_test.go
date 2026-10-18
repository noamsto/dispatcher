package ascii

import "testing"

func TestIsDigits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", false},
		{"1a", false},
		{"-1", false},
		{" 1", false},
		{"0x1", false},
		{"1.0", false},
		{"١", false},
		{"0", true},
		{"007", true},
		{"1700000000000", true},
	} {
		if got := IsDigits(tc.in); got != tc.want {
			t.Errorf("IsDigits(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
