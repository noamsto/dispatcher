package main

import (
	"fmt"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/nudge"
	"github.com/noamsto/dispatcher/crew/internal/rate"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
	"github.com/noamsto/dispatcher/crew/internal/retro"
	"github.com/noamsto/dispatcher/crew/internal/stack"
	"github.com/noamsto/dispatcher/crew/internal/where"
)

// The typed commands (#862). Kong binds the flags, so `--flag value` and
// `--flag=value` are one spelling. A string flag whose bare or empty use is a
// refusal is a *string: with hyphen-prefixed parameters on, `--crew` at the end
// of argv binds "" rather than failing, so nil is absent and "" is given empty.

type whereCmd struct {
	Crew   *string `name:"crew"`
	Target string  `arg:"" optional:"" name:"target"`
}

type identityCmd struct {
	Hash   *string `name:"hash"`
	Branch string  `arg:"" optional:"" name:"branch"`
	Crew   string  `arg:"" optional:"" name:"crew"`
}

type resolveCmd struct {
	Crew   *string `name:"crew"`
	Target string  `arg:"" optional:"" name:"target"`
}

type crewsCmd struct {
	Mine bool `name:"mine"`
}

type registerCmd struct {
	PID string `arg:"" optional:"" name:"pid"`
}

type deregisterCmd struct{}

type logCmd struct {
	Crew string `arg:"" optional:"" name:"crew"`
}

type sessionsCmd struct {
	Crew   *string `name:"crew"`
	Branch string  `arg:"" optional:"" name:"branch"`
}

type inboxCmd struct {
	Since       *string `name:"since"`
	From        *string `name:"from"`
	Undelivered bool    `name:"undelivered"`
	Agent       string  `arg:"" optional:"" name:"agent"`
	Crew        string  `arg:"" optional:"" name:"crew"`
}

type rosterCmd struct {
	Crew string `arg:"" optional:"" name:"crew"`
}

type reportCmd struct {
	Crew string `arg:"" optional:"" name:"crew"`
}

type retroCmd struct {
	Report bool `name:"report"`
	JSON   bool `name:"json"`
}

// rateCmd.Root keeps every value whole (`sep:"none"`, so a comma stays in the
// path) and binds an empty one, which validate refuses.
type rateCmd struct {
	Report   bool      `name:"report"`
	JSON     bool      `name:"json"`
	Pooled   bool      `name:"pooled"`
	SweepAll bool      `name:"sweep-all"`
	Root     []*string `name:"root" sep:"none"`
}

// fleetTableCmd is bare `crew fleet`: the hidden default command, so the table's
// flags sit on it rather than on fleet, where they would clash with the lane's
// own --crew. Its name is flag-shaped so no word selects it: `crew fleet table`
// stays the usage refusal it always was.
type fleetTableCmd struct {
	All  bool    `name:"all"`
	JSON bool    `name:"json"`
	Crew *string `name:"crew"`
}

type fleetCmd struct {
	Table  fleetTableCmd  `cmd:"" name:"-table" default:"withargs" hidden:""`
	Stream fleetStreamCmd `cmd:"" name:"stream"`
}

// streamFlags are the flags `crew stream` and `crew fleet stream` share.
type streamFlags struct {
	Crew      *string `name:"crew"`
	States    *string `name:"states"`
	Park      *string `name:"park"`
	Heartbeat *string `name:"heartbeat"`
	Coalesce  *string `name:"coalesce"`
	Retry     *string `name:"retry"`
	Interval  *string `name:"interval"`
	Force     bool    `name:"force"`
	Status    bool    `name:"status"`
}

type streamCmd struct {
	streamFlags `embed:""`
	ReapEvery   *string `name:"reap-every"`
}

// valueFlag is a *string flag under the name a refusal gives it.
type valueFlag struct {
	name string
	v    *string
}

// valueFlags are the shared value flags in the order their refusals are judged.
func (f streamFlags) valueFlags() []valueFlag {
	return []valueFlag{
		{"--crew", f.Crew}, {"--states", f.States}, {"--park", f.Park}, {"--heartbeat", f.Heartbeat},
		{"--coalesce", f.Coalesce}, {"--retry", f.Retry}, {"--interval", f.Interval},
	}
}

// fleetStreamCmd.ReapEvery is bound only to be refused with the lane's reason.
type fleetStreamCmd struct {
	streamFlags `embed:""`
	Rediscover  *string `name:"rediscover"`
	ReapEvery   *string `name:"reap-every" hidden:""`
}

type stackCutCmd struct {
	Record bool   `name:"record"`
	Child  string `arg:"" optional:"" name:"child"`
	Oid    string `arg:"" optional:"" name:"oid"`
}

type stackAlignCmd struct {
	Top string `arg:"" optional:"" name:"top"`
}

type stackCmd struct {
	Cut   stackCutCmd   `cmd:"" name:"cut"`
	Align stackAlignCmd `cmd:"" name:"align"`
}

// rosterRenderCmd keeps interval and quiet as the caller's bytes: they are what
// the detached daemon is started with.
type rosterRenderCmd struct {
	Crew     *string `name:"crew"`
	Pane     *string `name:"pane"`
	Interval *string `name:"interval"`
	Quiet    *string `name:"quiet"`
	NoOpen   bool    `name:"no-open"`
	Once     bool    `name:"once"`
	Detach   bool    `name:"detach"`
}

type statusCmd struct {
	Restamp bool   `name:"restamp"`
	From    string `arg:"" optional:"" name:"from"`
	State   string `arg:"" optional:"" name:"state"`
	Detail  string `arg:"" optional:"" name:"detail"`
	PR      string `arg:"" optional:"" name:"pr"`
}

type replyCmd struct {
	Crew *string `name:"crew"`
	To   string  `arg:"" optional:"" name:"to"`
	Body string  `arg:"" optional:"" name:"body"`
}

type holdReadCmd struct {
	JSON bool    `name:"json"`
	Crew *string `name:"crew"`
}

type holdParkCmd struct {
	Crew    *string `name:"crew"`
	Default string  `arg:"" optional:"" name:"default"`
}

type holdReleaseCmd struct {
	Crew *string `name:"crew"`
	ID   string  `arg:"" optional:"" name:"id"`
}

// holdAddCmd.Title is the words kong leaves alone once it meets one that is no
// flag: a later `--draft` is part of the title.
type holdAddCmd struct {
	Engine   *string  `name:"engine"`
	Window   *string  `name:"window"`
	ResetsAt *string  `name:"resets-at"`
	Agent    *string  `name:"agent"`
	Ref      *string  `name:"ref"`
	Branch   *string  `name:"branch"`
	Tier     *string  `name:"tier"`
	Model    *string  `name:"model"`
	Effort   *string  `name:"effort"`
	Plan     *string  `name:"plan"`
	MCP      *string  `name:"mcp"`
	Shape    *string  `name:"shape"`
	Spec     *string  `name:"spec"`
	Crew     *string  `name:"crew"`
	Draft    bool     `name:"draft"`
	Title    []string `arg:"" optional:"" passthrough:"partial" name:"title"`
}

type holdCmd struct {
	Add     holdAddCmd     `cmd:"" name:"add"`
	List    holdReadCmd    `cmd:"" name:"list"`
	Due     holdReadCmd    `cmd:"" name:"due"`
	Park    holdParkCmd    `cmd:"" name:"park"`
	Release holdReleaseCmd `cmd:"" name:"release"`
}

type nudgeCmd struct {
	Crew   *string `name:"crew"`
	Wait   *string `name:"wait"`
	Target string  `arg:"" optional:"" name:"target"`
}

type awaitCmd struct {
	From     *string `name:"from"`
	Timeout  *string `name:"timeout"`
	Interval *string `name:"interval"`
	Agent    string  `arg:"" optional:"" name:"agent"`
}

type watchCmd struct {
	Since    *string `name:"since"`
	States   *string `name:"states"`
	Timeout  *string `name:"timeout"`
	Interval *string `name:"interval"`
	Crew     *string `name:"crew"`
}

// refusal is the stderr line and exit status a typed subcommand's argv earned.
type refusal struct {
	msg  string
	code int
}

const (
	identityUsage     = "crew: identity <branch> [crew]"
	identityHashUsage = "crew: identity --hash <name>"
	identitySignature = "usage: crew identity <branch> [crew]"
	rateRootUsage     = "crew: rate --root needs a directory"
)

// wording is how one subcommand phrases the two grammar failures kong reports
// by prefix: the offending token is handed in bare. A subcommand with one usage
// line for every grammar failure sets all instead.
type wording struct {
	unknownFlag func(flag string) string
	extra       func(word string) string
	all         string
	code        int
}

// plain builds the `crew: <sub>: …` phrasing the arms without a custom one share.
func plain(sub string) wording {
	return wording{
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: %s: unknown flag '%s'", sub, f) },
		extra:       func(w string) string { return fmt.Sprintf("crew: %s: unexpected argument '%s'", sub, w) },
		code:        exitFailure,
	}
}

var wordings = map[string]wording{
	"where": {
		unknownFlag: func(f string) string {
			return fmt.Sprintf("crew: where: unknown flag '%s' (%s)", f, where.Usage)
		},
		extra: func(string) string { return "crew: where: one target only (" + where.Usage + ")" },
		code:  exitFailure,
	},
	"resolve-target": {
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: resolve-target: unknown flag '%s'", f) },
		extra:       func(string) string { return "crew: resolve-target: one target only" },
		code:        exitFailure,
	},
	"identity": {
		unknownFlag: func(f string) string {
			return fmt.Sprintf("crew: identity: unknown flag '%s' (%s)", f, identitySignature)
		},
		extra: func(string) string { return identityUsage },
		code:  exitFailure,
	},
	"sessions": {
		unknownFlag: func(string) string { return sessionsUsage },
		extra:       func(string) string { return sessionsUsage },
		code:        exitFailure,
	},
	"crews": {
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: crews: unknown arg '%s'", f) },
		extra:       func(w string) string { return fmt.Sprintf("crew: crews: unknown arg '%s'", w) },
		code:        exitUsage,
	},
	"status": {
		unknownFlag: func(f string) string {
			return fmt.Sprintf("crew: status: unknown arg '%s' (use -- before a detail that starts with dashes)", f)
		},
		extra: func(w string) string { return fmt.Sprintf("crew: status: unexpected argument '%s'", w) },
		code:  exitFailure,
	},
	"reply": plain("reply"),
	"hold":  {all: hold.Usage, code: exitFailure},
	"hold add": {
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: hold add: unknown arg '%s'", f) },
		extra:       func(w string) string { return fmt.Sprintf("crew: hold add: unknown arg '%s'", w) },
		code:        exitFailure,
	},
	"hold list":    {all: hold.ListUsage, code: exitFailure},
	"hold due":     {all: hold.DueUsage, code: exitFailure},
	"hold park":    {all: hold.ParkUsage, code: exitFailure},
	"hold release": {all: hold.ReleaseUsage, code: exitFailure},
	"nudge": {
		unknownFlag: func(f string) string {
			return fmt.Sprintf("crew: nudge: unknown flag '%s' (usage: %s)", f, nudge.Usage)
		},
		extra: func(string) string { return "crew: nudge: one target only (usage: " + nudge.Usage + ")" },
		code:  exitFailure,
	},
	"await": {
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: await: unknown arg '%s'", f) },
		extra:       func(w string) string { return fmt.Sprintf("crew: await: unknown arg '%s'", w) },
		code:        exitFailure,
	},
	"watch": {
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: watch: unknown arg '%s'", f) },
		extra:       func(w string) string { return fmt.Sprintf("crew: watch: unknown arg '%s'", w) },
		code:        exitFailure,
	},
	"stream": {
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: stream: unknown arg '%s'", f) },
		extra:       func(w string) string { return fmt.Sprintf("crew: stream: unknown arg '%s'", w) },
		code:        exitUsage,
	},
	"fleet stream": {
		unknownFlag: func(f string) string { return fmt.Sprintf("crew: fleet stream: unknown arg '%s'", f) },
		extra:       func(w string) string { return fmt.Sprintf("crew: fleet stream: unknown arg '%s'", w) },
		code:        exitUsage,
	},
	"roster": plain("roster"),
	"report": plain("report"),
	"retro":  {all: retro.Usage, code: exitFailure},
	"rate":   {all: rate.Usage, code: exitFailure},
	"fleet":  {all: fleet.Usage, code: exitFailure},
	// stack.Usage ends in the newline run adds back.
	"stack": {all: strings.TrimSuffix(stack.Usage, "\n"), code: exitFailure},
	"roster-render": {
		unknownFlag: func(f string) string { return "crew: roster-render: unknown arg '" + f + "'" },
		extra:       func(w string) string { return "crew: roster-render: unknown arg '" + w + "'" },
		code:        exitUsage,
	},
	"inbox":      plain("inbox"),
	"register":   plain("register"),
	"deregister": plain("deregister"),
	"log":        plain("log"),
}

// kongRefusal maps kong's error for the refusal key (a subcommand, or `hold <action>`
// and `fleet stream`) onto the subcommand's wording. The two
// prefixes are kong's own text, pinned by TestKongErrorClassification; anything
// else (a bad bool value) keeps kong's sentence under the subcommand's name.
func kongRefusal(sub string, err error) *refusal {
	w, ok := wordings[sub]
	if !ok {
		w = plain(sub)
	}
	text := err.Error()
	// kong appends a suggestion to some of its messages.
	text, _, _ = strings.Cut(text, ", did you mean")
	// A trailing --root binds no value, unlike the *string flags, which bind "".
	if sub == "rate" && strings.HasPrefix(text, "--root: missing value") {
		return &refusal{rateRootUsage, exitFailure}
	}
	if w.all != "" {
		return &refusal{w.all, w.code}
	}
	if flag, ok := strings.CutPrefix(text, "unknown flag "); ok {
		return &refusal{w.unknownFlag(flag), w.code}
	}
	if word, ok := strings.CutPrefix(text, "unexpected argument "); ok {
		return &refusal{w.extra(word), w.code}
	}
	return &refusal{fmt.Sprintf("crew: %s: %s", sub, text), w.code}
}

// given is a *string flag that was passed with an empty or missing value.
func given(s *string) bool { return s != nil && *s == "" }

// validate is each subcommand's semantic checks. They run after kong's grammar
// errors, in a fixed order rather than argv position.
func validate(sub string, r *root) *refusal {
	fail := func(msg string) *refusal { return &refusal{msg, exitFailure} }
	switch sub {
	case "where":
		c := r.Where
		if given(c.Crew) {
			return fail("crew: where: --crew needs an id (" + where.Usage + ")")
		}
		if c.Target == "" {
			return fail("crew: where: " + where.Usage)
		}
	case "resolve-target":
		c := r.Resolve
		if given(c.Crew) {
			return fail("crew: resolve-target: --crew needs an id")
		}
		if c.Target == "" {
			return fail(resolve.Usage)
		}
	case "identity":
		c := r.Identity
		if c.Hash != nil {
			if *c.Hash == "" {
				return fail(identityHashUsage)
			}
			return nil
		}
		if c.Branch == "" {
			return fail(identityUsage)
		}
	case "sessions":
		c := r.Sessions
		if given(c.Crew) {
			return fail("crew: --crew needs a value")
		}
		if c.Branch == "" {
			return fail(sessionsUsage)
		}
	case "inbox":
		c := r.Inbox
		// Only a worker id promises a session: a branch-only one matches no message
		// the caller could be waiting for, and #290's marks are per session.
		if strings.HasPrefix(c.Agent, "worker:") && !bus.IsSessionID(c.Agent) {
			return fail(fmt.Sprintf(
				"crew: inbox: '%s' has no session suffix — pass the session id ($CREW_WORKER_ID); a branch-only worker id matches no message", c.Agent))
		}
		if given(c.Since) {
			return fail("crew: --since needs a value")
		}
		if given(c.From) {
			return fail("crew: --from needs a value")
		}
		// Marks exist only for a session; the suffix check above covers any worker id.
		if c.Undelivered && !strings.HasPrefix(c.Agent, "worker:") {
			return fail("crew: inbox: --undelivered needs a worker session id ($CREW_WORKER_ID)")
		}
	case "reply":
		if given(r.Reply.Crew) {
			return fail("crew: --crew needs a value")
		}
	case "nudge":
		c := r.Nudge
		if given(c.Crew) {
			return fail("crew: nudge: --crew needs an id (usage: " + nudge.Usage + ")")
		}
		if _, ok := nudge.NewRequest("", c.Target, c.Wait); !ok {
			return fail("crew: nudge: one target only (usage: " + nudge.Usage + ")")
		}
	case "await":
		c := r.Await
		for _, f := range []struct {
			name string
			v    *string
		}{{"--from", c.From}, {"--timeout", c.Timeout}, {"--interval", c.Interval}} {
			if given(f.v) {
				return fail(fmt.Sprintf("crew: %s needs a value", f.name))
			}
		}
	case "watch":
		c := r.Watch
		for _, f := range []struct {
			name string
			v    *string
		}{{"--since", c.Since}, {"--states", c.States}, {"--timeout", c.Timeout}, {"--interval", c.Interval}, {"--crew", c.Crew}} {
			if given(f.v) {
				return fail(fmt.Sprintf("crew: %s needs a value", f.name))
			}
		}
	case "hold add":
		c := r.Hold.Add
		for _, f := range []struct {
			name string
			v    *string
		}{
			{"--engine", c.Engine}, {"--window", c.Window}, {"--resets-at", c.ResetsAt},
			{"--agent", c.Agent}, {"--ref", c.Ref}, {"--branch", c.Branch},
			{"--tier", c.Tier}, {"--model", c.Model}, {"--effort", c.Effort},
			{"--plan", c.Plan}, {"--mcp", c.MCP}, {"--shape", c.Shape},
			{"--spec", c.Spec}, {"--crew", c.Crew},
		} {
			if given(f.v) {
				return fail(fmt.Sprintf("crew: %s needs a value", f.name))
			}
		}
	case "hold list", "hold due", "hold park", "hold release":
		var crew *string
		switch sub {
		case "hold list":
			crew = r.Hold.List.Crew
		case "hold due":
			crew = r.Hold.Due.Crew
		case "hold park":
			crew = r.Hold.Park.Crew
		default:
			if r.Hold.Release.ID == "" {
				return fail(hold.ReleaseUsage)
			}
			crew = r.Hold.Release.Crew
		}
		if given(crew) {
			return fail("crew: --crew needs a value")
		}
	case "rate":
		for _, d := range r.Rate.Root {
			if given(d) {
				return fail(rateRootUsage)
			}
		}
	case "stream":
		c := r.Stream
		for _, f := range append(c.valueFlags(), valueFlag{"--reap-every", c.ReapEvery}) {
			if given(f.v) {
				return &refusal{fmt.Sprintf("crew: %s needs a value", f.name), exitUsage}
			}
		}
	case "fleet":
		if given(r.Fleet.Table.Crew) {
			return fail(fleet.Usage)
		}
	case "fleet stream":
		// `fleet --all stream` selects the lane with the table's flags bound.
		if t := r.Fleet.Table; t.All || t.JSON || t.Crew != nil {
			return fail(fleet.Usage)
		}
		c := r.Fleet.Stream
		if c.ReapEvery != nil {
			return &refusal{"crew: fleet stream: --reap-every is not supported — the fleet lane does not reap (each repo's own stream or dispatch does)", exitUsage}
		}
		for _, f := range append(c.valueFlags(), valueFlag{"--rediscover", c.Rediscover}) {
			if given(f.v) {
				return &refusal{fmt.Sprintf("crew: %s needs a value", f.name), exitUsage}
			}
		}
	case "stack":
		c := r.Stack
		if (c.Cut.Child == "" && c.Align.Top == "") || (c.Cut.Oid != "" && !c.Cut.Record) {
			return fail(strings.TrimSuffix(stack.Usage, "\n"))
		}
	case "roster-render":
		c := r.RosterRender
		for _, f := range []struct {
			name string
			v    *string
		}{{"--crew", c.Crew}, {"--pane", c.Pane}, {"--interval", c.Interval}, {"--quiet", c.Quiet}} {
			if given(f.v) {
				return &refusal{fmt.Sprintf("crew: %s needs a value", f.name), exitUsage}
			}
		}
	}
	return nil
}

// str is a *string flag's value, "" when absent.
func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
