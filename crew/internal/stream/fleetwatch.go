package stream

import (
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	lanepkg "github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

const (
	defaultFleetWatchTimeout    = "3300"
	defaultFleetWatchRediscover = 30 * time.Second
	fleetWatchPoll              = "2"
	fleetWatchPrefix            = "crew: fleet watch"
)

// FleetWatchRequest is one `crew fleet watch` call; "" is each flag's default.
type FleetWatchRequest struct{ Crew, States, Timeout string }

// FleetWatchOptions is everything RunFleetWatch reads beyond the request.
// Watch supplies Hosts, Flush, Clock and Sleep; its CrewID and Repo are unused.
type FleetWatchOptions struct {
	Watch watch.Options
	Proc  proc.Table
	Git   git.Client
	// Discover lists the candidate buses. nil is repos.Discover over Registry.
	Discover func(stderr io.Writer) []repos.Repo
	Registry string
	Getenv   func(string) string
	// StateRoot holds the rotation file per crew. Empty is
	// ${XDG_STATE_HOME:-$HOME/.local/state}/crew/fleet-watch.
	StateRoot  string
	Rediscover time.Duration // 0 is 30s
}

// RunFleetWatch is `crew fleet watch`: park over every local bus holding rows
// for the crew, deliver the first batch any of them has through the same fold
// and cursor `crew watch` uses, and exit. One repo's batch per call.
func RunFleetWatch(r FleetWatchRequest, stdout, stderr io.Writer, o FleetWatchOptions) int {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Proc == nil {
		o.Proc = proc.OS{}
	}
	if o.Discover == nil {
		registry, env := o.Registry, o.Getenv
		o.Discover = func(stderr io.Writer) []repos.Repo {
			return repos.Discover(repos.Options{Registry: registry, Getenv: env, Stderr: stderr, Prefix: fleetWatchPrefix})
		}
	}
	if o.StateRoot == "" {
		o.StateRoot = filepath.Join(stateHome(o.Getenv), "crew", "fleet-watch")
	}
	if o.Rediscover == 0 {
		o.Rediscover = defaultFleetWatchRediscover
	}
	wo := o.Watch
	if wo.Clock.Now == nil {
		wo.Clock.Now = time.Now
	}
	wo.Clock.CrewClock = "" // like crew watch, never $CREW_CLOCK
	if wo.Sleep == nil {
		wo.Sleep = wo.Clock.Sleep
	}

	if r.Crew == "" {
		say(stderr, "%s: --crew ID is required\n", fleetWatchPrefix)
		return exitUsage
	}
	req := watch.Request{Crew: r.Crew, States: r.States, Timeout: r.Timeout}
	if msg := watch.Validate(req, wo); msg != "" {
		say(stderr, "%s\n", msg)
		return exitUsage
	}
	timeoutText := r.Timeout
	if timeoutText == "" {
		timeoutText = defaultFleetWatchTimeout
	}
	timeout, _ := strconv.ParseInt(timeoutText, 10, 64)

	// Armed before the first pass so a TERM never kills a pass between its lock
	// and release.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	n := newNotes(stderr)
	n.prefix = fleetWatchPrefix
	joined := map[string]struct{}{}
	names := FleetOptions{Git: o.Git}
	rotation := filepath.Join(o.StateRoot, r.Crew, "last")
	deadline := wo.Clock.RealMS() + timeout*1000
	var lastDiscover time.Time
	discovered := false

	for {
		select {
		case s := <-sigs:
			if sig, ok := s.(syscall.Signal); ok {
				return 128 + int(sig)
			}
			return 1
		default:
		}
		if now := wo.Clock.Now(); !discovered || now.Sub(lastDiscover) >= o.Rediscover {
			discovered, lastDiscover = true, now
			discoverBuses(o.Discover, n, joined,
				func(common string) { delete(joined, common) },
				func(common string, events []bus.Event) {
					if hasCrew(events, r.Crew) {
						joined[common] = struct{}{}
					}
				})
		}

		for _, common := range rotate(joined, readRotation(rotation)) {
			cdir := filepath.Join(common, "crew", "crews", r.Crew)
			repo := names.repoName(common)
			if state, _, pid, _ := lanepkg.State(cdir, o.Proc, wo.Clock.RealMS()); state == "alive" {
				n.lines(noteLine("%s: %s: per-repo stream alive (pid %s) — not watching\n", fleetWatchPrefix, repo, pidText(pid)))
				continue
			}
			ro := wo
			ro.Repo = repo
			paths := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
			outcome, rc := watch.Once(req, paths, stdout, stderr, ro)
			switch outcome {
			case watch.Locked:
				n.lines(noteLine("%s: %s: another watch is already running for this crew — skipped\n", fleetWatchPrefix, repo))
			case watch.Delivered:
				writeRotation(rotation, common)
				return 0
			default:
				if rc != 0 {
					return rc
				}
			}
		}

		if wo.Clock.RealMS() >= deadline {
			say(stderr, "%s park ended after %ss — no new events\n", fleetWatchPrefix, timeoutText)
			return 0
		}
		if err := wo.Sleep(fleetWatchPoll, stderr); err != nil {
			select {
			case s := <-sigs:
				if sig, ok := s.(syscall.Signal); ok {
					return 128 + int(sig)
				}
			default:
			}
			return 1
		}
	}
}

// noteLine formats a note line for notes.lines, which dedups by the whole line.
func noteLine(format string, args ...any) string {
	var b strings.Builder
	say(&b, format, args...)
	return b.String()
}

// rotate orders the joined repos by common dir, starting at the first one
// sorted after last and wrapping, so a repo that delivers goes to the back.
func rotate(joined map[string]struct{}, last string) []string {
	order := make([]string, 0, len(joined))
	for c := range joined {
		order = append(order, c)
	}
	sort.Strings(order)
	start := sort.Search(len(order), func(i int) bool { return order[i] > last })
	if start == len(order) {
		start = 0
	}
	return append(order[start:], order[:start]...)
}

func readRotation(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\n")
}

// writeRotation is best effort: losing it only loses fairness.
func writeRotation(path, common string) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".last.")
	if err != nil {
		return
	}
	_, werr := tmp.WriteString(common + "\n")
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
