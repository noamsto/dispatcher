package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

// PrioUsage is the line every `crew fleet prio` grammar refusal prints.
const PrioUsage = "crew: fleet prio <repo> <n>"

// maxPrio bounds a repo priority either way.
const maxPrio = 1000000

// stdinTTY is the terminal stdin is, which an audit line names as the caller
// when no tmux pane does; a test seam.
var stdinTTY = func() string {
	link, _ := os.Readlink("/proc/self/fd/0")
	return link
}

// PrioRequest is `crew fleet prio <repo> <n>`.
type PrioRequest struct{ Repo, N string }

// priorityFile is fleet-priority.json: each registered repo's priority by its
// common dir, 0 when absent.
type priorityFile struct {
	Repos     map[string]int64 `json:"repos"`
	UpdatedTS int64            `json:"updated_ts"`
}

// priorityPath is fleet-priority.json in the crew data dir, beside the repo
// registry.
func priorityPath(getenv func(string) string) string {
	return filepath.Join(filepath.Dir(repos.RegistryPath(getenv)), "fleet-priority.json")
}

// readPriorities is the repo priorities by common dir; a missing or corrupt
// file reads as all 0.
func readPriorities(path string) map[string]int64 {
	var f priorityFile
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &f) != nil {
		return map[string]int64{}
	}
	return f.Repos
}

// Prio sets a registered repo's priority, which the fleet queue ranks before
// anything else, on the human's word. It is refused under the dispatcher
// tripwire unless the caller is the overseer session, so a worker following an
// injected instruction cannot put its repo first, and every change appends one
// audit line.
func Prio(ctx context.Context, req PrioRequest, stdout, stderr io.Writer, o QueueOptions) int {
	if reason := prioSender(ctx, o); reason != "" {
		say(stderr, "crew: fleet prio: refused — %s\n", reason)
		return ExitError
	}
	n, err := strconv.ParseInt(req.N, 10, 64)
	if err != nil || n < -maxPrio || n > maxPrio {
		say(stderr, "%s\n", PrioUsage)
		return ExitError
	}
	common, name, why := registeredRepo(ctx, req.Repo, o)
	if why != "" {
		say(stderr, "crew: fleet prio: %s\n", why)
		return ExitError
	}
	path := priorityPath(o.Getenv)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		say(stderr, "crew: fleet prio: %s\n", oneLine(err))
		return ExitError
	}
	lockDir := filepath.Join(filepath.Dir(path), "fleet-priority.lock.d")
	if !acquire(lockDir) {
		say(stderr, "crew: fleet prio: %s is held by %s\n", lockDir, cleanText(lock.Holder(lockDir)))
		return ExitError
	}
	defer lock.Release(lockDir)

	prios := readPriorities(path)
	old := prios[common]
	if old == n {
		say(stdout, "%s: priority %d (unchanged)\n", cleanText(name), n)
		return ExitFits
	}
	f := priorityFile{Repos: maps.Clone(prios), UpdatedTS: o.Clock.Now().UnixMilli()}
	if f.Repos == nil {
		f.Repos = map[string]int64{}
	}
	if n == 0 {
		delete(f.Repos, common)
	} else {
		f.Repos[common] = n
	}
	data, err := json.Marshal(f)
	if err == nil {
		err = writeAtomic(path, append(data, '\n'))
	}
	if err == nil {
		err = audit(filepath.Join(filepath.Dir(path), "fleet-priority.log"), f.UpdatedTS, common, old, n, caller(o.Getenv))
	}
	if err != nil {
		say(stderr, "crew: fleet prio: %s\n", oneLine(err))
		return ExitError
	}
	say(stdout, "%s: priority %d → %d\n", cleanText(name), old, n)
	return ExitFits
}

// prioSender is why the caller may not set a priority, "" when it may: the
// dispatcher, or the overseer session, which posts only as overseer:<host>. A
// session marked as the overseer is told why the overseer check refused it,
// not the dispatcher check's reason, which says it may not post here at all.
func prioSender(ctx context.Context, o QueueOptions) string {
	reason := sender.Check(ctx, "dispatcher:fleet", "", o.Sender)
	if reason == "" {
		return ""
	}
	marked := o.Sender.Getenv != nil && o.Sender.Getenv("CREW_OVERSEER") != ""
	var self string
	err := errors.New("no host-name probe")
	if o.Sender.Self != nil {
		self, err = o.Sender.Self()
	}
	if err != nil {
		if marked {
			return "this host's name cannot be read (" + err.Error() + "), so the overseer cannot be told from a worker"
		}
		return reason
	}
	overseer := sender.Check(ctx, "overseer:"+self, "", o.Sender)
	if overseer == "" || marked {
		return overseer
	}
	return reason
}

// registeredRepo is the one registry entry arg names, by its common dir, its
// checkout, a directory in one of its worktrees or its repo name, as a clean
// path; why says when none or several do.
func registeredRepo(ctx context.Context, arg string, o QueueOptions) (common, name, why string) {
	abs, _ := filepath.Abs(arg)
	var gitCommon string
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		gitCommon = git.Output(ctx, o.Git, abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	}
	names := repoNamer(ctx, o.Git)
	var matches []string
	for _, r := range repos.Registered(o.Registry, o.Getenv) {
		c := filepath.Clean(r.Common)
		origin, _ := git.Origin(ctx, o.Git, r.Common)
		if abs == c || abs == filepath.Dir(c) || gitCommon == c || arg == names(r.Common) || arg == repos.Name(r.Common, origin) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return "", "", cleanText(arg) + " is not a registered repo"
	case 1:
		return matches[0], names(matches[0]), ""
	}
	return "", "", cleanText(arg) + " matches more than one registered repo: " + cleanText(strings.Join(matches, ", "))
}

// caller is who asked, for the audit line: the tmux pane, else the terminal,
// else "-".
func caller(getenv func(string) string) string {
	if pane := getenv("TMUX_PANE"); pane != "" {
		return pane
	}
	if tty := stdinTTY(); strings.HasPrefix(tty, "/dev/") {
		return tty
	}
	return "-"
}

// audit appends one tab-separated line: when, which repo, the old and new
// priority, and the caller.
func audit(path string, ts int64, common string, old, n int64, who string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%d\t%s\t%d\t%d\t%s\n", ts, cleanText(common), old, n, cleanText(who)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
