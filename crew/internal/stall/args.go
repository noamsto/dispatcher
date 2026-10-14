package stall

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/reap"
)

const (
	usageMsg    = "crew: stall-watch <worker-id|branch|role:branch:role> --pane <id> [--engine E] [--grace S] [--stall S] [--window S] [--interval S] [--idle S] [--dead S] [--max-life S] [--load S] [--release S] [--bg-wait S] [--launch S] [--unread S] [--runaway-hits N] [--runaway-tokens N] [--no-budget] [--budget-refresh S] [--no-nudge]"
	noCrewMsg   = "crew: CREW_ID unset and no WORKER_TASK.md crew_id"
	needPaneMsg = "crew: stall-watch needs --pane <id>"
)

// releaseGrace is --release's default.
const releaseGrace = reap.ReleaseGrace

// Request is one stall-watch invocation as the grammar bound it. A nil flag
// was not given; a given flag keeps its value verbatim, empty included.
type Request struct {
	Target                        string
	Pane, Engine, Grace, Interval *string
	Release, BudgetRefresh, Stall, Window, Idle, Dead, MaxLife,
	Load, BgWait, Launch, Unread, RunawayHits, RunawayTokens *string
	NoBudget, NoNudge bool
}

// usageError is a refusal: exit 1 with its text on stderr.
type usageError string

func (e usageError) Error() string { return string(e) }

// config is the parsed invocation, read-only once the watch starts.
type config struct {
	crew, fromID, me, branch, pane, engine, ownEpoch string
	roleMode, nudgeOn, budgetOn, noBudget            bool
	grace, interval                                  string // handed to SleepCtx verbatim
	stall, window, idle, dead, maxLife, loadWin, release, bgWait,
	launch, unread, runawayHits, runawayTokens, budgetRefresh, runStartMS int64
	sigPrompt, sigMeter, sigSessionLimit, sigCursorLimit, sigBgwait, sigRunaway bool
}

// numFlag is a flag that must hold a non-negative integer. --release and
// --budget-refresh lead: the arm validated those two, in that order. The rest
// it fed to arithmetic and crashed on; here they are refused the same way.
type numFlag struct {
	name  string
	src   func(*Request) *string
	dst   func(*config) *int64
	def   int64
	count bool
}

var numFlags = []numFlag{
	{"--release", func(r *Request) *string { return r.Release }, func(c *config) *int64 { return &c.release }, releaseGrace, false},
	{"--budget-refresh", func(r *Request) *string { return r.BudgetRefresh }, func(c *config) *int64 { return &c.budgetRefresh }, 900, false},
	{"--stall", func(r *Request) *string { return r.Stall }, func(c *config) *int64 { return &c.stall }, 300, false},
	{"--window", func(r *Request) *string { return r.Window }, func(c *config) *int64 { return &c.window }, 900, false},
	{"--idle", func(r *Request) *string { return r.Idle }, func(c *config) *int64 { return &c.idle }, 1800, false},
	{"--dead", func(r *Request) *string { return r.Dead }, func(c *config) *int64 { return &c.dead }, 1800, false},
	{"--max-life", func(r *Request) *string { return r.MaxLife }, func(c *config) *int64 { return &c.maxLife }, 43200, false},
	{"--load", func(r *Request) *string { return r.Load }, func(c *config) *int64 { return &c.loadWin }, 300, false},
	{"--bg-wait", func(r *Request) *string { return r.BgWait }, func(c *config) *int64 { return &c.bgWait }, 7200, false},
	{"--launch", func(r *Request) *string { return r.Launch }, func(c *config) *int64 { return &c.launch }, 150, false},
	{"--unread", func(r *Request) *string { return r.Unread }, func(c *config) *int64 { return &c.unread }, 600, false},
	{"--runaway-hits", func(r *Request) *string { return r.RunawayHits }, func(c *config) *int64 { return &c.runawayHits }, 3, true},
	{"--runaway-tokens", func(r *Request) *string { return r.RunawayTokens }, func(c *config) *int64 { return &c.runawayTokens }, 1500, true},
}

// parseArgs is the arm's prologue in its order: the id, the crew, then the
// checks. The crew is resolved before any flag is looked at, so a missing crew
// wins over a bad flag.
func parseArgs(req Request, crewID func() string) (config, error) {
	if req.Target == "" {
		return config{}, usageError(usageMsg)
	}
	var c config
	c.identify(req.Target)
	c.crew = crewID()
	if c.crew == "" {
		return config{}, usageError(noCrewMsg)
	}
	c.engine, c.grace, c.interval = "unknown", "45", "15"
	for _, f := range []struct {
		name string
		src  *string
		dst  *string
	}{{"--engine", req.Engine, &c.engine}, {"--grace", req.Grace, &c.grace}, {"--interval", req.Interval, &c.interval}} {
		if f.src == nil {
			continue
		}
		if *f.src == "" {
			return config{}, usageError(fmt.Sprintf("crew: stall-watch: %s needs a value", f.name))
		}
		*f.dst = *f.src
	}
	if req.Pane == nil || *req.Pane == "" {
		return config{}, usageError(needPaneMsg)
	}
	c.pane = *req.Pane
	c.noBudget, c.nudgeOn = req.NoBudget, !req.NoNudge
	for _, n := range numFlags {
		s := strconv.FormatInt(n.def, 10)
		if p := n.src(&req); p != nil {
			s = *p
		}
		v, ok := nonNegInt(s)
		if !ok {
			unit := " number of seconds"
			if n.count {
				unit = ""
			}
			return config{}, usageError(fmt.Sprintf("crew: stall-watch: %s must be a non-negative integer%s", n.name, unit))
		}
		*n.dst(&c) = v
	}
	c.signatures()
	return c, nil
}

// nonNegInt is the arm's `” | *[!0-9]*` refusal, plus int64 range.
func nonNegInt(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// identify is INV-W0: identity is branch-keyed and suffix-tolerant, because
// dispatch has shipped both `worker:<branch>#s<session>` and a bare
// `<branch>`. Reads stay branch-keyed (me); writes carry the invoking session
// id (fromID). Only a session suffix is stripped, so a branch containing '#'
// keys the same here and in _sessions. role:<branch>:<role> is prompt-only
// mode, keyed to the role's own bus id. ownEpoch is the session's epoch, the
// step-aside threshold.
func (c *config) identify(arg string) {
	if strings.HasPrefix(arg, "role:") {
		c.roleMode = true
		c.fromID, c.me = arg, arg
		c.branch = cutLast(strings.TrimPrefix(arg, "role:"), ":")
		return
	}
	id := "worker:" + strings.TrimPrefix(arg, "worker:")
	if bus.IsSessionID(id) {
		c.fromID = id
		c.branch = strings.TrimPrefix(cutLast(id, "#"), "worker:")
	} else {
		c.branch = strings.TrimPrefix(id, "worker:")
		c.fromID = "worker:" + c.branch
	}
	c.me = "worker:" + c.branch
	if c.fromID != c.me {
		epoch := c.fromID[strings.LastIndex(c.fromID, "#s")+len("#s"):]
		c.ownEpoch = cutLast(epoch, "-")
	}
}

// cutLast is bash's `${s%sep*}`: s up to its last sep, or s when there is none.
func cutLast(s, sep string) string {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

// signatures is the engine signature table. Enabling an engine is data, not
// logic: a row is added only once its frames are pinned as fixtures, because
// a guessed signature is a false-positive generator. In role mode the prompt
// detectors stay claude-only: no other engine's parked role pane was verified.
func (c *config) signatures() {
	switch c.engine {
	case "claude":
		c.sigPrompt, c.sigMeter, c.sigSessionLimit, c.sigBgwait, c.sigRunaway = true, true, true, true, true
	case "pi":
		c.sigRunaway = true
	case "codex":
		c.sigPrompt = true
	case "cursor":
		c.sigCursorLimit = true
	}
	if c.roleMode && c.engine != "claude" {
		c.sigPrompt, c.sigSessionLimit, c.sigCursorLimit = false, false, false
	}
}
