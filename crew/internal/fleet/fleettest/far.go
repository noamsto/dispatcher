// Package fleettest is a far fleet a partner overseer pulls from, served the
// way that host's `crew overseer status` answers, and the overseer side that
// pulls it. Package fleet's own tests cannot import it: it imports fleet.
package fleettest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// Far is a partner host's fleet: a registry of repos under a temp root, each
// common dir root/<name>/.git, on a clock only the test moves.
type Far struct {
	tb       testing.TB
	root     string
	registry string
	ms       atomic.Int64
	mu       sync.Mutex
	names    []string
	opts     fleet.StatusOptions
}

// NewFar is host's fleet with an empty registry, its clock at nowMS.
func NewFar(tb testing.TB, host string, nowMS int64) *Far {
	tb.Helper()
	root := tb.TempDir()
	f := &Far{tb: tb, root: root, registry: filepath.Join(root, "registry")}
	f.ms.Store(nowMS)
	empty := tb.TempDir()
	env := map[string]string{
		"HOME":                  empty,
		"CREW_SWEEP_ROOTS":      empty,
		"XDG_STATE_HOME":        filepath.Join(root, "state"),
		"DISPATCH_LOADAVG_FILE": filepath.Join(root, "loadavg"),
	}
	write(tb, env["DISPATCH_LOADAVG_FILE"], "0.50 0 0 1/1 1\n")
	write(tb, f.registry, "")
	f.opts = fleet.StatusOptions{
		Options: fleet.Options{
			Getenv:   func(k string) string { return env[k] },
			Registry: f.registry,
			Tmux:     &tmuxtest.Fake{Out: map[string]string{"list-panes": "", "list-windows": ""}},
			Git: &gittest.Fake{Func: func(_ string, args []string) (string, error) {
				if args[len(args)-1] == "--porcelain" {
					return "", nil
				}
				return "", &git.ExitError{Code: 1}
			}},
			Clock: clock.Clock{Now: f.now},
		},
		Proc:       proctest.New(nil, nil),
		Host:       func() (string, error) { return host, nil },
		Budget:     func(context.Context) (int, error) { return 8, nil },
		BudgetFile: filepath.Join(root, "engine-budget.json"),
		Cmd: func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "nproc" {
				return "4", nil
			}
			return "", fmt.Errorf("no such command")
		},
	}
	return f
}

func (f *Far) now() time.Time { return time.UnixMilli(f.ms.Load()) }

// Now is the far clock, in ms.
func (f *Far) Now() int64 { return f.ms.Load() }

// Advance moves the far clock by d.
func (f *Far) Advance(d time.Duration) { f.ms.Add(d.Milliseconds()) }

// Common is the common dir of the repo name.
func (f *Far) Common(name string) string { return filepath.Join(f.root, name, ".git") }

// Register lists the repo name in the registry with no bus yet: discovery
// skips it until Append makes one.
func (f *Far) Register(name string) {
	f.tb.Helper()
	if err := os.MkdirAll(filepath.Join(f.Common(name), "crew"), 0o755); err != nil {
		f.tb.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	var reg strings.Builder
	for _, n := range f.names {
		reg.WriteString(f.Common(n) + "\n")
	}
	write(f.tb, f.registry, reg.String())
}

// Repo registers the repo name with an empty bus.
func (f *Far) Repo(name string) {
	f.tb.Helper()
	f.Register(name)
	f.Append(name)
}

// Append adds lines to the bus of the registered repo name, making the bus
// when it has none.
func (f *Far) Append(name string, lines ...string) {
	f.tb.Helper()
	f.mu.Lock()
	known := slices.Contains(f.names, name)
	f.mu.Unlock()
	if !known {
		f.tb.Fatalf("fleettest: repo %s is not registered", name)
	}
	file, err := os.OpenFile(filepath.Join(f.Common(name), "crew", "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		f.tb.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for _, l := range lines {
		if _, err := file.WriteString(l + "\n"); err != nil {
			f.tb.Fatal(err)
		}
	}
}

// Reply is a bus row: crew's dispatcher replying to overseer:<origin> at ts.
func Reply(ts int64, crew, origin, body string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"from":"dispatcher:%s","to":"overseer:%s","kind":"msg","body":%q}`, ts, crew, crew, origin, body)
}

// Serve answers a pull as the far `crew overseer status --origin <o> --since
// <ms>` does: ServeFar over this fleet, encoded as one JSON object.
func (f *Far) Serve() roster.Capture {
	return func(ctx context.Context, _ string, argv []string, stdout, stderr io.Writer) int {
		if len(argv) != 7 || !slices.Equal(argv[:3], []string{"crew", "overseer", "status"}) || argv[3] != "--origin" || argv[5] != "--since" {
			_, _ = fmt.Fprintf(stderr, "fleettest: argv %q is no overseer status pull\n", argv)
			return 2
		}
		since, err := strconv.ParseInt(argv[6], 10, 64)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "fleettest: --since %q: %v\n", argv[6], err)
			return 2
		}
		ans, err := fleet.ServeFar(ctx, argv[4], since, stderr, f.opts)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if err := jsonval.Encode(stdout, ans, false); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
}

// Overseer is the status options of self's overseer session: no repos of its
// own, state under stateDir, a passing sender check for overseer:<self>, and
// settings hosts self plus partners, each pulled through federate.
func Overseer(tb testing.TB, self, stateDir string, now func() time.Time, federate roster.Capture, partners ...string) fleet.StatusOptions {
	tb.Helper()
	root := tb.TempDir()
	env := map[string]string{"CREW_OVERSEER": self, "XDG_STATE_HOME": stateDir, "HOME": root, "CREW_SWEEP_ROOTS": root}
	registry := filepath.Join(root, "registry")
	write(tb, registry, "")
	getenv := func(k string) string { return env[k] }
	return fleet.StatusOptions{
		Options: fleet.Options{
			Getenv:   getenv,
			Registry: registry,
			Tmux:     &tmuxtest.Fake{Out: map[string]string{"list-panes": "", "list-windows": ""}},
			Git:      &gittest.Fake{Func: func(string, []string) (string, error) { return "", &git.ExitError{Code: 1} }},
			Clock:    clock.Clock{Now: now},
		},
		Proc:     proctest.New(nil, nil),
		Host:     func() (string, error) { return self, nil },
		Budget:   func(context.Context) (int, error) { return 8, nil },
		Cmd:      func(context.Context, string, ...string) (string, error) { return "4", nil },
		Federate: federate,
		Sender: sender.Probes{
			Getenv: getenv,
			Tmux:   &tmuxtest.Fake{Out: map[string]string{"list-panes": ""}},
			Proc:   proctest.New(nil, map[int]int{os.Getpid(): 1}),
			Self:   func() (string, error) { return self, nil },
			Hosts: func() (map[string]remote.Host, error) {
				hosts := map[string]remote.Host{self: {}}
				for _, p := range partners {
					hosts[p] = remote.Host{}
				}
				return hosts, nil
			},
		},
		FederateTimeout: 5 * time.Second,
		BudgetFile:      filepath.Join(root, "engine-budget.json"),
	}
}

func write(tb testing.TB, path, text string) {
	tb.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		tb.Fatal(err)
	}
}
