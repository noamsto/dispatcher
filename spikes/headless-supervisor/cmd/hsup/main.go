// Command hsup runs the headless-supervisor spike: scripted cases against an
// engine child, and a long-lived serve/ctl pair for pause, resume and take-over.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/cases"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/claude"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/pi"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/transcript"
)

const usage = `usage:
  hsup cases
  hsup case <name> --engine <e> --work <dir> --transcript <file> [--outside <dir>] [--state <dir>] [--mcpecho <bin>] [--gate <ts>] [--timeout 10m]
  hsup serve --engine <e> --work <dir> --state <dir> [--model <m>] [--session-id <id>] [--transcript <file>] [--silence <d>] [--gate <ts>] [--policy allow|deny]
  hsup ctl <state> <pause|resume|send|status|stop|session> [text...]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "cases":
		listCases(stdout)
		return 0
	case "case":
		return runCase(args[1:], stdout, stderr)
	case "serve":
		err = serve(args[1:], stderr)
	case "ctl":
		err = ctl(args[1:], stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "hsup: unknown command %q\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hsup:", err)
		return 1
	}
	return 0
}

func listCases(w io.Writer) {
	for _, c := range cases.All() {
		engines := c.Engines
		if len(engines) == 0 {
			engines = cases.DefaultEngines
		}
		for _, e := range engines {
			_, _ = fmt.Fprintf(w, "%s %s\n", c.Name, e)
		}
	}
}

// newScrubber maps each directory to its placeholder; empty dirs are skipped.
func newScrubber(work, outside, state string) *transcript.Scrubber {
	home, _ := os.UserHomeDir()
	return &transcript.Scrubber{Paths: []transcript.PathRule{
		{Prefix: home, Placeholder: "$HOME"},
		{Prefix: work, Placeholder: "$WORK"},
		{Prefix: outside, Placeholder: "$OUTSIDE"},
		{Prefix: state, Placeholder: "$STATE"},
		{Prefix: os.TempDir(), Placeholder: "$TMP"},
	}}
}

func runCase(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("case", flag.ContinueOnError)
	fs.SetOutput(stderr)
	engine := fs.String("engine", "", "engine name")
	work := fs.String("work", "", "work dir (child cwd)")
	outside := fs.String("outside", "", "directory outside the work dir")
	state := fs.String("state", "", "supervisor state dir (default: fresh temp dir)")
	tpath := fs.String("transcript", "", "transcript JSONL file")
	mcpecho := fs.String("mcpecho", os.Getenv("HSUP_MCPECHO"), "mcpecho binary")
	gate := fs.String("gate", os.Getenv("HSUP_GATE"), "pi gate extension")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall case timeout")
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stdout, "FAIL %s %s: %s\n", name, *engine, fmt.Sprintf(format, a...))
		return 1
	}
	c, ok := cases.Lookup(name)
	if !ok {
		return fail("unknown case")
	}
	if !c.Applies(*engine) {
		return fail("case not defined for this engine")
	}
	if *work == "" || *tpath == "" {
		return fail("--work and --transcript are required")
	}
	if *outside == "" {
		*outside = *work + "-outside"
	}
	reg := registry()
	if _, ok := reg[*engine]; !ok {
		return fail("unknown engine")
	}
	for _, d := range []string{*work, *outside, filepath.Dir(*tpath)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fail("%v", err)
		}
	}
	if *state == "" {
		d, err := os.MkdirTemp("", "hsup-state-")
		if err != nil {
			return fail("%v", err)
		}
		defer func() { _ = os.RemoveAll(d) }() // created above, removed by absolute name
		*state = d
	}
	tw, err := transcript.New(*tpath, newScrubber(*work, *outside, *state))
	if err != nil {
		return fail("%v", err)
	}
	defer func() { _ = tw.Close() }()

	env := &cases.CaseEnv{
		Engine: *engine, Work: *work, Outside: *outside, State: *state, Transcript: tw,
		MCPEcho: *mcpecho, Gate: *gate,
		Factory: func(o event.Options) (event.Engine, error) {
			if o.Work == "" {
				o.Work = *work
			}
			if o.Transcript == nil {
				o.Transcript = tw
			}
			if err := fillBin(*engine, &o, *gate); err != nil {
				return nil, err
			}
			return reg.New(*engine, o)
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	msg, err := c.Run(ctx, env)
	if err != nil {
		return fail("%v", err)
	}
	_, _ = fmt.Fprintf(stdout, "PASS %s %s: %s\n", name, *engine, msg)
	return 0
}

// fillBin resolves the child binary the adapters require. The ACP engines
// resolve their own, so their Bin stays empty.
func fillBin(engine string, o *event.Options, gate string) (err error) {
	switch engine {
	case "claude":
		if o.Bin == "" {
			o.Bin, err = claude.ResolveBin()
		}
	case "pi":
		if o.Bin == "" {
			o.Bin, err = pi.ResolveBin()
		}
		if o.GatePath == "" {
			o.GatePath = gate
		}
	}
	return err
}

func serve(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	engine := fs.String("engine", "", "engine name")
	work := fs.String("work", "", "work dir (child cwd)")
	state := fs.String("state", "", "state dir")
	model := fs.String("model", "", "model override")
	sessionID := fs.String("session-id", "", "session id (default: new)")
	tpath := fs.String("transcript", "", "transcript JSONL file (default: <state>/transcript.jsonl)")
	silence := fs.Duration("silence", 0, "liveness silence limit (0 disables)")
	gate := fs.String("gate", os.Getenv("HSUP_GATE"), "pi gate extension")
	policyName := fs.String("policy", "deny", "permission policy: allow (every request, once) or deny")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *engine == "" || *work == "" || *state == "" {
		return errors.New("serve: --engine, --work and --state are required")
	}
	var policy func(*event.Request) event.Decision // nil: the server denies
	switch *policyName {
	case "deny":
	case "allow":
		policy = func(*event.Request) event.Decision { return event.Decision{Verdict: "allow_once"} }
	default:
		return fmt.Errorf("serve: --policy %q: want allow or deny", *policyName)
	}
	// session.json hands these paths to a TUI started from another cwd.
	var err error
	if *work, err = filepath.Abs(*work); err != nil {
		return err
	}
	if *state, err = filepath.Abs(*state); err != nil {
		return err
	}
	reg := registry()
	if _, ok := reg[*engine]; !ok {
		return fmt.Errorf("serve: unknown engine %q", *engine)
	}
	if err := supervisor.PrepareState(*state); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if *tpath == "" {
		*tpath = filepath.Join(*state, "transcript.jsonl")
	}
	opts, sessionDir, err := serveOptions(*engine, event.Options{Work: *work, Model: *model, SessionID: *sessionID}, *gate)
	if err != nil {
		return err
	}
	tw, err := transcript.New(*tpath, newScrubber(*work, "", *state))
	if err != nil {
		return err
	}
	defer func() { _ = tw.Close() }()
	opts.Transcript = tw

	// SIGHUP (the window closing) must tear the child down too; the child
	// keeps the inherited lock until it has exited.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	srv := supervisor.NewServer(supervisor.Config{
		Engine: *engine, Work: *work, State: *state, Factory: reg[*engine],
		Options: opts, SessionDir: sessionDir, Policy: policy,
		Transcript: tw, Silence: *silence,
	})
	return srv.Serve(ctx)
}

func ctl(args []string, stdout io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: hsup ctl <state> <pause|resume|send|status|stop|session> [text...]")
	}
	// The socket and session.json decide what takeover.sh launches; trust them
	// only from a state dir no one else can plant files in.
	if err := supervisor.CheckState(args[0]); err != nil {
		return err
	}
	if args[1] == "session" {
		sess, err := supervisor.ReadSession(args[0])
		if err != nil {
			return err
		}
		return printSession(stdout, sess)
	}
	req := supervisor.CtlRequest{Cmd: args[1], Arg: strings.Join(args[2:], " ")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resp, err := supervisor.Call(ctx, args[0], req)
	if err != nil {
		return err
	}
	switch req.Cmd {
	case "send":
		_, _ = fmt.Fprintln(stdout, resp.Text)
	case "status":
		_, _ = fmt.Fprintf(stdout, "state=%s turn=%s engine=%s session=%s work=%s\n", resp.State, resp.Turn, resp.Engine, resp.SessionID, resp.Work)
	default:
		_, _ = fmt.Fprintf(stdout, "ok state=%s session=%s\n", resp.State, resp.SessionID)
	}
	return nil
}
