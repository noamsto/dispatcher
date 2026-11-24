package overseer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/status"
)

// TrustUsage is the line every `crew overseer trust` grammar refusal prints.
const TrustUsage = "usage: crew overseer trust <pane>"

const (
	exitRefused      = 2 // refused before typing
	exitTyped        = 3 // typed, and the dialog is still shown
	exitNoticeFailed = 4 // typed and the dialog cleared, but the notice failed

	trustFormat = "#{pane_id}\x1f#{pane_current_command}\x1f#{pane_current_path}\x1f#{@crew_role}\x1f#{@crew_id}"
	// trustPoll × trustPolls is how long the dialog has to clear after the Enter.
	trustPoll  = "0.5"
	trustPolls = 6
)

var paneIDRe = regexp.MustCompile(`^%[0-9]+$`)

// trustPane is what tmux reports of the pane RunTrust may type into.
type trustPane struct{ id, cmd, path, role, crew string }

// RunTrust accepts Claude Code's workspace-trust dialog in one local engine
// pane with a single Enter, then posts a notice to the dispatcher of the crew
// that owns the pane. The checks run in a fixed order, sender first, so a
// caller that is not the overseer reads no settings and no pane; every refusal
// prints one line, types nothing and writes no row. Exit 0 accepted, 1 a usage
// or resolution error (nothing typed), 2 refused before typing, 3 typed and the
// dialog is still shown (even when its notice also fails), 4 typed and accepted
// but the notice to the dispatcher failed.
func RunTrust(ctx context.Context, pane string, stdout, stderr io.Writer, o Options) int {
	self, err := hostName(o.Sender)
	if err != nil {
		return trustFail(stderr, exitFailure, "this host's name cannot be read (%v)", err)
	}
	// An overseer pane stamp carries no branch, so no crew is consulted here.
	from := "overseer:" + self
	if reason := sender.Check(ctx, from, "", o.Sender); reason != "" {
		return trustFail(stderr, exitRefused, "refusing sender '%s' — %s", from, reason)
	}
	on, err := o.acceptTrust(ctx)
	if err != nil {
		return trustFail(stderr, exitFailure, "overseer.acceptTrust cannot be read (%v)", err)
	}
	if !on {
		return trustFail(stderr, exitRefused, "overseer.acceptTrust is off — the owner enables it")
	}
	p, code, reason := trustTarget(ctx, pane, o)
	if reason != "" {
		return trustFail(stderr, code, "%s", reason)
	}
	crew, paths, reason := trustBus(ctx, p, stderr, o)
	if reason != "" {
		return trustFail(stderr, exitFailure, "%s", reason)
	}
	text, err := o.Tmux.Run(ctx, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return trustFail(stderr, exitFailure, "pane %s cannot be captured", pane)
	}
	if frame.IsAddDirPrompt("claude", text) {
		return trustFail(stderr, exitRefused, "add-dir trust dialog rejects injected keys — relay it to the human")
	}
	if !frame.IsWorkspaceTrustPrompt(text) {
		return trustFail(stderr, exitRefused, "pane %s does not show the workspace-trust dialog with '1. Yes, I trust this folder' selected", pane)
	}
	if _, err := o.Tmux.Run(ctx, "send-keys", "-t", pane, "Enter"); err != nil {
		return trustFail(stderr, exitRefused, "send-keys to pane %s failed", pane)
	}
	detail := "accepted the workspace-trust dialog in pane " + pane
	code = 0
	if !trustCleared(ctx, pane, o) {
		detail = "typed Enter, workspace-trust dialog still shown in pane " + pane
		code = exitTyped
		_, _ = fmt.Fprintf(stderr, "crew: overseer trust: %s\n", detail)
	}
	if err := trustNotice(ctx, self, crew, detail, paths, o); err != nil {
		_, _ = fmt.Fprintf(stderr, "crew: overseer trust: typed Enter in pane %s, but the notice to dispatcher:%s failed (%v)\n", pane, crew, err)
		if code == 0 {
			code = exitNoticeFailed
		}
	}
	if code == 0 {
		_, _ = fmt.Fprintln(stdout, detail)
	}
	return code
}

// trustTarget is the pane's tmux report when it is a local engine pane other
// than the overseer's own, else the exit code and refusal reason.
func trustTarget(ctx context.Context, pane string, o Options) (trustPane, int, string) {
	if !paneIDRe.MatchString(pane) {
		return trustPane{}, exitFailure, fmt.Sprintf("%q is not a pane id (%%N)", pane)
	}
	out, err := o.Tmux.Run(ctx, "display-message", "-p", "-t", pane, trustFormat)
	f := strings.Split(out, "\x1f")
	if err != nil || len(f) != 5 || f[0] != pane {
		return trustPane{}, exitFailure, "no pane " + pane + " on this host's tmux server"
	}
	p := trustPane{id: f[0], cmd: f[1], path: f[2], role: f[3], crew: f[4]}
	if p.role == "overseer" {
		return trustPane{}, exitRefused, "pane " + pane + " is the overseer's own pane"
	}
	if !roster.IsEngineCmd(p.cmd) {
		who := p.cmd
		if who == "" {
			who = "no command"
		}
		return trustPane{}, exitRefused, "pane " + pane + " is not running an engine (" + who + ")"
	}
	return p, 0, ""
}

// trustBus is the crew that owns p and its bus, else the refusal reason. The
// repo is the git common dir of the pane's cwd; the crew is the pane's @crew_id
// stamp (a worker or role pane), else the one crew whose dispatcher pane file
// names it. Either way the crew must be on that repo's discovered bus.
func trustBus(ctx context.Context, p trustPane, stderr io.Writer, o Options) (string, bus.Paths, string) {
	if crew, paths, reason, ok := layout2Trust(p, o); ok {
		return crew, paths, reason
	}
	// git with an empty dir would read the overseer's own cwd.
	if !filepath.IsAbs(p.path) {
		return "", bus.Paths{}, "pane " + p.id + " has no working directory"
	}
	common := git.Output(ctx, o.Git, p.path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == "" {
		return "", bus.Paths{}, "pane " + p.id + "'s directory " + p.path + " is not in a git repo"
	}
	crew := p.crew
	if !bus.ValidCrewID(crew) {
		var reason string
		if crew, reason = paneCrew(common, p.id); reason != "" {
			return "", bus.Paths{}, reason
		}
	}
	found := repos.Discover(repos.Options{Registry: o.Registry, Getenv: o.Getenv, Stderr: stderr, Prefix: "crew: overseer trust"})
	paths, reason := busFor(ctx, common, crew, found, o)
	if reason != "" {
		return "", bus.Paths{}, reason
	}
	return crew, paths, ""
}

// layout2Trust resolves p's crew when it is layout 2, with no git call; ok is
// false when today's repo path must decide.
func layout2Trust(p trustPane, o Options) (string, bus.Paths, string, bool) {
	if bus.ValidCrewID(p.crew) {
		paths, _, reason, ok := layout2Crew(o, p.crew)
		if !ok {
			return "", bus.Paths{}, "", false
		}
		if reason != "" {
			return "", bus.Paths{}, reason, true
		}
		return p.crew, paths, "", true
	}
	state := stateRoot(o)
	if state == "" {
		return "", bus.Paths{}, "", false
	}
	crew, reason := paneCrew2(state, p.id)
	if reason != "" {
		return "", bus.Paths{}, reason, true
	}
	if crew == "" {
		return "", bus.Paths{}, "", false
	}
	return crew, bus.StatePaths(state), "", true
}

// paneCrew is the one crew of common whose crews/<id>/pane file names pane,
// else the refusal reason.
func paneCrew(common, pane string) (string, string) {
	paths := bus.PathsFor(common)
	dir := paths.CrewsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "pane " + pane + " has no @crew_id stamp, and " + dir + " cannot be read"
	}
	var named []string
	for _, e := range entries {
		crew, err := paths.Crew(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(crew.Dir + "/pane")
		if err == nil && strings.TrimSpace(string(data)) == pane {
			named = append(named, e.Name())
		}
	}
	switch len(named) {
	case 0:
		return "", "pane " + pane + " has no @crew_id stamp, and no crew of " + common + " names it as its dispatcher pane"
	case 1:
		return named[0], ""
	}
	return "", fmt.Sprintf("pane %s has no @crew_id stamp, and crews %s all name it as their dispatcher pane", pane, strings.Join(named, ", "))
}

// trustCleared re-captures pane until the workspace-trust dialog is gone, up to
// trustPolls times. An unreadable capture is not proof it cleared.
func trustCleared(ctx context.Context, pane string, o Options) bool {
	for range trustPolls {
		if o.Clock.SleepCtx(ctx, trustPoll, io.Discard) != nil {
			return false
		}
		if text, err := o.Tmux.Run(ctx, "capture-pane", "-p", "-t", pane); err == nil && !frame.IsWorkspaceTrustPrompt(text) {
			return true
		}
	}
	return false
}

// trustNotice posts the notice to dispatcher:<crew> through the overseer msg
// path; its error is the first line the post printed.
func trustNotice(ctx context.Context, self, crew, detail string, paths bus.Paths, o Options) error {
	body, err := json.Marshal(struct {
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	}{"notice", detail})
	if err != nil {
		return err
	}
	var why bytes.Buffer
	req := status.OverseerMsgRequest{Self: self, Crew: crew, Body: string(body)}
	if status.RunOverseerMsg(ctx, req, paths, &why, status.Options{Clock: o.Clock, Sender: o.Sender}) == 0 {
		return nil
	}
	line, _, _ := strings.Cut(strings.TrimSpace(why.String()), "\n")
	return errors.New(line)
}

// acceptTrust is o.AcceptTrust's answer, off when none is wired.
func (o Options) acceptTrust(ctx context.Context) (bool, error) {
	if o.AcceptTrust == nil {
		return false, nil
	}
	return o.AcceptTrust(ctx)
}

func trustFail(stderr io.Writer, code int, format string, args ...any) int {
	fail(stderr, "trust", format, args...)
	return code
}
