// Command crew-go is the crew entrypoint: it routes `--help` and every
// subcommand, and reports bare `crew` and an unknown one as the grouped help on
// stderr. flake.nix installs it as `crew` behind a PATH wrapper that keeps
// argv[0].
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/noamsto/dispatcher/crew/internal/adopt"
	"github.com/noamsto/dispatcher/crew/internal/await"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/entry"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/inbox"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/log"
	"github.com/noamsto/dispatcher/crew/internal/nudge"
	"github.com/noamsto/dispatcher/crew/internal/piagentdir"
	"github.com/noamsto/dispatcher/crew/internal/prwatch"
	"github.com/noamsto/dispatcher/crew/internal/rate"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/register"
	"github.com/noamsto/dispatcher/crew/internal/reply"
	"github.com/noamsto/dispatcher/crew/internal/report"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
	"github.com/noamsto/dispatcher/crew/internal/retro"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/rosterrender"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
	"github.com/noamsto/dispatcher/crew/internal/stack"
	"github.com/noamsto/dispatcher/crew/internal/stall"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
	"github.com/noamsto/dispatcher/crew/internal/status"
	"github.com/noamsto/dispatcher/crew/internal/stream"
	"github.com/noamsto/dispatcher/crew/internal/watch"
	"github.com/noamsto/dispatcher/crew/internal/where"
)

const (
	sessionsUsage = "crew: sessions <branch> [--crew ID]"
	exitFailure   = 1
	exitOpen      = 2 // sessions prints [] where jq slurps zero inputs
	exitUsage     = 64
)

// isHelpFlag is the two spellings crew.sh's help preamble matched.
func isHelpFlag(s string) bool { return s == "--help" || s == "-h" }

// isSweepAll names the mode on a line kong could not parse, so its refusal still
// prints outside a repo.
func isSweepAll(a string) bool { return strings.HasPrefix(a, "--sweep-all") }

// arg is args[i], or "" when the caller passed fewer arguments.
func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// routeHelp is crew.sh's help preamble (#812): `--help` as the command itself,
// and as the FIRST argument of one. Deliberately before the repo, crew-id and
// bus reads below, so `crew <sub> --help` answers in a bare temp dir. Only those
// positions count: `hold` takes an action and `fleet stream` a sub-subcommand,
// so their --help is one position deeper, and `crew status w1 working --help`
// keeps --help as the detail text. The screens are crew/internal/entry.
func routeHelp(sub string, args []string, stdout, stderr io.Writer) (int, bool) {
	rest := []string{}
	if len(args) > 1 {
		rest = args[1:]
	}
	switch sub {
	case "--help", "-h", "help":
		return entry.RunHelp(rest, stdout, stderr), true
	case "hold":
		if isHelpFlag(arg(args, 2)) {
			return entry.RunHelp([]string{"hold", arg(args, 1)}, stdout, stderr), true
		}
	case "fleet":
		if arg(args, 1) == "stream" && isHelpFlag(arg(args, 2)) {
			return entry.RunHelp([]string{"fleet"}, stdout, stderr), true
		}
	}
	if isHelpFlag(arg(args, 1)) {
		return entry.RunHelp([]string{sub}, stdout, stderr), true
	}
	return 0, false
}

// The kong root (#862). Every subcommand is a kong command: the converted ones
// bind typed fields (commands.go), and dash alone is registered around the
// passthrough command, so kong accepts the word and stops. Two measurements of
// kong v1.16.1 decide the shape.
//
//   - Kong splits the first word after a command name at `=`, so
//     `crew dash --crew=x` would reach dash as `--crew x`. Dash parses the
//     raw tail, not the capture; the split is pinned by
//     TestParseCrewSplitsLeadingEquals. The typed commands want that split:
//     `--flag=value` is `--flag value`.
//   - With the help flag off, the writers discarded and the exit func empty,
//     kong can neither print a screen nor end the process, so routeHelp and
//     helpTexts still decide when help prints and what it says.

// subcommands is every word crew admits: the thirty-seven the grammar types, then
// dash, which parses its own argv. It is the list the switch of #821 admitted,
// and tests/crew.bats parses this literal to prove every subcommand has its own
// row in the grouped help; TestAdmittedSubcommands pins it against that
// revision and against what the grammar really admits.
var subcommands = []string{
	"id", "new", "engine-cmd", "occupants",
	"where", "identity", "resolve-target", "crews", "register", "deregister", "log", "sessions", "inbox",
	"roster", "report", "retro", "rate", "fleet", "stack", "roster-render", "pi-agent-dir",
	"adopt", "await", "dash", "git-baseline",
	"git-guard", "help", "hold", "msg", "nudge",
	"pr-watch", "reap", "reap-autosweep",
	"reply", "stall-watch", "status", "stream", "watch",
}

// typedSubcommands are the subcommands whose argv root binds: every name but
// dash, which is registered as a dynamic passthrough command.
var typedSubcommands = map[string]bool{
	"engine-cmd": true, "id": true, "new": true, "occupants": true,
	"where": true, "identity": true, "resolve-target": true, "crews": true, "register": true,
	"deregister": true, "log": true, "sessions": true, "inbox": true,
	"roster": true, "report": true, "retro": true, "rate": true, "fleet": true,
	"stack": true, "roster-render": true, "pi-agent-dir": true,
	"status": true, "msg": true, "reply": true, "hold": true,
	"nudge": true, "await": true, "watch": true, "stream": true,
	"git-guard": true, "git-baseline": true, "reap": true, "reap-autosweep": true,
	"adopt": true, "pr-watch": true, "stall-watch": true, "help": true,
}

// passthroughCmd is dash to kong: its name, then whatever words it wants, kept
// as the caller typed them — an unknown flag, a `--`
// separator and a `--help` that is detail text rather than a request, all of
// them reaching the arm that reads them.
type passthroughCmd struct {
	Args []string `arg:"" optional:"" passthrough:"" name:"args"`
}

// argvCmd is a converted subcommand whose argv is a list of words and nothing
// else. id, new and pi-agent-dir bind it and drop it, because their arms ignored argv and an
// empty kong command would refuse `crew id extra` where the arm printed the id.
// engine-cmd and occupants read the first word, which is data even when it is
// flag-shaped (`crew occupants --foo` looks a path up, it does not refuse).
//
// Binding is the gate a conversion passes rather than the default, and
// docs/crew-go-port.md ("kong, the root parser") says what it is measured
// against: no engine name and no worktree path survives half of a `=`.
type argvCmd struct {
	Args []string `arg:"" optional:"" passthrough:"all" name:"args"`
}

// root is the grammar's typed subcommands.
type root struct {
	ID        argvCmd `cmd:"" name:"id" help:"Print this session's crew id."`
	New       argvCmd `cmd:"" name:"new" help:"Mint a crew id."`
	EngineCmd argvCmd `cmd:"" name:"engine-cmd" help:"Is this pane command an engine?"`
	Occupants argvCmd `cmd:"" name:"occupants" help:"The worker windows on a worktree."`

	Where      whereCmd      `cmd:"" name:"where" help:"Where a worker's pane is."`
	Identity   identityCmd   `cmd:"" name:"identity" help:"The codename a branch works under."`
	Resolve    resolveCmd    `cmd:"" name:"resolve-target" help:"What a target names on the bus."`
	Crews      crewsCmd      `cmd:"" name:"crews" help:"This repo's crews."`
	Register   registerCmd   `cmd:"" name:"register" help:"Claim this crew directory."`
	Deregister deregisterCmd `cmd:"" name:"deregister" help:"Release this crew directory."`
	Log        logCmd        `cmd:"" name:"log" help:"The crew's bus rows."`
	Sessions   sessionsCmd   `cmd:"" name:"sessions" help:"A branch's sessions."`
	Inbox      inboxCmd      `cmd:"" name:"inbox" help:"Msgs addressed to one agent."`

	Roster       rosterCmd       `cmd:"" name:"roster" help:"This crew's workers."`
	Report       reportCmd       `cmd:"" name:"report" help:"The crew's report."`
	Retro        retroCmd        `cmd:"" name:"retro" help:"The retro notes."`
	Rate         rateCmd         `cmd:"" name:"rate" help:"The ratings sweep and report."`
	Fleet        fleetCmd        `cmd:"" name:"fleet" help:"Every crew on the machine."`
	Stack        stackCmd        `cmd:"" name:"stack" help:"Cut points and rebase plans of a stack."`
	RosterRender rosterRenderCmd `cmd:"" name:"roster-render" help:"The per-crew diagram daemon."`
	PiAgentDir   argvCmd         `cmd:"" name:"pi-agent-dir" help:"The pi agent dir."`

	Status statusCmd `cmd:"" name:"status" help:"Post a worker's state."`
	Msg    argvCmd   `cmd:"" name:"msg" help:"Post a message."`
	Reply  replyCmd  `cmd:"" name:"reply" help:"Answer a worker as the dispatcher."`
	Hold   holdCmd   `cmd:"" name:"hold" help:"Queued dispatches waiting on a quota window."`
	Nudge  nudgeCmd  `cmd:"" name:"nudge" help:"Type into a lead's pane."`
	Await  awaitCmd  `cmd:"" name:"await" help:"Block until a msg answers an agent."`
	Watch  watchCmd  `cmd:"" name:"watch" help:"Park until a worker event qualifies."`
	Stream streamCmd `cmd:"" name:"stream" help:"The long-lived lane that pushes batches."`

	GitGuard    gitGuardCmd    `cmd:"" name:"git-guard" help:"The git-config guard worktree-git.sh runs."`
	GitBaseline gitBaselineCmd `cmd:"" name:"git-baseline" help:"List or accept the git-config baseline."`

	Adopt adoptCmd `cmd:"" name:"adopt" help:"Re-attach to a crew already on disk."`

	PRWatch prWatchCmd `cmd:"" name:"pr-watch" help:"Park on a PR and post its change event to the bus."`

	StallWatch stallWatchCmd `cmd:"" name:"stall-watch" help:"The per-worker liveness watchdog."`

	Reap          reapCmd `cmd:"" name:"reap" help:"Reclaim the windows and worktrees of landed workers."`
	ReapAutosweep argvCmd `cmd:"" name:"reap-autosweep" help:"The detached ratings sweep reap starts."`

	Help argvCmd `cmd:"" name:"help" help:"The grouped help."`
}

// crewArgs is what the root parse resolves to: the word that named the
// subcommand, the raw tail dash parses, the bound fields of the typed
// subcommands, and the refusal a typed subcommand's argv earned, if any.
type crewArgs struct {
	sub     string
	argv    []string
	typed   root
	refusal *refusal
	cmd     string // kong's selected command path, "" when the parse failed
}

// newCrewParser builds the grammar: the converted subcommands as fields of
// root, and dash as a dynamic command on the capture.
func newCrewParser(r *root, capture *passthroughCmd) (*kong.Kong, error) {
	opts := []kong.Option{
		kong.Name("crew"),
		kong.NoDefaultHelp(),
		// A flag's value may start with `--`, as it did for the arms' own loops.
		kong.WithHyphenPrefixedParameters(true),
		kong.Writers(io.Discard, io.Discard),
		kong.Exit(func(int) {}),
		kong.NamedMapper("optwait", kong.MapperFunc(optWaitMapper)),
	}
	for _, name := range subcommands {
		if !typedSubcommands[name] {
			opts = append(opts, kong.DynamicCommand(name, "", "", capture))
		}
	}
	return kong.New(r, opts...)
}

// parseCrew selects the subcommand. ok is false for exactly the input the
// switch's default arm rejected — no words at all, or a first word that names
// nothing — and nothing of kong's own error reaches stderr on the way out.
//
// The parser is built per call because kong writes the capture into the struct
// it was handed, so one shared across calls would keep a previous call's argv on
// any path it did not take. Building it is reflection over every command.
func parseCrew(args []string) (crewArgs, bool) {
	var (
		r       root
		capture passthroughCmd
	)
	p, err := newCrewParser(&r, &capture)
	if err != nil {
		panic("crew: kong grammar: " + err.Error())
	}
	kctx, err := p.Parse(args)
	if err != nil {
		// An error inside a typed subcommand's argv is that subcommand's refusal;
		// everywhere else it is the unknown word the switch rejected.
		if len(args) > 0 && typedSubcommands[args[0]] {
			return crewArgs{sub: args[0], argv: args[1:], refusal: kongRefusal(refusalKey(args), err)}, true
		}
		return crewArgs{}, false
	}
	// The one token kong selects that no arm admitted: it swallows a leading
	// `--` and matches the word after it, so `crew -- id` would reach no arm and
	// exit 0 where the switch printed the list and exited 1.
	if !slices.Contains(subcommands, args[0]) {
		return crewArgs{}, false
	}
	// kong matched args[0] to a command, so the words it describes are the rest
	// of the argv — the tail the arms have always been handed.
	cmd := kctx.Command()
	key := refusalKey(args)
	// `fleet --all stream` selects the lane behind a table flag, which only the
	// selected command shows.
	if strings.HasPrefix(cmd, "fleet stream") {
		key = "fleet stream"
	}
	return crewArgs{sub: args[0], argv: args[1:], typed: r, refusal: validate(key, &r), cmd: cmd}, true
}

// refusalKey names the wording and checks a typed argv is judged by: the
// subcommand, or its action word when the subcommand has one.
func refusalKey(args []string) string {
	if len(args) > 1 {
		switch {
		case args[0] == "hold" && slices.Contains([]string{"add", "list", "due", "park", "release"}, args[1]):
			return "hold " + args[1]
		case args[0] == "fleet" && args[1] == "stream":
			return "fleet stream"
		}
	}
	return args[0]
}

// holdRequest is the typed hold call: the action is the word after `hold` in
// the command kong selected.
func holdRequest(p crewArgs) hold.Request {
	h := p.typed.Hold
	action := strings.Fields(p.cmd)[1]
	switch action {
	case "add":
		a := h.Add
		return hold.Request{Action: action, Crew: str(a.Crew), Add: hold.AddRequest{
			Engine: str(a.Engine), Window: str(a.Window), ResetsAt: str(a.ResetsAt),
			Agent: str(a.Agent), Ref: str(a.Ref), Branch: str(a.Branch),
			Tier: str(a.Tier), Model: str(a.Model), Effort: str(a.Effort),
			Plan: str(a.Plan), MCP: str(a.MCP), Shape: str(a.Shape), Spec: str(a.Spec),
			Draft: a.Draft, Title: a.Title,
		}}
	case "list":
		return hold.Request{Action: action, JSON: h.List.JSON, Crew: str(h.List.Crew)}
	case "due":
		return hold.Request{Action: action, JSON: h.Due.JSON, Crew: str(h.Due.Crew)}
	case "park":
		return hold.Request{Action: action, Crew: str(h.Park.Crew), Default: h.Park.Default}
	default:
		return hold.Request{Action: action, Crew: str(h.Release.Crew), ID: h.Release.ID}
	}
}

// guardRequest is the typed git-guard call: the op is the word after
// `git-guard` in the command kong selected.
func guardRequest(p crewArgs) gitguard.GuardRequest {
	g := p.typed.GitGuard
	req := gitguard.GuardRequest{Op: strings.Fields(p.cmd)[1]}
	switch req.Op {
	case "check":
		req.Dir, req.Gitdir = g.Check.Common, g.Check.Gitdir
	case "check-cwd":
		req.Dir = g.CheckCwd.Common
	case "init":
		req.Dir = g.Init.Common
	case "commondir":
		req.Dir = g.Commondir.Common
	case "pairs":
		req.Dir = g.Pairs.Common
	case "union":
		req.Dir = g.Union.Common
	case "show-url":
		req.URL, req.Mode = g.ShowURL.URL, g.ShowURL.Mode
	case "show-key":
		req.Key = g.ShowKey.Key
	case "show-pair":
		req.Key, req.Value, req.Aliases = g.ShowPair.Key, g.ShowPair.Value, g.ShowPair.Aliases
	case "canon":
		req.Root, req.Rec = g.Canon.Root, g.Canon.Rec
	}
	return req
}

// stallRequest is the typed stall-watch call.
func stallRequest(p crewArgs) stall.Request {
	c := p.typed.StallWatch
	return stall.Request{
		Target: c.Target, Pane: c.Pane, Engine: c.Engine, Grace: c.Grace, Interval: c.Interval,
		Release: c.Release, BudgetRefresh: c.BudgetRefresh, Stall: c.Stall, Window: c.Window,
		Idle: c.Idle, Dead: c.Dead, MaxLife: c.MaxLife, Load: c.Load, BgWait: c.BgWait,
		Launch: c.Launch, Unread: c.Unread, RunawayHits: c.RunawayHits, RunawayTokens: c.RunawayTokens,
		NoBudget: c.NoBudget, NoNudge: c.NoNudge,
	}
}

// env is everything run reads from the process, so tests can supply their own.
type env struct {
	getwd    func() (string, error)
	jqColors string // $JQ_COLORS
	color    bool   // stdout is a terminal and $NO_COLOR is unset
	probes   func(ctx context.Context, cwd string) roster.Probes
	procs    crews.Probes // the pid probes crews reads
}

func main() {
	// The entry path crew.sh used to export for its children, derived from how
	// this process was invoked when its caller left it unset.
	entry.EnsureSelf(entry.DefaultSelfProbes(), os.Args[0])
	e := env{
		getwd:    os.Getwd,
		jqColors: os.Getenv("JQ_COLORS"),
		color:    bus.IsTerminal(1) && os.Getenv("NO_COLOR") == "",
		probes:   probes,
		procs:    crews.DefaultProbes(),
	}
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, e))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if code, routed := routeHelp(sub, args, stdout, stderr); routed {
		return code
	}
	parsed, ok := parseCrew(args)
	if !ok {
		// Bare `crew` and an unknown subcommand: the grouped list on stderr, not
		// the one 2,000-character line (#812). Exit is 1 whatever it printed.
		_ = entry.RunHelp(nil, stderr, stderr)
		return exitFailure
	}
	sub, args = parsed.sub, parsed.argv

	// fleet and rate --sweep-all read every repo on the machine, so they run
	// before Locate and outside any repo, and their refusals print before it too.
	switch sub {
	case "fleet":
		if parsed.refusal != nil {
			return refuse(stderr, parsed.refusal)
		}
		return runFleet(ctx, parsed, stdout, stderr, e)
	case "rate":
		// --sweep-all resolves the repos it discovers and reads neither paths nor
		// cwd; the sweep and --report fall through to the Locate below, which is
		// the outside-a-repo refusal the bash preamble kept for them.
		if parsed.typed.Rate.SweepAll || (parsed.refusal != nil && slices.ContainsFunc(parsed.argv, isSweepAll)) {
			if parsed.refusal != nil {
				return refuse(stderr, parsed.refusal)
			}
			return rate.Run(rateRequest(parsed.typed.Rate), bus.Paths{}, "", stdout, stderr, rate.Options{})
		}
	}

	// occupants reads tmux and a path it is given, so it needs no repo.
	if sub == "occupants" {
		return reap.RunOccupants(ctx, parsed.typed.Occupants.Args, stdout, stderr, reap.DefaultProbes(), os.Getenv("TMUX_PANE"))
	}

	// help, id, new, engine-cmd, dash and pi-agent-dir are the arms crew.sh runs ahead of its
	// repo check: no bus, and no working directory either — a worktree reaped out
	// from under a live shell leaves bash printing a shell-init warning and
	// answering anyway, and dispatch-resume reads `engine-cmd`'s exit as a verdict.
	if sub == "id" {
		// getwd failing is bash's `git rev-parse … || true` evidence: no toplevel,
		// so the env answers.
		dir, _ := e.getwd()
		return entry.RunID(stdout, stderr, entry.IDOptions{
			CrewID: func() string { return bus.CrewID(ctx, dir) },
		})
	}
	switch sub {
	case "help":
		return entry.RunHelp(parsed.typed.Help.Args, stdout, stderr)
	case "new":
		return entry.RunNew(stdout, entry.NewOptions{Now: time.Now, PID: os.Getpid()})
	case "engine-cmd":
		return entry.RunEngineCmd(parsed.typed.EngineCmd.Args, stderr)
	case "dash":
		return entry.RunDash(args, stderr, entry.DefaultDashOptions())
	case "pi-agent-dir":
		return piagentdir.Run(stdout, stderr, piagentdir.DefaultOptions())
	}

	cwd, err := e.getwd()
	if err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}

	// The git-config guard takes explicit dirs and git-baseline finds its own
	// common dir, so neither reads the bus. getwd keeps a logical $PWD that
	// names the cwd: refusals print the path the caller typed.
	switch sub {
	case "git-guard", "git-baseline":
		if parsed.refusal != nil {
			return refuse(stderr, parsed.refusal)
		}
		if sub == "git-guard" {
			return gitguard.RunGuard(guardRequest(parsed), cwd, stdout, stderr)
		}
		return gitguard.RunBaseline(parsed.typed.GitBaseline.Accept, cwd, gitguard.BaselineOptions{
			IsTTY: func() bool { return bus.IsTerminal(0) },
			In:    os.Stdin,
		}, stdout, stderr)
	}

	paths, err := bus.Locate(ctx, cwd)
	if err != nil {
		say(stderr, "crew: not in a git repo\n")
		return exitFailure
	}

	if parsed.refusal != nil {
		return refuse(stderr, parsed.refusal)
	}

	// reap prints its lines as it goes and runs the ratings sweep as a detached
	// child of this binary, so it writes straight to stdout like stream.
	if sub == "reap" || sub == "reap-autosweep" {
		self, _ := os.Executable()
		o := reap.Options{
			Probes: reap.DefaultProbes(),
			Getenv: os.Getenv,
			PID:    os.Getpid(),
			Self:   self,
			RateRun: func(_ []string, p bus.Paths, c string, so, se io.Writer) int {
				return rate.Run(rate.Request{}, p, c, so, se, rate.Options{})
			},
		}
		if sub == "reap-autosweep" {
			return reap.RunAutosweep(paths, cwd, o)
		}
		c := parsed.typed.Reap
		return reap.Run(ctx, reap.Request{
			Quiet: c.Quiet, DryRun: c.DryRun, NoWait: c.NoWait, Idle: str(c.Idle), Discard: str(c.Discard),
		}, paths, cwd, stdout, stderr, o)
	}

	// stack cut/align read the bus and the anchored git of the repo they are
	// called in, and change the process's cwd to the trusted one first, like reap.
	if sub == "stack" {
		st := parsed.typed.Stack
		req := stack.Request{Record: st.Cut.Record, Child: st.Cut.Child, Oid: st.Cut.Oid, Top: st.Align.Top}
		return stack.Run(ctx, req, paths, cwd, stdout, stderr, stack.Options{Probes: stack.DefaultProbes()})
	}

	// crews emits TSV, reads the log with its own torn-line tolerance, and
	// --mine never reads it at all, so it stays off the shared fold path.
	if sub == "crews" {
		out := bufio.NewWriterSize(stdout, 64<<10)
		_, colorsOK := jsonv.ParseJQColors(e.jqColors)
		code := crews.Run(parsed.typed.Crews.Mine, paths, out, stderr, crews.Options{
			Probes:          e.procs,
			JQColorsInvalid: !colorsOK,
			Now:             time.Now,
		})
		return flush(out, stderr, code)
	}

	// inbox prints lines too, but it reads its own arguments (the arm's crew and
	// --since ordering) and raises the delivered marks of whatever it printed,
	// so it owns its run rather than sharing log's fold call.
	if sub == "inbox" {
		out := bufio.NewWriterSize(stdout, 64<<10)
		_, colorsOK := jsonv.ParseJQColors(e.jqColors)
		in := parsed.typed.Inbox
		code := inbox.Run(inbox.Request{
			Me: in.Agent, Crew: in.Crew, Since: str(in.Since), From: str(in.From), Undelivered: in.Undelivered,
		}, paths, out, stderr, inbox.Options{
			JQColorsInvalid: !colorsOK,
			CrewID:          func() string { return bus.CrewID(ctx, cwd) },
			Flush:           out.Flush,
		})
		return flush(out, stderr, code)
	}

	// await blocks on the bus with its own clock (`CREW_CLOCK`), and raises the
	// delivered marks of whatever it printed once that is on the wire, so it owns
	// its run like inbox does.
	if sub == "await" {
		aw := parsed.typed.Await
		out := bufio.NewWriterSize(stdout, 64<<10)
		_, colorsOK := jsonv.ParseJQColors(e.jqColors)
		code := await.Run(await.Request{Me: aw.Agent, From: str(aw.From), Timeout: str(aw.Timeout), Interval: str(aw.Interval)}, paths, out, stderr, await.Options{
			CrewID:          func() string { return bus.CrewID(ctx, cwd) },
			Flush:           out.Flush,
			JQColorsInvalid: !colorsOK,
			Clock:           clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")},
		})
		return flush(out, stderr, code)
	}

	// watch parks on the bus with its own clock and its own lock, and prints one
	// batch value rather than lines, so it owns its run like await does. Its
	// cursor file moves once that value is on the wire.
	if sub == "watch" {
		w := parsed.typed.Watch
		out := bufio.NewWriterSize(stdout, 64<<10)
		_, colorsOK := jsonv.ParseJQColors(e.jqColors)
		code := watch.Run(watch.Request{
			Since: w.Since, States: str(w.States), Timeout: str(w.Timeout), Interval: str(w.Interval), Crew: str(w.Crew),
		}, paths, out, stderr, watch.Options{
			CrewID:          func() string { return bus.CrewID(ctx, cwd) },
			Flush:           out.Flush,
			Now:             time.Now,
			JQColorsInvalid: !colorsOK,
		})
		return flush(out, stderr, code)
	}

	// stream runs for as long as the lane lives and prints one protocol line at
	// a time, so it writes straight to stdout: a buffered line would wait for a
	// batch that may never come. It re-enters the crew script for its children,
	// which is why it takes the environment's CREW_SELF through Options.
	if sub == "stream" {
		return stream.Run(streamRequest(parsed.typed.Stream.streamFlags, parsed.typed.Stream.ReapEvery), paths, stdout, stderr, stream.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Now:    time.Now,
			Probes: e.procs,
		})
	}

	// roster-render is the per-crew diagram daemon. It replaces the process for a
	// newer installed build, so it takes the environment's CREW_SELF as its own
	// identity the way stream takes it for its children.
	if sub == "roster-render" {
		rr := parsed.typed.RosterRender
		req := rosterrender.Request{
			Crew: str(rr.Crew), Pane: str(rr.Pane), Interval: str(rr.Interval), Quiet: str(rr.Quiet),
			NoOpen: rr.NoOpen, Once: rr.Once, Detach: rr.Detach,
		}
		return rosterrender.Run(ctx, req, paths, rosterrender.Options{
			Probes:    rosterrender.Default(ctx),
			Procs:     e.procs,
			Roster:    renderProbes(ctx, paths.Common),
			Clock:     clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")},
			Stderr:    stderr,
			RosterDir: os.Getenv("CREW_ROSTER_DIR"),
			Self:      os.Getenv("CREW_SELF"),
			StartPath: os.Getenv("CREW_RR_START_PATH"),
			Path:      os.Getenv("PATH"),
			PID:       os.Getpid(),
		})
	}

	// hold reads its own action and flags, prints either TSV or one JSON value,
	// and appends to the bus itself, so it owns its run like inbox does.
	if sub == "hold" {
		_, colorsOK := jsonv.ParseJQColors(e.jqColors)
		return hold.Run(holdRequest(parsed), paths, stdout, stderr, hold.Options{
			JQColorsInvalid: !colorsOK,
			CrewID:          func() string { return bus.CrewID(ctx, cwd) },
			Now:             time.Now,
			CrewClock:       os.Getenv("CREW_CLOCK"),
			Sender:          senderProbes(ctx, e.procs),
		})
	}

	// retro folds the whole bus with no crew filter, and prints the TSV rows, the
	// padded report or one JSON object, so it owns its run like hold does.
	if sub == "retro" {
		return retro.Run(retro.Request{Report: parsed.typed.Retro.Report, JSON: parsed.typed.Retro.JSON}, paths, stdout, stderr)
	}

	// rate's two repo-keyed modes: the sweep of this repo, and the store rollup.
	if sub == "rate" {
		return rate.Run(rateRequest(parsed.typed.Rate), paths, cwd, stdout, stderr, rate.Options{})
	}

	// reply resolves its target against the bus, then appends one row itself, so
	// it owns its run like hold does — and it prints nothing on success.
	if sub == "reply" {
		c := parsed.typed.Reply
		return reply.Run(reply.Request{Crew: str(c.Crew), To: c.To, Body: c.Body}, paths, stderr, reply.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Clock:  clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")},
			Probes: e.procs,
			Sender: senderProbes(ctx, e.procs),
		})
	}

	// nudge resolves its own target, runs its own frame checks and posts its own rows,
	// its unread scan in process through internal/unread (docs/crew-go-port.md).
	if sub == "nudge" {
		n := parsed.typed.Nudge
		req, _ := nudge.NewRequest(str(n.Crew), n.Target, n.Wait)
		return nudge.Run(ctx, req, paths, stdout, stderr,
			nudge.Default(paths, stderr, os.Getpid(), func() string { return bus.CrewID(ctx, cwd) }))
	}

	// The two target readers own their run the way hold does: neither is a fold
	// over every event.
	if sub == "resolve-target" {
		return resolve.Run(resolve.Request{Crew: str(parsed.typed.Resolve.Crew), Target: parsed.typed.Resolve.Target}, paths, stdout, stderr)
	}
	// pr-watch runs the standalone pr-watch binary as a child and posts the event it
	// printed, so it owns its run the way watch does rather than folding the bus.
	if sub == "pr-watch" {
		c := parsed.typed.PRWatch
		return prwatch.Run(ctx, prwatch.Request{PR: c.PR, Repo: c.Repo, Timeout: c.Timeout, Interval: c.Interval}, paths, stdout, stderr, prwatch.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Clock:  clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")},
		})
	}
	if sub == "where" {
		return where.Run(where.Request{Crew: str(parsed.typed.Where.Crew), Target: parsed.typed.Where.Target}, paths, stdout, stderr, where.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Probes: whereProbes(ctx),
		})
	}
	// identity answers the codename a branch works under. Its one tmux read is
	// best-effort, unlike whereProbes', which turns a failure into a refusal.
	if sub == "identity" {
		id := parsed.typed.Identity
		return identity.Run(identity.Request{Branch: id.Branch, Crew: id.Crew, Hash: str(id.Hash)}, paths, stdout, stderr, identity.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Probes: identity.Probes{Windows: func() (string, error) {
				return tmuxOutput(ctx, "list-windows", "-a", "-F", "#{@crew_name}")
			}},
		})
	}

	// adopt records the caller's own identity in pidfile.log: `$$`, `$PPID` and
	// `$PWD`, which crew.sh's exec leaves ours.
	if sub == "adopt" {
		c := parsed.typed.Adopt
		return adopt.Run(adopt.Request{ID: c.ID, PID: c.PID, Force: c.Force}, paths, stdout, stderr, adopt.Options{
			Probes: e.procs,
			Now:    time.Now,
			PID:    os.Getpid(),
			PPID:   os.Getppid(),
			Pwd:    cwd,
		})
	}

	// status and msg append one row each as the caller; the stamp is real time,
	// never CREW_CLOCK, as jq's `now` was.
	if sub == "status" || sub == "msg" {
		// msg's words are data: the first three are from, to and body, the rest ignored.
		words := parsed.typed.Msg.Args
		word := func(i int) string {
			if i < len(words) {
				return words[i]
			}
			return ""
		}
		o := status.Options{
			CrewID:   func() string { return bus.CrewID(ctx, cwd) },
			Toplevel: func() string { return bus.Toplevel(ctx, cwd) },
			NowMS:    clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")}.RealMS,
			Getenv:   os.Getenv,
			Sender:   senderProbes(ctx, e.procs),
		}
		if _, err := exec.LookPath("tmux"); err == nil {
			o.Tmux = func(args ...string) (string, error) { return tmuxOutput(ctx, args...) }
		}
		if sub == "status" {
			c := parsed.typed.Status
			return status.RunStatus(status.StatusRequest{From: c.From, State: c.State, Detail: c.Detail, PR: c.PR, Restamp: c.Restamp}, paths, stderr, o)
		}
		return status.RunMsg(status.MsgRequest{From: word(0), To: word(1), Body: word(2)}, paths, stderr, o)
	}

	// register and deregister record the caller's own identity like adopt does.
	if sub == "register" || sub == "deregister" {
		return register.Run(sub, parsed.typed.Register.PID, paths, stderr, register.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Probes: e.procs,
			Now:    time.Now,
			PID:    os.Getpid(),
			PPID:   os.Getppid(),
			Pwd:    cwd,
			Args:   adopt.PsArgs,
			Getenv: os.Getenv,
		})
	}

	// stall-watch is a long-lived watch with its own flags: it reads the bus and
	// the pane itself, and SIGTERM/SIGINT end it through ctx so the budget
	// refresh lock is released.
	if sub == "stall-watch" {
		ctx, stop := stall.NotifySignals(ctx)
		defer stop()
		return stall.Run(ctx, stallRequest(parsed), paths, stderr, stall.Options{
			// Anchor, don't discover: dispatch hands a watchdog its crew in the
			// env, and its cwd is a worktree whose WORKER_TASK.md the watched
			// worker can rewrite.
			CrewID: func() string { return bus.CrewIDEnvFirst(ctx, cwd) },
			Clock:  clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")},
			Sender: senderProbes(ctx, e.procs),
			NewProbes: func(pane string) probe.Probes {
				return probe.Default(probe.Env{
					Getenv: os.Getenv, Pane: pane, Dir: paths.Common, Stderr: stderr,
					Bash:           bashBin,
					BudgetLib:      cmp.Or(os.Getenv("BUDGET_GATE_LIB"), budgetGateLib),
					LocalModelsLib: cmp.Or(os.Getenv("LOCAL_MODELS_LIB"), localModelsLib),
				})
			},
			Release: func(ctx context.Context, branch, session, state string, tsMS, grace int64) (int, error) {
				return reap.ReleaseWindows(ctx, reap.Deps{
					Common: paths.Common, Log: paths.Log,
					Say: func(string) {}, Note: func(string) {},
					P: reap.DefaultProbes(),
				}, branch, session, state, tsMS, grace, false)
			},
			BudgetFile: budgetFile(),
			PID:        os.Getpid(),
		})
	}

	// log and report print lines rather than one JSON value, and each reads the
	// bus with its arm's tolerance (log keeps jq -c's torn-tail prefix, report
	// has none), so they own their read too.
	if sub == "log" || sub == "report" {
		crew := parsed.typed.Log.Crew
		if sub == "report" {
			crew = parsed.typed.Report.Crew
		}
		if crew == "" {
			crew = bus.CrewID(ctx, cwd)
		}
		out := bufio.NewWriterSize(stdout, 64<<10)
		palette, colorsOK := jsonv.ParseJQColors(e.jqColors)
		var code int
		if sub == "log" {
			opts := log.Options{JQColorsInvalid: !colorsOK}
			if e.color {
				opts.Colors = &palette
			}
			code = log.Run(crew, paths, out, stderr, opts)
		} else {
			code = report.Run(crew, paths, out, stderr, report.Options{
				JQColorsInvalid: !colorsOK,
				Now:             time.Now,
			})
		}
		return flush(out, stderr, code)
	}

	var (
		branch, crew string
		pretty       bool
	)
	if sub == "sessions" {
		branch, crew = parsed.typed.Sessions.Branch, str(parsed.typed.Sessions.Crew)
	} else {
		pretty = true
		crew = cmp.Or(parsed.typed.Roster.Crew, bus.CrewID(ctx, cwd))
	}

	out := bufio.NewWriterSize(stdout, 64<<10)
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		if sub == "sessions" {
			say(out, "[]\n")
		}
		return flush(out, stderr, 0)
	}
	// From here jq would have started, so it warns about a bad $JQ_COLORS even
	// when it then fails to open the log.
	palette, valid := jsonv.ParseJQColors(e.jqColors)
	if !valid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	opts := jsonv.Options{Indent: pretty}
	if e.color {
		opts.Colors = &palette
	}

	raw, code := fold(sub, events, err, paths.Log, stderr, func(evs []jsonv.Value) (jsonv.Value, error) {
		now := time.Now()
		nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6
		if sub == "sessions" {
			docs := make([]any, len(evs))
			for i, ev := range evs {
				docs[i] = jsonv.ToAny(ev)
			}
			folded, err := sessions.Fold(docs, branch, crew, nowSec)
			if err != nil {
				return jsonv.Value{}, err
			}
			b, err := sessions.JSON(folded)
			if err != nil {
				return jsonv.Value{}, err
			}
			outs, err := jsonv.DecodeStream(bytes.NewReader(b))
			if err != nil || len(outs) != 1 {
				return jsonv.Value{}, fmt.Errorf("fold result: %v", err)
			}
			return outs[0], nil
		}
		return roster.Fold(evs, crew, nowSec, e.probes(ctx, cwd))
	})
	if code == exitOpen && sub == "sessions" {
		// jq slurps zero inputs when it cannot open the file, so the program
		// runs on [] and prints its result before exiting 2.
		raw = jsonv.Array()
	} else if code != 0 {
		return code
	}

	if err := jsonv.Encode(out, raw, opts); err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	say(out, "\n")
	if sub == "sessions" && code == 0 {
		say(out, "\n") // the arm's `printf '\n'` after jq -c
	}
	return flush(out, stderr, code)
}

// originOf is a git common dir's origin URL, or "" when it has none.
func originOf(ctx context.Context, common string) string {
	out, err := exec.CommandContext(ctx, "git", "--git-dir="+common, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(out), "\n")
}

// streamRequest is the typed stream call; an absent flag is "", the default.
func streamRequest(f streamFlags, reapEvery *string) stream.Request {
	return stream.Request{
		Crew: str(f.Crew), States: str(f.States), Park: str(f.Park), Heartbeat: str(f.Heartbeat),
		Coalesce: str(f.Coalesce), Retry: str(f.Retry), Interval: str(f.Interval), ReapEvery: str(reapEvery),
		Force: f.Force, Status: f.Status,
	}
}

func runFleet(ctx context.Context, p crewArgs, stdout, stderr io.Writer, e env) int {
	// The lane prints one protocol line at a time for as long as it lives, so it
	// writes straight to stdout like stream.
	if strings.HasPrefix(p.cmd, "fleet stream") {
		s := p.typed.Fleet.Stream
		req := stream.FleetRequest{Request: streamRequest(s.streamFlags, nil), Rediscover: str(s.Rediscover)}
		return stream.RunFleet(req, stdout, stderr, stream.FleetOptions{
			Options: stream.Options{Now: time.Now, Probes: e.procs},
			Origin:  func(common string) string { return originOf(ctx, common) },
		})
	}
	o := fleet.Options{
		Panes: func() string { return probes(ctx, "").Panes() },
		Windows: func() string {
			out, _ := tmuxOutput(ctx, "list-windows", "-a", "-F", "#{@crew_branch}")
			return out
		},
		Worktrees: func(common string) (string, error) {
			return worktreeList(exec.CommandContext(ctx, "git", "--git-dir="+common, "worktree", "list", "--porcelain"))
		},
		Origin: func(common string) string { return originOf(ctx, common) },
		Now:    time.Now,
	}
	if palette, _ := jsonv.ParseJQColors(e.jqColors); e.color {
		o.Colors = &palette
	}
	c := p.typed.Fleet.Table
	return fleet.Run(fleet.Request{All: c.All, JSON: c.JSON, Crew: str(c.Crew)}, stdout, stderr, o)
}

// The Nix build sets these with -X: the pinned bash and the two libs
// stall-watch's budget and local-model probes source. BUDGET_GATE_LIB and
// LOCAL_MODELS_LIB override the libs, as they do for dispatch.sh.
var (
	bashBin        = "bash"
	budgetGateLib  string
	localModelsLib string
)

// budgetFile is `${XDG_DATA_HOME:-$HOME/.local/share}/crew/engine-budget.json`.
func budgetFile() string {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = os.Getenv("HOME") + "/.local/share"
	}
	return data + "/crew/engine-budget.json"
}

// say writes to a stream whose failure is reported elsewhere (out, through
// flush) or has nowhere to go (stderr).
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// flush writes out what is buffered and keeps code unless the write fails.
func flush(out *bufio.Writer, stderr io.Writer, code int) int {
	if err := out.Flush(); err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	return code
}

// refuse prints a typed subcommand's refusal and returns its status.
func refuse(stderr io.Writer, r *refusal) int {
	say(stderr, "%s\n", r.msg)
	return r.code
}

// rateRequest is the typed flags rate.Run takes.
func rateRequest(c rateCmd) rate.Request {
	roots := make([]string, len(c.Root))
	for i, d := range c.Root {
		roots[i] = *d
	}
	return rate.Request{Report: c.Report, JSON: c.JSON, Pooled: c.Pooled, SweepAll: c.SweepAll, Roots: roots}
}

// fold runs the subcommand's fold over the read events, or maps the read or
// fold error to its stderr line and exit status.
func fold(sub string, events []bus.Event, readErr error, log string, stderr io.Writer, f func([]jsonv.Value) (jsonv.Value, error)) (jsonv.Value, int) {
	var v jsonv.Value
	err := readErr
	if err == nil {
		raws := make([]jsonv.Value, len(events))
		for i, e := range events {
			raws[i] = e.Raw
		}
		v, err = f(raws)
	}
	if err == nil {
		return v, 0
	}
	var exit *roster.ExitError
	if errors.As(err, &exit) {
		say(stderr, "%s", exit.Stderr)
		return v, exit.Code
	}
	// Every other outcome — a bus it cannot read, or a fold that failed on a row
	// it cannot use — maps through bus.JQFailure to jq's message and status for
	// the same file (2 unreadable, 5 corrupt or type error).
	msg, code := bus.JQFailure(err)
	say(stderr, "crew: %s: %s: %v\n", sub, log, msg)
	return v, code
}

// whereProbes is the arm's two tmux reads, argv and all: a failed read is
// `where`'s own refusal rather than "the target is gone".
func whereProbes(ctx context.Context) where.Probes {
	return where.Probes{
		Windows: func() (string, error) {
			return tmuxOutput(ctx, "list-windows", "-a", "-F",
				"#{window_id}\t#{@crew_branch}\t#{@crew_dir}\t#{@crew_id}\t#{@crew_name}\t#{session_name}\t#{window_index}\t#{window_name}")
		},
		Panes: func() (string, error) {
			return tmuxOutput(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{@crew_role}\t#{pane_index}")
		},
	}
}

// tmuxOutput is `$(tmux args… 2>/dev/null)`: stdout, stderr dropped, and a
// non-zero exit kept, because the two arms that read tmux refuse on it.
func tmuxOutput(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// senderProbes are the real reads behind the sender check: the caller's env, its
// pid ancestry, and every pane's stamps when tmux is on PATH.
func senderProbes(ctx context.Context, procs crews.Probes) sender.Probes {
	p := sender.Probes{Getenv: os.Getenv, Parent: procs.Parent, PID: os.Getpid()}
	if _, err := exec.LookPath("tmux"); err == nil {
		p.Panes = func() (string, error) { return tmuxOutput(ctx, "list-panes", "-a", "-F", sender.Format) }
	}
	return p
}

func probes(ctx context.Context, cwd string) roster.Probes {
	return roster.Probes{
		Panes: func() string {
			out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_current_command} #{pane_current_path}").Output()
			if err != nil {
				return ""
			}
			return string(out)
		},
		Worktrees: func() (string, error) {
			cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
			cmd.Dir = cwd
			return worktreeList(cmd)
		},
	}
}

// renderProbes is the renderer's row fold, anchored at the common dir rather
// than at the caller's directory: `roster-render` moves out of the worktree it
// was started in, because a reap can take that directory away while the daemon
// lives, and a worktree list that dies with it says nothing about who is gone.
func renderProbes(ctx context.Context, common string) roster.Probes {
	p := probes(ctx, "")
	p.Worktrees = func() (string, error) {
		return worktreeList(exec.CommandContext(ctx, "git", "--git-dir="+common, "worktree", "list", "--porcelain"))
	}
	return p
}

// worktreeList runs a `git worktree list --porcelain` command, mapping a
// failure to the roster arm's death with git's status and stderr.
func worktreeList(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return string(out), nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return "", &roster.ExitError{Code: 127, Stderr: "git: command not found\n"}
	}
	return "", &roster.ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
}
