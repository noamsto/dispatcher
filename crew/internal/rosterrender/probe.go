package rosterrender

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Probes are the side effects the loop cannot compute, injected so the daemon is
// testable without a terminal, an aeye install, or a second build to hop into. A
// nil field is a bug in Default, not a runtime case. The pid probes are not here:
// they come in as crews.Probes, shared with `crews`, `stall` and `adopt`.
type Probes struct {
	// RolePanes is the stdout of `tmux list-panes -a -F` with rolePaneFormat. A
	// tmux failure is the arm's `|| true`: no rows.
	RolePanes func() string
	// PanePID is `tmux display-message -p -t <pane> '#{pane_pid}'`, ok=false when
	// tmux fails or the pane is gone.
	PanePID func(pane string) (string, bool)
	// AeyeHelp is `timeout 10 aeye --help`: stdout only, stderr dropped.
	AeyeHelp func(ctx context.Context) (string, error)
	// AeyePublish is `timeout 60 aeye publish-diagram <file> --pane <pane>
	// [--open]` with both streams dropped; its status is the arm's.
	AeyePublish func(ctx context.Context, file, pane string, open bool) error
	// InstalledCrew is `_rr_installed_crew` over the given PATH.
	InstalledCrew func(path string) string
	// SpawnDetached starts argv as a detached child — new session, /dev/null
	// stdio, the given environment — without waiting for it.
	SpawnDetached func(argv, env []string) error
	// Exec is the build hop: replace this process. It only returns on failure.
	Exec func(path string, argv, env []string) error
	// Sleep is the poll wait on the wall clock, cut short by a done ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Default wires the real tmux and aeye reads. ctx is the command's, so a killed
// worker never leaves a `timeout 60 aeye` behind it.
func Default(ctx context.Context) Probes {
	return Probes{
		RolePanes: func() string {
			out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", rolePaneFormat).Output()
			if err != nil {
				return ""
			}
			return string(out)
		},
		PanePID: func(pane string) (string, bool) {
			out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", pane, "#{pane_pid}").Output()
			if err != nil {
				return "", false
			}
			return strings.TrimRight(string(out), "\n"), true
		},
		AeyeHelp: func(ctx context.Context) (string, error) {
			out, err := exec.CommandContext(ctx, "timeout", "10", "aeye", "--help").Output()
			return string(out), err
		},
		AeyePublish: func(ctx context.Context, file, pane string, open bool) error {
			args := []string{"timeout", "60", "aeye", "publish-diagram", file, "--pane", pane}
			if open {
				args = append(args, "--open")
			}
			return exec.CommandContext(ctx, args[0], args[1:]...).Run()
		},
		InstalledCrew: InstalledCrew,
		SpawnDetached: SpawnDetached,
		Exec: func(path string, argv, env []string) error {
			return syscall.Exec(path, argv, env)
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// SpawnDetached starts argv in its own session with its stdio off /dev/null, so
// the daemon has no controlling terminal and outlives whoever started it — the
// arm's `nohup … </dev/null >/dev/null 2>&1 &`.
func SpawnDetached(argv, env []string) error {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
