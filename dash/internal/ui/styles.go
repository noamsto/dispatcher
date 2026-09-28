package ui

import "github.com/charmbracelet/lipgloss"

// styles.go is the one place every lipgloss.Style used by the interactive
// dashboard lives, so a color or emphasis change never drifts between views.
// NO_COLOR is handled by ui.Run calling lipgloss.SetColorProfile(termenv.Ascii)
// once at startup — never in here, and never at package-init time, so tests
// can force whatever profile they need (see charm-tui skill).
var (
	tabActiveStyle   = lipgloss.NewStyle().Bold(true).Underline(true)
	tabInactiveStyle = lipgloss.NewStyle().Faint(true)
	tabSepStyle      = lipgloss.NewStyle().Faint(true)

	statusStyle = lipgloss.NewStyle().Faint(true)

	cursorStyle = lipgloss.NewStyle().Bold(true)

	branchStyle = lipgloss.NewStyle()
	leafStyle   = lipgloss.NewStyle()
	headerStyle = lipgloss.NewStyle().Bold(true)
	warnStyle   = lipgloss.NewStyle().Faint(true)

	// Badge styles: one distinct rendering per settings origin (spec
	// §Settings) — default faint, user cyan, env magenta, locked bold
	// yellow with a lock glyph the only emoji on screen.
	badgeDefaultStyle = lipgloss.NewStyle().Faint(true)
	badgeUserStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	badgeEnvStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	badgeLockedStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))

	detailStyle = lipgloss.NewStyle()

	staleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)

	gaugeNormalColor  = "#2ecc71"
	gaugeWarningColor = "#e6b800"
	gaugeDangerColor  = "#e63939"
)

// badgeText is the origin's label, exactly as the --once renderer's badgeOf
// (the lock glyph is the only emoji on screen).
func badgeText(origin string) string {
	switch origin {
	case "base":
		return "default"
	case "user":
		return "user"
	case "env":
		return "env"
	case "locked":
		return "🔒 locked"
	default:
		return origin
	}
}

// badgeStyle picks the origin's distinct style.
func badgeStyle(origin string) lipgloss.Style {
	switch origin {
	case "user":
		return badgeUserStyle
	case "env":
		return badgeEnvStyle
	case "locked":
		return badgeLockedStyle
	default:
		return badgeDefaultStyle
	}
}

// gaugeColor picks a progress-bar solid-fill color by the spec's 85/95
// thresholds (§Budget): <85 normal, 85-95 warning, >=95 danger.
func gaugeColor(usedPct float64) string {
	switch {
	case usedPct >= 95:
		return gaugeDangerColor
	case usedPct >= 85:
		return gaugeWarningColor
	default:
		return gaugeNormalColor
	}
}
