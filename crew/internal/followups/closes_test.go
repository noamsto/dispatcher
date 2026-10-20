package followups

import "testing"

func TestClosesRef(t *testing.T) {
	tests := []struct {
		name, body string
		want       ref
	}{
		{"prose before closes line", "this fixes #812's regression\n\nCloses #900\n", ref{num: 900}},
		{"indented", "intro\n  Closes #7", ref{num: 7}},
		{"ticket", "Closes ABC-12\n", ref{ticket: "ABC-12"}},
		{"prose only", "this fixes #812's regression", ref{}},
		{"none", "no keyword here", ref{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := closesRef(tt.body); got != tt.want {
				t.Errorf("closesRef(%q) = %+v, want %+v", tt.body, got, tt.want)
			}
		})
	}
}
