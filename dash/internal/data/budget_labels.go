package data

import (
	"strconv"
	"strings"
)

// AnthropicCredit decodes the report's anthropic_credit object: pi's prepaid
// Anthropic credit, a budget separate from the OpenRouter month window.
type AnthropicCredit struct {
	BalanceUSD   float64 `json:"balance_usd"`
	SpentUSD     float64 `json:"spent_usd"`
	RemainingUSD float64 `json:"remaining_usd"`
	UsedPct      float64 `json:"used_pct"`
	ResetsAt     *int64  `json:"resets_at"`
	Expired      bool    `json:"expired"`
}

// Budget labels name the provider or billing source a budget draws on.
const (
	LabelAnthropicKey = "claude api-key"
	LabelGemini       = "google (gemini)"
)

// GeminiBudget decodes the report's gemini_budget object: the monthly dollar
// target for Google Gemini spend.
type GeminiBudget struct {
	TargetUSD    float64 `json:"target_usd"`
	SpentUSD     float64 `json:"spent_usd"`
	RemainingUSD float64 `json:"remaining_usd"`
	UsedPct      float64 `json:"used_pct"`
}

// Exhausted reports a budget with nothing left to spend.
func (g *GeminiBudget) Exhausted() bool {
	return g.RemainingUSD <= 0
}

// LeftText is the budget's spend and what remains.
func (g *GeminiBudget) LeftText() string {
	return CreditText(g.TargetUSD, g.SpentUSD, g.RemainingUSD)
}

// Detail is the budget's full-text line for the Budget views.
func (g *GeminiBudget) Detail() string {
	return LabelGemini + ": " + detail(g.TargetUSD, g.SpentUSD, g.RemainingUSD, g.UsedPct)
}

// Exhausted reports a credit that cannot pay for more work: expired or spent.
func (c *AnthropicCredit) Exhausted() bool {
	return c.Expired || c.RemainingUSD <= 0
}

// LeftText is the credit's spend and what remains, plus " expired" once the
// credit lapsed.
func (c *AnthropicCredit) LeftText() string {
	text := CreditText(c.BalanceUSD, c.SpentUSD, c.RemainingUSD)
	if c.Expired {
		text += " expired"
	}
	return text
}

// WindowName is the label of a window inside its engine's own section: claude
// windows are its subscription, pi's month window is its OpenRouter spend
// against the monthly target.
func WindowName(engine, key string) string {
	switch {
	case engine == "claude" && strings.HasPrefix(key, "7d_"):
		return "claude " + key
	case engine == "claude":
		return "claude subscription " + key
	case engine == "pi" && key == "month":
		return "openrouter"
	}
	return key
}

// WindowLabel names a window where no engine heading precedes it.
func WindowLabel(engine, key string) string {
	if name := WindowName(engine, key); name != key {
		return name
	}
	return engine + " " + key
}

// Detail is the credit's full-text line for the Budget views.
func (c *AnthropicCredit) Detail() string {
	text := LabelAnthropicKey + ": " + detail(c.BalanceUSD, c.SpentUSD, c.RemainingUSD, c.UsedPct)
	if c.Expired {
		text += " (expired)"
	}
	return text
}

func detail(budget, spent, remaining, usedPct float64) string {
	return "$" + dollars(budget) + " budget, $" + dollars(spent) + " spent, $" + dollars(max(remaining, 0)) +
		" left, " + strconv.FormatFloat(usedPct, 'f', -1, 64) + "% used"
}

// CreditText is "$spent/$budget · $left left" for a direct-billed budget.
func CreditText(budget, spent, remaining float64) string {
	return "$" + dollars(spent) + "/$" + dollars(budget) + " · $" + dollars(max(remaining, 0)) + " left"
}

func dollars(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}

func limit(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// LocalTarget is the operator's own monthly dollar target. A target whose
// source is the key limit is not one: KeyLimitText covers it.
func (e *EngineBudget) LocalTarget() *float64 {
	if e.TargetSource == nil || *e.TargetSource != "config" {
		return nil
	}
	return e.TargetUSD
}

// SpendText is the compact spend with whichever limits are set: the local
// monthly target and what is left of the provider key's own limit. Without
// spend it is "".
func (e *EngineBudget) SpendText() string {
	if e.SpendUSD == nil {
		return ""
	}
	text := "$" + dollars(*e.SpendUSD)
	if t := e.LocalTarget(); t != nil {
		text += "/$" + limit(*t)
	}
	if e.KeyLimitUSD == nil {
		return text
	}
	if e.KeyLimitRemainingUSD != nil {
		return text + " · key $" + limit(max(*e.KeyLimitRemainingUSD, 0)) + " left"
	}
	return text + " · key $" + limit(*e.KeyLimitUSD)
}

// KeyLimitText is the provider key's own limit and what is left of it, or ""
// when the key has none.
func (e *EngineBudget) KeyLimitText() string {
	if e.KeyLimitUSD == nil {
		return ""
	}
	text := "key limit $" + limit(*e.KeyLimitUSD)
	if e.KeyLimitRemainingUSD != nil {
		text += ", $" + dollars(max(*e.KeyLimitRemainingUSD, 0)) + " left"
	}
	return text
}
