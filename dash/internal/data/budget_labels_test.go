package data

import (
	"encoding/json"
	"testing"
)

func TestBudgetReportAnthropicCredit(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want *AnthropicCredit
	}{
		"present": {
			`{"engines":{},"anthropic_credit":{"balance_usd":50,"spent_usd":12.5,"remaining_usd":37.5,"used_pct":25,"expired":true}}`,
			&AnthropicCredit{BalanceUSD: 50, SpentUSD: 12.5, RemainingUSD: 37.5, UsedPct: 25, Expired: true},
		},
		"absent": {`{"engines":{}}`, nil},
		"null":   {`{"engines":{},"anthropic_credit":null}`, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var r BudgetReport
			if err := json.Unmarshal([]byte(tc.raw), &r); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil && r.AnthropicCredit != nil:
				t.Fatalf("AnthropicCredit = %+v, want nil", r.AnthropicCredit)
			case tc.want != nil && (r.AnthropicCredit == nil || *r.AnthropicCredit != *tc.want):
				t.Fatalf("AnthropicCredit = %+v, want %+v", r.AnthropicCredit, tc.want)
			}
		})
	}
}

func TestAnthropicCreditText(t *testing.T) {
	if got := (&AnthropicCredit{RemainingUSD: 12.345}).LeftText(); got != "$12.35 left" {
		t.Errorf("LeftText = %q", got)
	}
	if got := (&AnthropicCredit{RemainingUSD: -3, Expired: true}).LeftText(); got != "$0.00 left expired" {
		t.Errorf("LeftText = %q", got)
	}
	if (&AnthropicCredit{RemainingUSD: 1}).Exhausted() || !(&AnthropicCredit{}).Exhausted() {
		t.Error("Exhausted: want false with credit left, true at zero")
	}
}
