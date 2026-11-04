package ui

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// action is one operator command on the selected worker.
type action int

const (
	actJump action = iota
	actOpenPR
	actReply
	actNudge
)

var actionNames = [...]string{"jump", "open PR", "reply", "nudge"}

func (a action) String() string { return actionNames[a] }

// actionMsg is an action's outcome (or refusal), shown on the status line.
type actionMsg struct {
	text string
	err  bool
}

// actionDeps is what the Live view needs to run actions: the Runner and
// resolved crew prefix come from Deps, inTmux from $TMUX.
type actionDeps struct {
	ctx    context.Context
	runner data.Runner
	crew   []string
	inTmux bool
}

// disabledReason is why action a cannot run on w, or "" when it can. have is
// false when nothing is selected.
func disabledReason(a action, w data.Worker, inTmux, have bool) string {
	if !have {
		return "no worker selected"
	}
	switch a {
	case actJump:
		switch {
		case w.Host != "":
			return "remote worker (host " + cleanText(w.Host) + ")"
		case !inTmux:
			return "not inside tmux"
		case w.Branch == "":
			return "no branch"
		}
	case actOpenPR:
		if w.PRURL == "" {
			return "no PR"
		}
	case actReply:
		if w.State != "blocked" {
			return "not blocked"
		}
	case actNudge:
		if nudgeTarget(w) == "" {
			return "no name or branch"
		}
	}
	return ""
}

// nudgeTarget is the codename, else the branch.
func nudgeTarget(w data.Worker) string {
	if w.Name != "" {
		return w.Name
	}
	return w.Branch
}

// crewArgv is `<crew…> <sub> --crew <crew id> -- <target…>`.
func crewArgv(crew []string, sub string, w data.Worker, target ...string) []string {
	argv := append(slices.Clone(crew), sub, "--crew", w.Crew, "--")
	return append(argv, target...)
}

func whereArgv(crew []string, w data.Worker) []string { return crewArgv(crew, "where", w, w.Branch) }

func switchArgv(pane string) []string { return []string{"tmux", "switch-client", "-t", pane} }

func ghArgv(url string) []string { return []string{"gh", "pr", "view", "--web", url} }

func replyArgv(crew []string, w data.Worker, text string) []string {
	return crewArgv(crew, "reply", w, "worker:"+w.Branch, text)
}

func nudgeArgv(crew []string, w data.Worker) []string {
	return crewArgv(crew, "nudge", w, nudgeTarget(w))
}

const jumpMarker = "jump: ! tmux switch-client -t "

var paneIDRe = regexp.MustCompile(`^%[0-9]+$`)

// parseJumpPane extracts the pane id from `crew where` output: the first
// token after the jump marker. A line naming a host is refused because a
// remote worker's pane id belongs to another tmux server.
func parseJumpPane(out string) (string, error) {
	if strings.Contains(out, " [host ") {
		return "", errors.New("remote worker")
	}
	_, rest, ok := strings.Cut(out, jumpMarker)
	if !ok {
		return "", errors.New("no pane id")
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || !paneIDRe.MatchString(fields[0]) {
		return "", errors.New("no pane id")
	}
	return fields[0], nil
}

func statusCmd(text string, isErr bool) tea.Cmd {
	return func() tea.Msg { return actionMsg{text: text, err: isErr} }
}

// runActionCmd runs argv through r, then, when then is set, the argv it
// derives from the first step's stdout. The whole sequence runs inside the
// returned Cmd, never in Update.
func runActionCmd(ctx context.Context, r data.Runner, a action, argv []string, then func(stdout string) ([]string, error)) tea.Cmd {
	return func() tea.Msg {
		fail := func(reason string) tea.Msg {
			return actionMsg{text: cleanText(a.String() + " failed: " + reason), err: true}
		}
		out, err := runArgv(ctx, r, argv)
		if err != nil {
			return fail(err.Error())
		}
		if then != nil {
			next, err := then(out)
			if err != nil {
				return fail(err.Error())
			}
			if out, err = runArgv(ctx, r, next); err != nil {
				return fail(err.Error())
			}
		}
		first, _, _ := strings.Cut(strings.TrimLeft(out, "\r\n"), "\n")
		if first = strings.TrimSpace(first); first == "" {
			first = "ok"
		}
		return actionMsg{text: cleanText(a.String() + ": " + first)}
	}
}

func runArgv(ctx context.Context, r data.Runner, argv []string) (string, error) {
	stdout, stderr, err := r.Run(ctx, argv[0], argv[1:]...)
	if err != nil {
		return "", errors.New(data.FailureLine(stderr, err))
	}
	return string(stdout), nil
}

// startAction runs the key's action on the selected worker, opens its
// prompt (reply) or confirm (nudge), or reports why it is disabled.
func (v liveView) startAction(a action) (view, tea.Cmd) {
	sel, have := v.Selection().(data.Worker)
	if reason := disabledReason(a, sel, v.acts.inTmux, have); reason != "" {
		return v, statusCmd(a.String()+": "+reason, true)
	}
	switch a {
	case actJump:
		then := func(out string) ([]string, error) {
			pane, err := parseJumpPane(out)
			if err != nil {
				return nil, err
			}
			return switchArgv(pane), nil
		}
		return v, runActionCmd(v.acts.ctx, v.acts.runner, a, whereArgv(v.acts.crew, sel), then)
	case actOpenPR:
		return v, runActionCmd(v.acts.ctx, v.acts.runner, a, ghArgv(sel.PRURL), nil)
	case actReply:
		v.mode, v.target = modeReply, sel
		v.input = textinput.New()
		v.input.Prompt = "reply to " + cleanText(nudgeTarget(sel)) + ": "
		v.input.Focus()
		return v, textinput.Blink
	default:
		v.mode, v.target = modeConfirmNudge, sel
		return v, nil
	}
}

func (v liveView) handleReplyKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "esc":
		v.mode = modeNormal
		return v, nil
	case "enter":
		text := strings.TrimSpace(v.input.Value())
		if text == "" {
			return v, statusCmd("reply: text required", true)
		}
		v.mode = modeNormal
		return v, runActionCmd(v.acts.ctx, v.acts.runner, actReply, replyArgv(v.acts.crew, v.target, text), nil)
	}
	var cmd tea.Cmd
	v.input, cmd = v.input.Update(msg)
	return v, cmd
}

func (v liveView) handleConfirmKey(msg tea.KeyMsg) (view, tea.Cmd) {
	v.mode = modeNormal
	if msg.String() != "y" {
		return v, statusCmd("nudge: cancelled", false)
	}
	return v, runActionCmd(v.acts.ctx, v.acts.runner, actNudge, nudgeArgv(v.acts.crew, v.target), nil)
}
