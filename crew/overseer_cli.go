package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/overseer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// overseerProbes are the sender reads for every overseer: row this process
// reads or posts: the real env, panes and ancestry, plus the settings hosts a
// foreign overseer's name is checked against.
func overseerProbes(e env) sender.Probes {
	p := senderProbes(e.proc)
	p.Hosts = e.peerHosts
	return p
}

// overseerProbe is the capture an overseer asks a repo's --host workers
// through, given the repo's checkout dir.
func overseerProbe(e env, run func(context.Context, remote.Call, io.Writer, io.Writer) int) func(dir string) roster.Capture {
	return overseerProbeFor(overseerProbes(e), run)
}

// overseerProbeFor runs only `crew roster --live -- <crew>` on the host, after
// the overseer sender check for that crew. It never goes through remoteRun,
// which checks dispatcher:<crew> and would refuse the overseer session.
func overseerProbeFor(p sender.Probes, run func(context.Context, remote.Call, io.Writer, io.Writer) int) func(dir string) roster.Capture {
	return func(dir string) roster.Capture {
		return func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
			refuse := func(format string, a ...any) int {
				_, _ = fmt.Fprintf(stderr, "crew: overseer probe: "+format+"\n", a...)
				return exitFailure
			}
			if len(argv) != 5 || !slices.Equal(argv[:4], []string{"crew", "roster", "--live", "--"}) {
				return refuse("only 'crew roster --live -- <crew>' runs on a host")
			}
			if p.Self == nil {
				return refuse("this host's name cannot be read")
			}
			self, err := p.Self()
			if err != nil {
				return refuse("this host's name cannot be read (%v)", err)
			}
			if reason := sender.Check(ctx, "overseer:"+self, argv[4], p); reason != "" {
				return refuse("%s", reason)
			}
			return run(ctx, remote.Call{Host: host, Cwd: dir, Argv: argv}, stdout, stderr)
		}
	}
}

// overseerOptions are the production reads behind the overseer verbs.
func overseerOptions(e env, stderr io.Writer) overseer.Options {
	o := fleet.Options{Tmux: tmux.Exec{}, Git: git.Exec{}, Clock: clock.FromEnv()}
	return overseer.Options{
		Git:        git.Exec{},
		Clock:      clock.FromEnv(),
		Sender:     overseerProbes(e),
		RunHost:    remote.RunHost,
		Status:     statusOptions(o, e, stderr),
		ServedRoot: servedRoot,
	}
}

// servedRoot is the sweep root the far verbs walk besides the registry:
// .overseer.root of the dispatcher settings, "" when unset. Unreadable
// settings are an error, so a far verb never serves fewer buses than it has.
func servedRoot(ctx context.Context) (string, error) {
	bin := remote.ConfigBin()
	out, err := exec.CommandContext(ctx, bin).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", bin, err)
	}
	var s struct {
		Overseer struct {
			Root string `json:"root"`
		} `json:"overseer"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return "", fmt.Errorf("%s: %w", bin, err)
	}
	return s.Overseer.Root, nil
}
