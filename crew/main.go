// Command crew-go is the crew entrypoint: it routes `--help` and every
// subcommand, and reports bare `crew` and an unknown one as the grouped help on
// stderr. flake.nix installs it as `crew` behind a PATH wrapper that keeps
// argv[0].
package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/noamsto/dispatcher/crew/internal/adopt"
	"github.com/noamsto/dispatcher/crew/internal/await"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/classes"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/entry"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/followups"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/inbox"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/log"
	"github.com/noamsto/dispatcher/crew/internal/merge"
	"github.com/noamsto/dispatcher/crew/internal/nudge"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/piagentdir"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/prwatch"
	"github.com/noamsto/dispatcher/crew/internal/rate"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/register"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/reply"
	"github.com/noamsto/dispatcher/crew/internal/report"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
	"github.com/noamsto/dispatcher/crew/internal/retro"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/rosterrender"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
	"github.com/noamsto/dispatcher/crew/internal/speclint"
	"github.com/noamsto/dispatcher/crew/internal/stack"
	"github.com/noamsto/dispatcher/crew/internal/stall"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
	"github.com/noamsto/dispatcher/crew/internal/status"
	"github.com/noamsto/dispatcher/crew/internal/stream"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/watch"
	"github.com/noamsto/dispatcher/crew/internal/where"
)

const (
	sessionsUsage = "crew: sessions <branch> [--crew ID]"
	exitFailure   = 1
	exitOpen      = 2 // sessions prints [] where jq slurps zero inputs
	exitUsage     = 64
	// exitRemoteUsage is the refusal status of the remote-* transport commands.
	exitRemoteUsage = 2
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
	case "class":
		if isHelpFlag(arg(args, 2)) {
			return entry.RunHelp([]string{"class"}, stdout, stderr), true
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

// subcommands is every word crew admits: the forty the grammar types, then
// dash, which parses its own argv. It is the list the switch of #821 admitted,
// and crew/help_cli_test.go reads this literal to prove every subcommand has its own
// row in the grouped help; TestAdmittedSubcommands pins it against that
// revision and against what the grammar really admits.
var subcommands = []string{
	"id", "new", "engine-cmd", "occupants",
	"where", "identity", "resolve-target", "crews", "register", "deregister", "log", "sessions", "inbox",
	"roster", "report", "retro", "rate", "fleet", "stack", "roster-render", "pi-agent-dir",
	"adopt", "await", "dash", "git-baseline", "merge",
	"git-guard", "help", "hold", "msg", "nudge",
	"pr-watch", "reap", "reap-autosweep",
	"relay", "reply", "stall-watch", "status", "stream", "watch",
	"remote-run", "remote-exec", "dispatch-row", "peer-sync", "peer-claims", "decide", "decisions", "spec-lint", "followups", "class",
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
	"nudge": true, "await": true, "watch": true, "relay": true, "stream": true,
	"git-guard": true, "git-baseline": true, "reap": true, "reap-autosweep": true,
	"adopt": true, "merge": true, "pr-watch": true, "stall-watch": true, "help": true,
	"remote-run": true, "remote-exec": true, "dispatch-row": true, "peer-sync": true, "peer-claims": true, "decide": true, "decisions": true, "spec-lint": true,
	"followups": true, "class": true,
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
	Log        logCmd        `cmd:"" name:"log" help:"The crew's bus rows, invisible code points escaped (the bus log stays raw)."`
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
	Relay  relayCmd  `cmd:"" name:"relay" help:"Follow this bus and print watch batches, for a remote dispatcher."`
	Stream streamCmd `cmd:"" name:"stream" help:"The long-lived lane that pushes batches."`

	RemoteRun   remoteRunCmd   `cmd:"" name:"remote-run" help:"Run a command in the clone on a remote host."`
	RemoteExec  remoteExecCmd  `cmd:"" name:"remote-exec" help:"The ssh-side of remote-run: serve one request."`
	DispatchRow dispatchRowCmd `cmd:"" name:"dispatch-row" help:"The dispatch row of one session."`
	PeerSync    peerSyncCmd    `cmd:"" name:"peer-sync" help:"The ssh side of a peer sync: one exchange on stdin."`
	PeerClaims  peerClaimsCmd  `cmd:"" name:"peer-claims" help:"Print a peer host's live claim on an issue."`
	Decide      decideCmd      `cmd:"" name:"decide" help:"Announce a dispatcher decision to the peer hosts."`
	Decisions   decisionsCmd   `cmd:"" name:"decisions" help:"List the decisions announced here and by peer hosts."`
	Class       classCmd       `cmd:"" name:"class" help:"Revert or promote a model class default."`

	SpecLint specLintCmd `cmd:"" name:"spec-lint" help:"Lint a dispatcher task spec."`

	GitGuard    gitGuardCmd    `cmd:"" name:"git-guard" help:"The git-config guard worktree-git.sh runs."`
	GitBaseline gitBaselineCmd `cmd:"" name:"git-baseline" help:"List or accept the git-config baseline."`

	Adopt adoptCmd `cmd:"" name:"adopt" help:"Re-attach to a crew already on disk."`

	Merge mergeCmd `cmd:"" name:"merge" help:"Merge a PR only when head, base and ledger are green."`

	Followups followupsCmd `cmd:"" name:"followups" help:"Plan or apply a PR's follow-up batch."`

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
	getwd  func() (string, error)
	probes func(ctx context.Context, cwd string) roster.Probes
	proc   proc.Table  // the process table the pid reads go through
	stdin  io.Reader   // remote-exec's request
	clock  clock.Clock // the fold's clock (jq's `now`); nil Now is the wall clock
	// peerHosts is the settings loader behind every peer read; nil reads no peer.
	peerHosts func() (map[string]remote.Host, error)
}

// now is the env clock when one is set, else the wall clock.
func (e env) now() time.Time {
	if e.clock.Now != nil {
		return e.clock.Now()
	}
	return time.Now()
}

// nowSec is jq's `now`: the env clock when one is set, in fractional seconds.
func (e env) nowSec() float64 {
	now := e.now()
	return float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6
}

func main() {
	// The entry path crew.sh used to export for its children, derived from how
	// this process was invoked when its caller left it unset.
	entry.EnsureSelf(entry.DefaultSelfProbes(), os.Args[0])
	e := env{
		getwd:  os.Getwd,
		probes: probes,
		proc:   proc.OS{},
		stdin:  os.Stdin,
		peerHosts: func() (map[string]remote.Host, error) {
			return remote.Load(context.Background(), remote.ConfigBin(), io.Discard)
		},
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
	case "class":
		// A worker is refused before anything is located or read.
		if line := classes.WorkerRefusal(os.Getenv); line != "" {
			say(stderr, "%s\n", line)
			return exitFailure
		}
	}

	// remote-exec is what sshd starts in the remote user's $HOME, so it runs
	// before the working directory and the repo are read: Serve changes into the
	// clone its request names and never trusts its own cwd.
	if sub == "remote-exec" {
		if parsed.refusal != nil {
			return refuse(stderr, parsed.refusal)
		}
		return remote.Serve(e.stdin, stdout, stderr, remote.DefaultServeOptions())
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
		return entry.RunNew(stdout, entry.NewOptions{Clock: clock.FromEnv(), PID: os.Getpid()})
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
			PID:    os.Getpid(),
			Self:   self,
			RateRun: func(_ []string, p bus.Paths, c string, so, se io.Writer) int {
				return rate.Run(rate.Request{}, p, c, so, se, rate.Options{
					ClassCheck: func(rows []any, w io.Writer) { classes.Check(rows, classEnv(ctx, c, p), w) },
				})
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
		code := crews.Run(parsed.typed.Crews.Mine, paths, out, stderr, crews.Options{
			Proc:  e.proc,
			Clock: clock.FromEnv(),
		})
		return flush(out, stderr, code)
	}

	// inbox prints lines too, but it reads its own arguments (the arm's crew and
	// --since ordering) and raises the delivered marks of whatever it printed,
	// so it owns its run rather than sharing log's fold call.
	if sub == "inbox" {
		out := bufio.NewWriterSize(stdout, 64<<10)
		in := parsed.typed.Inbox
		code := inbox.Run(inbox.Request{
			Me: in.Agent, Crew: in.Crew, Since: str(in.Since), From: str(in.From), Undelivered: in.Undelivered,
		}, paths, out, stderr, inbox.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Flush:  out.Flush,
		})
		return flush(out, stderr, code)
	}

	// await blocks on the bus with its own clock (`CREW_CLOCK`), and raises the
	// delivered marks of whatever it printed once that is on the wire, so it owns
	// its run like inbox does.
	if sub == "await" {
		aw := parsed.typed.Await
		out := bufio.NewWriterSize(stdout, 64<<10)
		code := await.Run(await.Request{Me: aw.Agent, From: str(aw.From), Timeout: str(aw.Timeout), Interval: str(aw.Interval)}, paths, out, stderr, await.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Flush:  out.Flush,
			Clock:  clock.FromEnv(),
		})
		return flush(out, stderr, code)
	}

	// watch parks on the bus with its own clock and its own lock, and prints one
	// batch value rather than lines, so it owns its run like await does. Its
	// cursor file moves once that value is on the wire.
	if sub == "watch" {
		w := parsed.typed.Watch
		out := bufio.NewWriterSize(stdout, 64<<10)
		code := watch.Run(watch.Request{
			Since: w.Since, States: str(w.States), Timeout: str(w.Timeout), Interval: str(w.Interval), Crew: str(w.Crew),
		}, paths, out, stderr, watch.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Flush:  out.Flush,
			Clock:  clock.FromEnv(),
		})
		return flush(out, stderr, code)
	}

	// relay follows the bus for as long as its reader keeps the pipe open: it
	// holds no lock and writes no cursor, and ends quietly when a write fails.
	if sub == "relay" {
		r := parsed.typed.Relay
		out := bufio.NewWriterSize(stdout, 64<<10)
		// Relay flushes every line itself, and a failed flush is its exit 0.
		return watch.Relay(watch.RelayRequest{
			Since: r.Since, States: str(r.States), Interval: str(r.Interval), Heartbeat: str(r.Heartbeat), Crew: str(r.Crew),
		}, paths, out, stderr, watch.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Flush:  out.Flush,
			Clock:  clock.FromEnv(),
		})
	}

	// remote-run sends one command to the clone of this repo on a remote host.
	if sub == "remote-run" {
		c := parsed.typed.RemoteRun
		return remoteRun(ctx, e, remote.Call{
			Host: str(c.Host), Cwd: cwd, Env: c.Env, SpecFile: str(c.SpecFile), Argv: remoteArgv(c.Argv),
		}, stdout, stderr)
	}
	if sub == "dispatch-row" {
		c := parsed.typed.DispatchRow
		crew := cmp.Or(str(c.Crew), bus.CrewID(ctx, cwd))
		return remote.DispatchRow(paths.Log, crew, str(c.Session), stdout, stderr)
	}
	// peer-sync runs inside the clone remote-exec put it in, so the repo-located
	// paths are the clone's. The dialer quotes only stderr's first line, so
	// dispatch-config's own stderr must not come ahead of the refusal.
	if sub == "peer-sync" {
		return peer.Serve(e.stdin, stdout, stderr, peer.ServeOptions{
			Paths: paths,
			Hosts: func() (map[string]remote.Host, error) { return remote.Load(ctx, remote.ConfigBin(), io.Discard) },
		})
	}
	if sub == "peer-claims" {
		line, err := peer.Claims(peer.StoreFor(paths), parsed.typed.PeerClaims.Issue, time.Now())
		if err != nil {
			say(stderr, "crew: peer-claims: %v\n", err)
			return exitFailure
		}
		if line != "" {
			say(stdout, "%s\n", line)
		}
		return 0
	}

	if sub == "decide" {
		return status.RunDecide(ctx, parsed.typed.Decide.Text, paths, stderr, status.Options{
			Dir:       cwd,
			CrewID:    func() string { return bus.CrewID(ctx, cwd) },
			Clock:     clock.FromEnv(),
			Sender:    senderProbes(e.proc),
			Tmux:      tmux.Exec{},
			PeerHosts: e.peerHosts,
		})
	}
	if sub == "class" {
		ce := classEnv(ctx, cwd, paths)
		if strings.Fields(parsed.cmd)[1] == "revert" {
			c := parsed.typed.Class.Revert
			return classes.Revert(c.Engine, c.Class, ce, stderr)
		}
		c := parsed.typed.Class.Promote
		return classes.Promote(c.Engine, c.Class, c.ID, ce, stderr)
	}
	if sub == "decisions" {
		c := parsed.typed.Decisions
		now := e.now()
		since := now.Add(-decisionsWindow).UnixMilli()
		if c.All {
			since = 0
		} else if c.Since != nil {
			since, _ = parseSince(c.Since, now)
		}
		return printDecisions(paths, since, c.All, c.JSON, stdout, stderr)
	}

	// spec-lint reads a spec file and the repo's origin refs, never the bus.
	if sub == "spec-lint" {
		c := parsed.typed.SpecLint
		issues := make([]string, len(c.Issue))
		for i, n := range c.Issue {
			issues[i], _ = speclint.ParseIssue(n)
		}
		return speclint.Run(ctx, speclint.Request{File: c.File, Issues: issues, NoHostChecks: c.NoHostChecks}, cwd, stderr, speclint.DefaultOptions())
	}

	// stream runs for as long as the lane lives and prints one protocol line at
	// a time, so it writes straight to stdout: a buffered line would wait for a
	// batch that may never come. It re-enters the crew script for its children,
	// which is why it takes the environment's CREW_SELF through Options.
	if sub == "stream" {
		return stream.Run(streamRequest(parsed.typed.Stream.streamFlags, parsed.typed.Stream.ReapEvery), paths, stdout, stderr, stream.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Clock:  clock.FromEnv(),
			Proc:   e.proc,
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
			Probes:    rosterrender.Default(),
			Proc:      e.proc,
			Git:       git.Exec{},
			Clock:     clock.FromEnv(),
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
		return hold.Run(ctx, holdRequest(parsed), paths, stdout, stderr, hold.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Clock:  clock.FromEnv(),
			Sender: senderProbes(e.proc),
		})
	}

	// retro folds the whole bus with no crew filter, and prints the TSV rows, the
	// padded report or one JSON object, so it owns its run like hold does. --draft
	// is the one crew-filtered shape, and it reads its codenames off the roster's
	// own rows.
	if sub == "retro" {
		c := parsed.typed.Retro
		return retro.Run(retro.Request{Report: c.Report, JSON: c.JSON, Draft: c.Draft, All: c.All, Crew: str(c.Crew)}, paths, stdout, stderr, retro.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Roster: func(docs []any, crew string) []map[string]any {
				rows, err := roster.Fold(ctx, docs, crew, e.nowSec(), e.probes(ctx, cwd))
				if err != nil {
					// The draft still prints, but the names lose the roster's suffix.
					say(stderr, "crew: retro: roster: %v\n", err)
					return nil
				}
				return rows
			},
		})
	}

	// rate's two repo-keyed modes: the sweep of this repo, and the store rollup.
	if sub == "rate" {
		return rate.Run(rateRequest(parsed.typed.Rate), paths, cwd, stdout, stderr, rate.Options{
			ClassCheck: func(rows []any, w io.Writer) { classes.Check(rows, classEnv(ctx, cwd, paths), w) },
			ClassReport: func(rows []any) []string {
				return classes.ReportLines(rows, classEnv(ctx, cwd, paths))
			},
		})
	}

	// reply resolves its target against the bus, then appends one row itself, so
	// it owns its run like hold does — and it prints nothing on success.
	if sub == "reply" {
		c := parsed.typed.Reply
		crewID := func() string { return bus.CrewID(ctx, cwd) }
		return reply.Run(ctx, reply.Request{Crew: str(c.Crew), To: c.To, Body: c.Body}, paths, stderr, reply.Options{
			CrewID: crewID,
			Clock:  clock.FromEnv(),
			Proc:   e.proc,
			Sender: senderProbes(e.proc),
			Remote: func(host, crew string, argv []string) int {
				return remoteAs(ctx, e, cwd, crew)(host, argv, stdout, stderr)
			},
		})
	}

	// nudge resolves its own target, runs its own frame checks and posts its own rows,
	// its unread scan in process through internal/unread (docs/crew-go-port.md).
	if sub == "nudge" {
		n := parsed.typed.Nudge
		req, _ := nudge.NewRequest(str(n.Crew), n.Target, n.Wait)
		crewID := func() string { return bus.CrewID(ctx, cwd) }
		o := nudge.Default(paths, stderr, os.Getpid(), crewID)
		o.Remote = func(host string, argv []string) int {
			return remoteAs(ctx, e, cwd, cmp.Or(req.Crew, crewID()))(host, argv, stdout, stderr)
		}
		return nudge.Run(ctx, req, paths, stdout, stderr, o)
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
			Clock:  clock.FromEnv(),
		})
	}
	if sub == "followups" {
		c := parsed.typed.Followups
		return followups.Run(ctx, followups.Request{PR: c.PR, Entries: str(c.Entries), Apply: c.Apply, Draft: c.Draft, Crew: str(c.Crew), Tracker: str(c.Tracker)}, paths, stdout, stderr, followups.Options{GH: gh.Exec{}, Clock: clock.FromEnv()})
	}
	if sub == "merge" {
		c := parsed.typed.Merge
		return merge.Run(ctx, merge.Request{PR: c.PR, DryRun: c.DryRun, AllowNoCI: c.AllowNoCI, NoLedger: c.NoLedger, Crew: str(c.Crew)}, paths, stdout, stderr, merge.Options{GH: gh.Exec{}, Git: git.Exec{}, Dir: cwd, Reply: reply.Options{Clock: clock.FromEnv(), Proc: e.proc, Sender: senderProbes(e.proc)}})
	}
	if sub == "where" {
		w := parsed.typed.Where
		crewID := func() string { return bus.CrewID(ctx, cwd) }
		return where.Run(ctx, where.Request{Crew: str(w.Crew), Target: w.Target}, paths, stdout, stderr, where.Options{
			CrewID: crewID,
			Tmux:   tmux.Exec{},
			Remote: func(host, crew string, argv []string, out io.Writer) int {
				return remoteAs(ctx, e, cwd, crew)(host, argv, out, stderr)
			},
		})
	}
	// identity answers the codename a branch works under.
	if sub == "identity" {
		id := parsed.typed.Identity
		crew := id.Crew
		if id.CrewFlag != nil {
			crew = *id.CrewFlag
		}
		return identity.Run(ctx, identity.Request{Branch: id.Branch, Crew: crew, Hash: str(id.Hash), Occupied: id.Occupied}, paths, stdout, stderr, identity.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Tmux:   tmux.Exec{},
			Getenv: os.Getenv,
		})
	}

	// adopt records the caller's own identity in pidfile.log: `$$`, `$PPID` and
	// `$PWD`, which crew.sh's exec leaves ours.
	if sub == "adopt" {
		c := parsed.typed.Adopt
		return adopt.Run(ctx, adopt.Request{ID: c.ID, PID: c.PID, Force: c.Force}, paths, stdout, stderr, adopt.Options{
			Proc:  e.proc,
			Clock: clock.FromEnv(),
			PID:   os.Getpid(),
			PPID:  os.Getppid(),
			Pwd:   cwd,
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
			Dir:       cwd,
			CrewID:    func() string { return bus.CrewID(ctx, cwd) },
			Clock:     clock.FromEnv(),
			Sender:    senderProbes(e.proc),
			Tmux:      tmux.Exec{},
			PeerHosts: e.peerHosts,
		}
		if sub == "status" {
			c := parsed.typed.Status
			return status.RunStatus(ctx, status.StatusRequest{From: c.From, State: c.State, Detail: c.Detail, PR: c.PR, Restamp: c.Restamp}, paths, stderr, o)
		}
		return status.RunMsg(ctx, status.MsgRequest{From: word(0), To: word(1), Body: word(2)}, paths, stderr, o)
	}

	// register and deregister record the caller's own identity like adopt does.
	if sub == "register" || sub == "deregister" {
		return register.Run(sub, parsed.typed.Register.PID, paths, stderr, register.Options{
			CrewID: func() string { return bus.CrewID(ctx, cwd) },
			Proc:   e.proc,
			Clock:  clock.FromEnv(),
			PID:    os.Getpid(),
			PPID:   os.Getppid(),
			Pwd:    cwd,
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
			Clock:  clock.FromEnv(),
			Sender: senderProbes(e.proc),
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
		var code int
		if sub == "log" {
			code = log.Run(crew, paths, out, stderr)
		} else {
			code = report.Run(crew, paths, out, stderr, report.Options{
				Clock: clock.FromEnv(),
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

	var rosterRows []map[string]any
	v, code := fold(sub, events, err, paths.Log, stderr, func(docs []any) (any, error) {
		nowSec := e.nowSec()
		if sub == "sessions" {
			return sessions.Fold(docs, branch, crew, nowSec)
		}
		p := e.probes(ctx, cwd)
		p.Live = parsed.typed.Roster.Live
		var err error
		rosterRows, err = roster.Fold(ctx, docs, crew, nowSec, p)
		return rosterRows, err
	})
	if code == exitOpen && sub == "sessions" {
		// jq slurps zero inputs when it cannot open the file, so the program
		// runs on [] and prints its result before exiting 2.
		v = []any{}
	} else if code != 0 {
		return code
	}

	// --live is the form a dispatcher routes to this host: it answers for this
	// host alone, so hosts never ask each other.
	if sub == "roster" && !parsed.typed.Roster.Live {
		merged := roster.MergeRemote(ctx, rosterRows, events, crew, func(ctx context.Context, host string, argv []string, so, se io.Writer) int {
			return remoteAs(ctx, e, cwd, crew)(host, argv, so, se)
		})
		v = merged
	}
	if sub == "roster" && parsed.typed.Roster.Peers {
		peerRows, err := peer.RosterRows(peer.StoreFor(paths), e.peerHosts, e.now())
		if err != nil {
			say(stderr, "crew: roster: peer settings: %v\n", err)
		}
		peerRows, dropped := peer.DropLocal(peerRows, events)
		if dropped > 0 {
			say(stderr, "crew: roster: %d peer rows dropped (a local crew id or branch)\n", dropped)
		}
		v = append(v.([]map[string]any), peerRows...) // after the merge, so a host never relays its peers
	}

	if sub == "roster" || sub == "sessions" {
		v = invisible.Value(v)
	}
	if err := jsonval.Encode(out, v, pretty); err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	if sub == "sessions" && code == 0 {
		say(out, "\n") // the arm's `printf '\n'` after jq -c
	}
	return flush(out, stderr, code)
}

// remoteAs runs a command on a remote host in this checkout's clone, as crew.
func remoteAs(ctx context.Context, e env, cwd, crew string) func(host string, argv []string, stdout, stderr io.Writer) int {
	return func(host string, argv []string, stdout, stderr io.Writer) int {
		return remoteRun(ctx, e, remote.Call{Host: host, Cwd: cwd, Env: []string{"CREW_ID=" + crew}, Argv: argv}, stdout, stderr)
	}
}

// remoteRun is every call to a remote host. The remote takes the request as the
// dispatcher's, since ssh carries neither this env nor this pane, so the sender
// check runs here for every argv, read or write, as the crew the call names.
func remoteRun(ctx context.Context, e env, c remote.Call, stdout, stderr io.Writer) int {
	crew := bus.CrewID(ctx, c.Cwd)
	for _, kv := range c.Env {
		if v, ok := strings.CutPrefix(kv, "CREW_ID="); ok {
			crew = v
		}
	}
	if reason := sender.Check(ctx, "dispatcher:"+crew, crew, senderProbes(e.proc)); reason != "" {
		say(stderr, "crew: remote-run: %s\n", reason)
		return exitRemoteUsage
	}
	return remote.Run(ctx, c, stdout, stderr)
}

// remoteArgv is remote-run's command: the words after the flags, without the
// `--` that ended them.
func remoteArgv(words []string) []string {
	if len(words) > 0 && words[0] == "--" {
		return words[1:]
	}
	return words
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
			Options: stream.Options{Clock: clock.FromEnv(), Proc: e.proc},
			Git:     git.Exec{},
		})
	}
	o := fleet.Options{
		Tmux:      tmux.Exec{},
		Git:       git.Exec{},
		Clock:     clock.FromEnv(),
		PeerHosts: e.peerHosts,
		// A repo's remote workers answer for themselves, as they do for
		// `crew roster`: through remote-run, which runs the sender check, from
		// the checkout of the repo whose bus is being read.
		Remote: func(dir, crew string) roster.Capture {
			return func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
				return remoteAs(ctx, e, dir, crew)(host, argv, stdout, stderr)
			}
		},
	}
	c := p.typed.Fleet.Table
	return fleet.Run(ctx, fleet.Request{All: c.All, JSON: c.JSON, Crew: str(c.Crew)}, stdout, stderr, o)
}

// The Nix build sets these with -X: the pinned bash and the two libs
// stall-watch's budget and local-model probes source. BUDGET_GATE_LIB and
// LOCAL_MODELS_LIB override the libs, as they do for dispatch.sh.
var (
	bashBin        = "bash"
	budgetGateLib  string
	localModelsLib string
	// modelResolveScript is model-resolve.sh, baked by flake.nix; the class
	// check and `crew class` run it. MODEL_RESOLVE_SCRIPT overrides it.
	modelResolveScript string
)

// classEnv is the production classes.Env: the baked resolver, this crew's id
// and bus. Getenv, Settings and Now take the classes defaults.
func classEnv(ctx context.Context, cwd string, paths bus.Paths) classes.Env {
	return classes.Env{
		Bash:   bashBin,
		Script: cmp.Or(os.Getenv("MODEL_RESOLVE_SCRIPT"), modelResolveScript),
		CrewID: bus.CrewID(ctx, cwd),
		Paths:  paths,
	}
}

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
func fold(sub string, events []bus.Event, readErr error, log string, stderr io.Writer, f func([]any) (any, error)) (any, int) {
	var v any
	err := readErr
	if err == nil {
		docs := make([]any, len(events))
		for i, e := range events {
			docs[i] = e.Doc
		}
		v, err = f(docs)
	}
	if err == nil {
		return v, 0
	}
	var exit *git.ExitError
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

// senderProbes are the real reads behind the sender check: the caller's env, its
// pid ancestry, and every pane's stamps.
func senderProbes(t proc.Table) sender.Probes {
	return sender.Probes{Getenv: os.Getenv, Tmux: tmux.Exec{}, Proc: t}
}

func probes(ctx context.Context, cwd string) roster.Probes {
	return roster.Probes{
		Tmux:      tmux.Exec{},
		Worktrees: func() (string, error) { return git.Worktrees(ctx, git.Exec{}, cwd, "") },
	}
}
