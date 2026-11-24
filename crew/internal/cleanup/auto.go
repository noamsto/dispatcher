package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/snooze"
)

const (
	autoLogName = "auto.log"
	// summaryTail bounds how much of the log --list reads to find the last summary.
	summaryTail = 64 << 10
)

// Settings are the cleanup.* dispatcher settings.
type Settings struct {
	Auto          bool `json:"auto"`
	AutoAfterDays int  `json:"autoAfterDays"`
	ArchiveDays   int  `json:"archiveDays"`
}

var defaultSettings = Settings{Auto: true, AutoAfterDays: 14, ArchiveDays: 365}

// dryVerb is what a dry run prints in place of the action it skips.
var dryVerb = map[string]string{"remove": "would remove", "skipped": "would skip", "pruned": "would prune"}

type logEntry struct {
	TS      string `json:"ts"`
	Action  string `json:"action"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Issue   []Ref  `json:"issue,omitempty"`
	PR      *Ref   `json:"pr,omitempty"`
	Archive string `json:"archive,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type summary struct {
	TS      string `json:"ts"`
	Action  string `json:"action"`
	Removed int    `json:"removed"`
	Skipped int    `json:"skipped"`
	Errors  int    `json:"errors"`
	Pruned  int    `json:"pruned"`
}

// autoRun is one `--auto` pass: its counters and where it reports.
type autoRun struct {
	o                       Options
	dry                     bool
	stdout, stderr          io.Writer
	removed, skipped, fails int
	pruned                  int
}

// runAuto is `crew cleanup --auto`: it archives and removes the stale items
// whose work is settled and past the grace period, then prunes old archives.
// It exits 1 when settings are unreadable or any repo or removal errored.
func (o Options) runAuto(ctx context.Context, req Request, cwd string, stdout, stderr io.Writer) int {
	s := defaultSettings
	if o.Settings != nil {
		var err error
		if s, err = o.Settings(ctx); err != nil {
			say(stderr, "crew cleanup: --auto: cannot read the cleanup settings: %v\n", err)
			return 1
		}
	}
	if !s.Auto {
		say(stdout, "crew cleanup: --auto is disabled (cleanup.auto is false)\n")
		return 0
	}
	grace := time.Duration(s.AutoAfterDays) * 24 * time.Hour
	if req.OlderThan != "" {
		var err error
		if grace, err = ParseAge(req.OlderThan); err != nil {
			say(stderr, "crew cleanup: %v\n", err)
			return 1
		}
	}
	buses, cwds, code := o.repos(ctx, Request{Fleet: req.Fleet}, cwd, stderr)
	if code != 0 {
		return code
	}

	a := &autoRun{o: o, dry: req.DryRun, stdout: stdout, stderr: stderr}
	for _, p := range buses {
		a.bus(ctx, p, cwds[p.Common], cwd, grace)
	}
	if s.ArchiveDays > 0 {
		a.prune(time.Duration(s.ArchiveDays) * 24 * time.Hour)
	}
	say(stdout, "crew cleanup: --auto: removed %d, skipped %d, errors %d, pruned %d\n", a.removed, a.skipped, a.fails, a.pruned)
	if !a.dry {
		a.write(summary{TS: o.stamp(), Action: "summary", Removed: a.removed, Skipped: a.skipped, Errors: a.fails, Pruned: a.pruned})
	}
	if a.fails > 0 {
		return 1
	}
	return 0
}

func (o Options) stamp() string { return o.now().UTC().Format(time.RFC3339) }

// bus handles one repo; a failure there is logged and never stops the others.
func (a *autoRun) bus(ctx context.Context, p bus.Paths, cwd, callerCwd string, grace time.Duration) {
	o := a.o
	cands, code := reap.Candidates(ctx, p, cwd, a.stderr, o.Reap)
	if code != 0 {
		a.fails++
		a.log(logEntry{Action: "error", Repo: p.Common, Reason: fmt.Sprintf("reap failed (exit %d)", code)})
		return
	}
	for _, h := range snooze.Snoozed(p.Dir, o.now()) {
		wt, _ := o.worktreeOf(ctx, p, h.Branch)
		if o.branchTip(ctx, p, h.Branch) != h.Key.Tip || wt != h.Key.Worktree {
			continue
		}
		a.skip(logEntry{Repo: p.Common, Branch: h.Branch}, "snoozed until "+h.Until.UTC().Format("2006-01-02"))
	}
	for _, c := range cands {
		it := o.item(ctx, p, cwd, c)
		if it == nil {
			continue
		}
		it.callerCwd = callerCwd
		a.handle(ctx, it, grace)
	}
}

func (a *autoRun) handle(ctx context.Context, it *Item, grace time.Duration) {
	e := logEntry{Repo: it.Repo, Branch: it.Branch, Issue: it.Issues, PR: it.PR}
	if why := ineligible(it, grace); why != "" {
		a.skip(e, why)
		return
	}
	if a.dry {
		a.removed++
		a.log(logEntry{Action: "remove", Repo: e.Repo, Branch: e.Branch, Issue: e.Issue, PR: e.PR, Reason: it.Reason})
		return
	}
	dir, err := a.o.remove(ctx, it, a.stderr)
	e.Archive = dir
	switch {
	case err == nil:
		a.removed++
		e.Action, e.Reason = "removed", it.Reason
		a.log(e)
	case errors.Is(err, errBranchKept):
		a.fails++
		e.Action, e.Reason = "error", err.Error()
		a.log(e)
	default:
		a.skip(e, err.Error())
	}
}

// ineligible is why --auto must leave it, "" when its work is settled: it is
// stale, past the grace period, and every issue and PR it names is closed or
// merged. A state gh could not read blocks it.
func ineligible(it *Item, grace time.Duration) string {
	if !it.Stale {
		return "not stale: " + it.Reason
	}
	switch {
	case it.AgeS <= 0:
		return "no status on the bus: its age is unknown"
	case time.Duration(it.AgeS)*time.Second < grace:
		return fmt.Sprintf("inside the grace period (%s old, needs %s)", age(it.AgeS), age(int64(grace/time.Second)))
	}
	for _, i := range it.Issues {
		if i.State != "CLOSED" {
			return fmt.Sprintf("issue %s is %s", i.Ref, i.State)
		}
	}
	if it.PR != nil && it.PR.State != "MERGED" && it.PR.State != "CLOSED" {
		return fmt.Sprintf("PR %s is %s", it.PR.Ref, it.PR.State)
	}
	if len(it.Issues) == 0 && it.PR == nil {
		return "no issue or PR to show its work is settled"
	}
	return ""
}

func (a *autoRun) skip(e logEntry, reason string) {
	a.skipped++
	e.Action, e.Reason = "skipped", reason
	a.log(e)
}

// prune removes the archives older than d and logs each.
func (a *autoRun) prune(d time.Duration) {
	dirs, failed, err := a.o.sweep(d, a.dry, a.stderr)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.fails++
		a.log(logEntry{Action: "error", Reason: "prune: " + err.Error()})
		return
	}
	a.fails += failed
	for _, dir := range dirs {
		a.pruned++
		a.log(logEntry{Action: "pruned", Archive: dir, Reason: "older than " + age(int64(d/time.Second))})
	}
}

// log prints the action and, outside a dry run, appends it to the auto log.
func (a *autoRun) log(e logEntry) {
	e.TS = a.o.stamp()
	line := e.Action
	if v, ok := dryVerb[e.Action]; ok && a.dry {
		line = v
	}
	say(a.stdout, "%s %s %s", line, plain(e.Branch), plain(e.Archive))
	if e.Reason != "" {
		say(a.stdout, " (%s)", plain(e.Reason))
	}
	say(a.stdout, "\n")
	if !a.dry {
		a.write(e)
	}
}

// write appends one JSON line; a log that cannot be written is reported, not fatal.
func (a *autoRun) write(v any) {
	b, err := json.Marshal(v)
	if err == nil {
		err = appendLine(a.o.archiveRoot(), b)
	}
	if err != nil {
		say(a.stderr, "crew cleanup: auto log: %v\n", err)
	}
}

func appendLine(root string, line []byte) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, autoLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	return errors.Join(werr, f.Close())
}

// lastAutoSummary is the newest summary line in the log's tail.
func (o Options) lastAutoSummary() (summary, bool) {
	f, err := os.Open(filepath.Join(o.archiveRoot(), autoLogName))
	if err != nil {
		return summary{}, false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return summary{}, false
	}
	buf := make([]byte, min(fi.Size(), summaryTail))
	if _, err := f.ReadAt(buf, fi.Size()-int64(len(buf))); err != nil && !errors.Is(err, io.EOF) {
		return summary{}, false
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var s summary
		if json.Unmarshal(lines[i], &s) == nil && s.Action == "summary" {
			return s, true
		}
	}
	return summary{}, false
}
