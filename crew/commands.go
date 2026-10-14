package main

import (
	"fmt"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
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

// refusal is the stderr line and exit status a typed subcommand's argv earned.
type refusal struct {
	msg  string
	code int
}

const (
	identityUsage     = "crew: identity <branch> [crew]"
	identityHashUsage = "crew: identity --hash <name>"
	identitySignature = "usage: crew identity <branch> [crew]"
)

// wording is how one subcommand phrases the two grammar failures kong reports
// by prefix: the offending token is handed in bare.
type wording struct {
	unknownFlag func(flag string) string
	extra       func(word string) string
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
	"inbox":      plain("inbox"),
	"register":   plain("register"),
	"deregister": plain("deregister"),
	"log":        plain("log"),
}

// kongRefusal maps kong's error for sub onto the subcommand's wording. The two
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
