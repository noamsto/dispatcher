package view

import "testing"

func TestLedgerCount(t *testing.T) {
	cases := []struct {
		detail      string
		done, total int
		ok          bool
	}{
		{"AC1 pass(go test); AC2-4 pass(bats); AC5 waived(dispatcher: n/a)", 5, 5, true},
		{"AC1 pass(x); AC2 fail(y)", 1, 2, true},
		{"AC1-3 pass(x)", 3, 3, true},
		{"tests green", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		done, total, ok := ledgerCount(c.detail)
		if done != c.done || total != c.total || ok != c.ok {
			t.Errorf("ledgerCount(%q) = %d/%d %v", c.detail, done, total, ok)
		}
	}
}
