// Package overseer is the overseer session's verbs: `crew overseer msg`.
package overseer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/status"
)

// MsgUsage is the line every `crew overseer msg` grammar refusal prints.
const MsgUsage = "usage: crew overseer msg <repo> <crew> '<json>'"

const exitFailure = 1

var (
	kinds     = []string{"ask", "request", "capacity", "notice"}
	errNoSelf = errors.New("no host-name probe")
)

// MsgRequest is `crew overseer msg <repo> <crew> '<json>'`.
type MsgRequest struct{ Repo, Crew, Body string }

// Options carry every outside read, so tests use temp dirs and stubs.
type Options struct {
	Getenv   func(string) string // repos.Discover's env (registry, CREW_SWEEP_ROOTS, HOME); nil: os.Getenv
	Registry string              // repos.Options.Registry
	Git      git.Client          // names a discovered repo from its origin
	Clock    clock.Clock
	Sender   sender.Probes
}

// RunMsg posts the overseer's JSON message to the dispatcher of <crew> on the
// bus of <repo>: a repo named as crew fleet names it, or its git common dir.
// The sender check runs first, so no other repo is read for a session that may
// not post. Every refusal prints one line and writes nothing.
func RunMsg(ctx context.Context, req MsgRequest, stderr io.Writer, o Options) int {
	self, err := hostName(o.Sender)
	if err != nil {
		return refuse(stderr, "this host's name cannot be read (%v)", err)
	}
	// The raw crew goes to the check on purpose: no overseer rule reads it.
	from := "overseer:" + self
	if reason := sender.Check(ctx, from, req.Crew, o.Sender); reason != "" {
		return refuse(stderr, "refusing sender '%s' — %s", from, reason)
	}
	if reason := bodyRefusal(req.Body); reason != "" {
		return refuse(stderr, "%s", reason)
	}
	common, reason := findBus(ctx, req.Repo, o, stderr)
	if reason != "" {
		return refuse(stderr, "%s", reason)
	}
	paths := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	if !bus.ValidCrewID(req.Crew) || !slices.Contains(crews.IDs(paths), req.Crew) {
		return refuse(stderr, "crew %s is not on %s's bus", req.Crew, common)
	}
	return status.RunOverseerMsg(ctx, status.OverseerMsgRequest{Self: self, Crew: req.Crew, Body: req.Body}, paths, stderr, status.Options{Clock: o.Clock, Sender: o.Sender})
}

func refuse(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, "crew: overseer msg: "+format+"\n", args...)
	return exitFailure
}

func hostName(p sender.Probes) (string, error) {
	if p.Self == nil {
		return "", errNoSelf
	}
	return p.Self()
}

const reasonControl = "body has a control or invisible character (escaped ones too); send compact one-line JSON, e.g. jq -c"

// bodyRefusal is why body is not a message the overseer may post, "" when it
// is: one JSON object of an allowed kind, as one line of visible text.
func bodyRefusal(body string) string {
	if len(body) > peer.MaxBody {
		return fmt.Sprintf("body is %d bytes, over the %d-byte limit", len(body), peer.MaxBody)
	}
	if !utf8.ValidString(body) {
		return "body is not valid UTF-8"
	}
	if hidden(body) {
		return reasonControl
	}
	v, err := jqrun.FromJSON(body)
	if err != nil {
		return "body is not valid JSON"
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return "body must be a JSON object"
	}
	// Decoding unescapes, so \u001b and \n only show up here.
	if hiddenIn(obj) {
		return reasonControl
	}
	kind, isString := obj["kind"].(string)
	if slices.Contains(kinds, kind) {
		return ""
	}
	want := "body kind must be one of " + strings.Join(kinds, "|")
	if isString {
		return fmt.Sprintf("%s, got %q", want, kind)
	}
	return want
}

// hidden is the peer channel's test for text a terminal hides or a model
// misreads: C0, DEL and C1 controls, plus the invisible format characters.
func hidden(s string) bool {
	return invisible.Has(s) || strings.IndexFunc(s, func(r rune) bool {
		return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
	}) >= 0
}

func hiddenIn(v any) bool {
	switch x := v.(type) {
	case string:
		return hidden(x)
	case map[string]any:
		for k, item := range x {
			if hidden(k) || hiddenIn(item) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(x, hiddenIn)
	}
	return false
}

// findBus resolves repo to the git common dir of a discovered repo with a bus,
// else returns the refusal reason. An exact common dir wins; otherwise repo is
// matched as the name crew fleet gives it, which must pick one repo.
func findBus(ctx context.Context, repo string, o Options, stderr io.Writer) (string, string) {
	found := repos.Discover(repos.Options{Registry: o.Registry, Getenv: o.Getenv, Stderr: stderr, Prefix: "crew: overseer msg"})
	for _, r := range found {
		if r.Common != repo {
			continue
		}
		if r.Status != repos.StatusOK {
			return "", fmt.Sprintf("repo %s is not a usable crew bus (%s)", repo, r.Status)
		}
		return r.Common, ""
	}
	var named []string
	for _, r := range found {
		if r.Status != repos.StatusOK {
			continue
		}
		origin, _ := git.Origin(ctx, o.Git, r.Common)
		if repos.Name(r.Common, origin) == repo {
			named = append(named, r.Common)
		}
	}
	switch len(named) {
	case 0:
		return "", "repo " + repo + " is not a discovered crew bus; pass a repo as crew fleet names it, or its git common dir"
	case 1:
		return named[0], ""
	}
	return "", fmt.Sprintf("repo %s is ambiguous (%s); pass the git common dir", repo, strings.Join(named, ", "))
}
