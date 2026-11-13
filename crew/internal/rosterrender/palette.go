package rosterrender

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
)

// The palette (catppuccin mocha): a card's border colour is the worker's state.
const (
	colBG      = "#1e1e2e"
	colSun     = "#f9e2af"
	colWorking = "#a6e3a1"
	colBlocked = "#f38ba8"
	colPROpen  = "#89b4fa"
	colDone    = "#45475a"
	colFailed  = "#7f4a5a"
	colHolds   = "#fab387"
	colFill    = "#313244"
	colEdge    = "#585b70"
	colText    = "#cdd6f4"
	colDim     = "#6c7086"
	colLegend  = "#a6adc8"
	colWaiting = "#cba6f7"
)

// planetPenwidth is the standard tier's border width and a queued card's;
// cardPen picks a worker card's.
const planetPenwidth = 3.0

// diagramFont is aeye's default render font (AEYE_D2_FONT). The svg names it on
// its root and aeye's resvg draws it from its font directory, which holds
// nothing else.
const diagramFont = "Source Sans 3"

// flowDash is the resting dash of a working card's flow edge; aeye's animation
// moves it.
const flowDash = ` stroke-dasharray="0.01 12" stroke-linecap="round"`

// maxAnimated caps each animated class. Motion should mark what is live and
// recent; past about eight elements it turns into noise and costs per-frame
// render time. The column headers carry the totals.
const maxAnimated = 8

// planetKey is a planet's identity: a branch name can recur in another repo.
type planetKey struct {
	repo, branch string
}

// stripText is the header's budget strip: each engine's most-used quota window
// that has a reading, then this crew's live weight against the fleet budget when
// the budget is set. Unknown readings are left out, never drawn as 0.
func stripText(lg legend) string {
	var parts []string
	for _, e := range lg.engines {
		if e.pct != nil {
			parts = append(parts, strings.Join(slices.DeleteFunc([]string{clean(e.name), clean(e.window), strconv.FormatFloat(*e.pct, 'f', 0, 64) + "%"}, func(p string) bool { return p == "" }), " "))
		}
	}
	if lg.budget != nil && *lg.budget > 0 {
		parts = append(parts, "crew "+strconv.Itoa(lg.weight)+"/"+strconv.Itoa(*lg.budget))
	}
	return strings.Join(parts, " · ")
}

var (
	issuePrefixRe = regexp.MustCompile(`^([0-9]+)(?:-|$)`)
	linearIDRe    = regexp.MustCompile(`^[A-Za-z]+-[0-9]+$`)
)

// branchIssue is the leading digit run of the segment after a branch's first
// `/`, when `-` or the end follows it: `feat/12-x` is 12, `feat/grok-4-7-x` none.
func branchIssue(branch string) string {
	if _, rest, ok := strings.Cut(branch, "/"); ok {
		branch = rest
	}
	m := issuePrefixRe.FindStringSubmatch(branch)
	if m == nil {
		return ""
	}
	return m[1]
}

// linearTarget reports whether a is a Linear id and branch is the one
// `dispatch <id>` names: the lowercased id, or that and `-<slug>`.
func linearTarget(a, branch string) bool {
	if !linearIDRe.MatchString(a) {
		return false
	}
	id, b := strings.ToLower(a), strings.ToLower(branch)
	return b == id || strings.HasPrefix(b, id+"-")
}

// prNumber is the digits a pull request url ends in, or "".
func prNumber(url string) string {
	_, n, ok := strings.Cut(url, "/pull/")
	if !ok || !ascii.IsDigits(n) {
		return ""
	}
	return n
}
