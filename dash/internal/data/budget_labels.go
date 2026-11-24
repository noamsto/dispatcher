package data

import (
	"strconv"
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

// Exhausted reports a credit that cannot pay for more work: expired or spent.
func (c *AnthropicCredit) Exhausted() bool {
	return c.Expired || c.RemainingUSD <= 0
}

// LeftText is "$<remaining> left", plus " expired" once the credit lapsed.
func (c *AnthropicCredit) LeftText() string {
	text := "$" + dollars(max(c.RemainingUSD, 0)) + " left"
	if c.Expired {
		text += " expired"
	}
	return text
}

// WindowName is the label of a window inside its engine's own section. pi's
// month window is its OpenRouter spend against the monthly target, so it
// reads as that budget.
func WindowName(engine, key string) string {
	if engine == "pi" && key == "month" {
		return "pi openrouter"
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
	return "pi anthropic: " + c.LeftText() + " of $" + dollars(c.BalanceUSD) + ", " +
		strconv.FormatFloat(c.UsedPct, 'f', -1, 64) + "% used"
}

// SpendText is "$spent/$target" for an engine with both figures, else "".
func (e *EngineBudget) SpendText() string {
	if e.SpendUSD == nil || e.TargetUSD == nil {
		return ""
	}
	return "$" + dollars(*e.SpendUSD) + "/$" + strconv.FormatFloat(*e.TargetUSD, 'f', -1, 64)
}

func dollars(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}
