package ui

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

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
		switch {
		case w.PRURL == "":
			return "no PR"
		case !ghPRURLRe.MatchString(w.PRURL):
			return "no valid PR URL"
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

// ghPRURLRe is the only shape of worker-written pr_url allowed to reach gh: it
// cannot read as a flag, resolve against the cwd repo, or name another host.
var ghPRURLRe = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/[0-9]+$`)

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

// parseJumpPane extracts the pane id from `crew where` output. The window name
// is worker-controlled and printed before the real marker, so the first line
// is parsed from its end: the last marker's token is the pane, and only the
// text after that token can name a host. A host suffix is refused because a
// remote worker's pane id belongs to another tmux server.
func parseJumpPane(out string) (string, error) {
	line, _, _ := strings.Cut(out, "\n")
	i := strings.LastIndex(line, jumpMarker)
	if i < 0 {
		return "", errors.New("no pane id")
	}
	rest := line[i+len(jumpMarker):]
	pane, _, _ := strings.Cut(rest, " ")
	if !paneIDRe.MatchString(pane) {
		return "", errors.New("no pane id")
	}
	if strings.Contains(rest[len(pane):], " [host ") {
		return "", errors.New("remote worker")
	}
	return pane, nil
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

// actionUI is the prompt and confirm state and the action dispatch shared by
// every view that acts on a worker. Views embed it and call its pointer
// methods on their own (copied) receiver.
type actionUI struct {
	acts actionDeps

	mode   liveMode
	input  textinput.Model // the reply prompt
	target data.Worker     // the worker the open prompt or confirm acts on
	width  int             // body width, from the root's WindowSizeMsg
}

// start runs action a on sel, opens its prompt (reply) or confirm (nudge), or
// reports why it is disabled. have is false when nothing is selected.
func (u *actionUI) start(a action, sel data.Worker, have bool) tea.Cmd {
	if reason := disabledReason(a, sel, u.acts.inTmux, have); reason != "" {
		return statusCmd(a.String()+": "+reason, true)
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
		return runActionCmd(u.acts.ctx, u.acts.runner, a, whereArgv(u.acts.crew, sel), then)
	case actOpenPR:
		return runActionCmd(u.acts.ctx, u.acts.runner, a, ghArgv(sel.PRURL), nil)
	case actReply:
		u.mode, u.target = modeReply, sel
		u.input = textinput.New()
		u.fit()
		u.input.Focus()
		return textinput.Blink
	default:
		u.mode, u.target = modeConfirmNudge, sel
		return nil
	}
}

// fit sizes the reply prompt and input to the body width so textinput
// scrolls its text and cursor into view instead of the frame clipping them.
// The prompt takes at most half the width, and a Width of 0 would mean
// unlimited to textinput, so the input keeps at least one cell.
func (u *actionUI) fit() {
	const lead, tail = "reply to ", ": "
	room := max(u.width/2-ansi.StringWidth(lead)-ansi.StringWidth(tail), 1)
	target := ansi.Truncate(cleanText(nudgeTarget(u.target)), room, "…")
	u.input.Prompt = lead + target + tail
	u.input.Width = max(u.width-ansi.StringWidth(u.input.Prompt)-1, 1)
}

// handleKey feeds msg to the open reply prompt or nudge confirm. handled is
// false when neither is open, so the caller keeps the key.
func (u *actionUI) handleKey(msg tea.KeyMsg) (cmd tea.Cmd, handled bool) {
	switch u.mode {
	case modeReply:
		return u.handleReplyKey(msg), true
	case modeConfirmNudge:
		return u.handleConfirmKey(msg), true
	}
	return nil, false
}

func (u *actionUI) handleReplyKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+c":
		u.mode = modeNormal
		return nil
	case "enter":
		text := strings.TrimSpace(u.input.Value())
		if text == "" {
			return statusCmd("reply: text required", true)
		}
		u.mode = modeNormal
		return runActionCmd(u.acts.ctx, u.acts.runner, actReply, replyArgv(u.acts.crew, u.target, text), nil)
	}
	var cmd tea.Cmd
	u.input, cmd = u.input.Update(msg)
	return cmd
}

func (u *actionUI) handleConfirmKey(msg tea.KeyMsg) tea.Cmd {
	u.mode = modeNormal
	if msg.String() != "y" {
		return statusCmd("nudge: cancelled", false)
	}
	return runActionCmd(u.acts.ctx, u.acts.runner, actNudge, nudgeArgv(u.acts.crew, u.target), nil)
}

// prompt is the open reply prompt or nudge confirm, else fallback.
func (u *actionUI) prompt(fallback string) string {
	switch u.mode {
	case modeReply:
		return u.input.View()
	case modeConfirmNudge:
		return "nudge " + cleanText(nudgeTarget(u.target)) + "? y/N"
	}
	return fallback
}

// promptKeys is the help for the open reply prompt or nudge confirm, nil when
// neither is open.
func (u *actionUI) promptKeys() []key.Binding {
	switch u.mode {
	case modeReply:
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		}
	case modeConfirmNudge:
		return []key.Binding{
			key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm")),
			key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n/esc", "cancel")),
		}
	}
	return nil
}

// workerFromRow maps a roster worker map to the Worker the actions take.
func workerFromRow(crewID string, w map[string]any) data.Worker {
	f := func(k string) string { s, _ := strField(w, k); return s }
	return data.Worker{
		Crew:    crewID,
		Branch:  f("branch"),
		Session: f("session"),
		Name:    f("name"),
		State:   f("state"),
		PRURL:   f("pr_url"),
		Host:    f("host"),
	}
}

// workerByBranch finds crewID's worker on branch over every worker in the
// roster, reaped ones included. A crew whose workers failed to load has none.
func workerByBranch(snap data.Snapshot, crewID, branch string) (data.Worker, bool) {
	for _, c := range snap.Roster.Crews {
		if c.ID != crewID || c.WorkersError != nil {
			continue
		}
		for _, w := range c.Workers {
			if b, _ := strField(w, "branch"); b == branch {
				return workerFromRow(crewID, w), true
			}
		}
	}
	return data.Worker{}, false
}
