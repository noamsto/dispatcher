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
	if got := (&AnthropicCredit{RemainingUSD: 12.345}).LeftText(); got != "$0.00/$0.00 · $12.35 left" {
		t.Errorf("LeftText = %q", got)
	}
	if got := (&AnthropicCredit{RemainingUSD: -3, Expired: true}).LeftText(); got != "$0.00/$0.00 · $0.00 left expired" {
		t.Errorf("LeftText = %q", got)
	}
	if (&AnthropicCredit{RemainingUSD: 1}).Exhausted() || !(&AnthropicCredit{}).Exhausted() {
		t.Error("Exhausted: want false with credit left, true at zero")
	}
}

func TestBudgetLabels(t *testing.T) {
	for _, tc := range []struct{ engine, key, name, label string }{
		{"claude", "7d", "claude subscription 7d", "claude subscription 7d"},
		{"pi", "month", "openrouter", "openrouter"},
		{"claude", "7d_sonnet", "claude 7d_sonnet", "claude 7d_sonnet"},
		{"claude", "7d_opus", "claude 7d_opus", "claude 7d_opus"},
		{"codex", "5h", "5h", "codex 5h"},
	} {
		if got := WindowName(tc.engine, tc.key); got != tc.name {
			t.Errorf("WindowName(%s, %s) = %q, want %q", tc.engine, tc.key, got, tc.name)
		}
		if got := WindowLabel(tc.engine, tc.key); got != tc.label {
			t.Errorf("WindowLabel(%s, %s) = %q, want %q", tc.engine, tc.key, got, tc.label)
		}
	}
}

func TestSpendText(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	str := func(v string) *string { return &v }
	for name, tc := range map[string]struct {
		e    EngineBudget
		want string
	}{
		"no spend":         {EngineBudget{TargetUSD: f(30), TargetSource: str("config")}, ""},
		"local only":       {EngineBudget{SpendUSD: f(12), TargetUSD: f(50), TargetSource: str("config")}, "$12.00/$50"},
		"key target only":  {EngineBudget{SpendUSD: f(12), TargetUSD: f(100), TargetSource: str("key_limit"), KeyLimitUSD: f(100), KeyLimitRemainingUSD: f(88)}, "$12.00 · key $88 left"},
		"key without left": {EngineBudget{SpendUSD: f(12), KeyLimitUSD: f(100)}, "$12.00 · key $100"},
		"local and key":    {EngineBudget{SpendUSD: f(12), TargetUSD: f(50), TargetSource: str("config"), KeyLimitUSD: f(100), KeyLimitRemainingUSD: f(88)}, "$12.00/$50 · key $88 left"},
	} {
		if got := tc.e.SpendText(); got != tc.want {
			t.Errorf("%s: SpendText = %q, want %q", name, got, tc.want)
		}
	}
	e := EngineBudget{KeyLimitUSD: f(100), KeyLimitRemainingUSD: f(88)}
	if got := e.KeyLimitText(); got != "key limit $100, $88.00 left" {
		t.Errorf("KeyLimitText = %q", got)
	}
	if got := (&EngineBudget{}).KeyLimitText(); got != "" {
		t.Errorf("KeyLimitText without limit = %q", got)
	}
}

func TestCreditTexts(t *testing.T) {
	c := &AnthropicCredit{BalanceUSD: 50, SpentUSD: 30, RemainingUSD: 20, UsedPct: 60}
	if got := c.LeftText(); got != "$30.00/$50.00 · $20.00 left" {
		t.Errorf("LeftText = %q", got)
	}
	if got, want := c.Detail(), "claude api-key: $50.00 budget, $30.00 spent, $20.00 left, 60% used"; got != want {
		t.Errorf("Detail = %q, want %q", got, want)
	}
	g := &GeminiBudget{TargetUSD: 40, SpentUSD: 10, RemainingUSD: 30, UsedPct: 25}
	if got, want := g.Detail(), "google (gemini): $40.00 budget, $10.00 spent, $30.00 left, 25% used"; got != want {
		t.Errorf("Detail = %q, want %q", got, want)
	}
	if g.Exhausted() || !(&GeminiBudget{}).Exhausted() {
		t.Error("GeminiBudget.Exhausted wrong")
	}
}

func TestBudgetReportGeminiBudget(t *testing.T) {
	var r BudgetReport
	in := `{"engines":{},"gemini_budget":{"target_usd":40,"spent_usd":10,"remaining_usd":30,"used_pct":25,"summary":"x"}}`
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	if want := (GeminiBudget{TargetUSD: 40, SpentUSD: 10, RemainingUSD: 30, UsedPct: 25}); r.GeminiBudget == nil || *r.GeminiBudget != want {
		t.Fatalf("GeminiBudget = %+v, want %+v", r.GeminiBudget, want)
	}
}
