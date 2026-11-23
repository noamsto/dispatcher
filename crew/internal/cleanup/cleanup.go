// Package cleanup is `crew cleanup`: the interactive, human-only review of the
// stale worker worktrees `crew reap` keeps and flags "needs human cleanup".
// Reap's own classifier decides what is listed; removal archives the work
// first, then removes the way reap's `remove:` line does.
package cleanup

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/snooze"
)

// Usage is the one-line usage the refusals quote.
const Usage = "usage: crew cleanup [--fleet] [--repo DIR] [--list] [--json] | --prune-archive [--older-than AGE]"

// DefaultOlderThan is --prune-archive's default age.
const DefaultOlderThan = "30d"

// Request is `crew cleanup`'s parsed flags.
type Request struct {
	Fleet, List, JSON, PruneArchive bool
	Repo, OlderThan                 string
}

// Options is everything `crew cleanup` reads from outside its arguments.
type Options struct {
	Reap  reap.Options
	Fleet reap.FleetOptions
	// Getenv reads CREW_WORKER_ID, CREW_ROLE_ID, PAGER, HOME and XDG_DATA_HOME.
	Getenv func(string) string
	Stdin  io.Reader
	// Interactive reports whether stdin and stdout are both terminals.
	Interactive func() bool
	// Pager shows text in $PAGER; nil runs the real one.
	Pager func(ctx context.Context, text string) error
}

func (o Options) now() time.Time {
	if o.Reap.Probes.Clock.Now != nil {
		return o.Reap.Probes.Clock.Now()
	}
	return time.Now()
}

func say(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// refusal is why this call may not run, "" when it may. Only the read-only
// --list/--json are open to workers, roles and pipes.
func (o Options) refusal(req Request) string {
	if req.List || req.JSON {
		return ""
	}
	for _, v := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID"} {
		if o.Getenv(v) != "" {
			return "crew cleanup: refusing — " + v + " is set; this session is a worker or role pane, and cleanup is human-only (--list and --json are read-only and allowed)"
		}
	}
	if o.Interactive == nil || !o.Interactive() {
		return "crew cleanup: refusing — stdin and stdout must both be a terminal; cleanup is interactive and human-only (--list and --json are read-only and allowed)"
	}
	return ""
}

// Run is `crew cleanup`. It exits 1 on a refusal or failure, 0 otherwise.
func Run(ctx context.Context, req Request, cwd string, stdout, stderr io.Writer, o Options) int {
	if why := o.refusal(req); why != "" {
		say(stderr, "%s\n", why)
		return 1
	}
	if req.PruneArchive {
		age, err := ParseAge(cmp.Or(req.OlderThan, DefaultOlderThan))
		if err != nil {
			say(stderr, "crew cleanup: %v\n", err)
			return 1
		}
		return o.prune(age, stdout, stderr)
	}

	buses, cwds, code := o.repos(ctx, req, cwd, stderr)
	if code != 0 {
		return code
	}
	items, code := o.gather(ctx, buses, cwds, cwd, stderr)
	if code != 0 {
		return code
	}
	slices.SortStableFunc(items, func(a, b *Item) int {
		return cmp.Or(strings.Compare(a.Repo, b.Repo), cmp.Compare(b.AgeS, a.AgeS), strings.Compare(a.Branch, b.Branch))
	})

	switch {
	case req.JSON:
		if items == nil {
			items = []*Item{}
		}
		b, _ := json.Marshal(struct {
			Items []*Item `json:"items"`
		}{items})
		say(stdout, "%s\n", b)
		return 0
	case req.List:
		for _, it := range items {
			it.render(stdout)
		}
		say(stdout, "crew cleanup: %d worktree(s) to review\n", len(items))
		return 0
	}
	return o.review(ctx, items, stdout, stderr)
}

// repos is the buses to cover and the checkout each reaps from.
func (o Options) repos(ctx context.Context, req Request, cwd string, stderr io.Writer) ([]bus.Paths, map[string]string, int) {
	cwds := map[string]string{}
	if req.Fleet {
		buses := reap.FleetBuses(stderr, o.Fleet)
		for _, p := range buses {
			cwds[p.Common] = filepath.Dir(p.Common)
		}
		return buses, cwds, 0
	}
	dir := cwd
	if req.Repo != "" {
		abs, err := filepath.Abs(req.Repo)
		if err != nil {
			say(stderr, "crew cleanup: --repo %s: %v\n", req.Repo, err)
			return nil, nil, 1
		}
		dir = abs
	}
	p, err := bus.Locate(ctx, dir)
	if err != nil {
		say(stderr, "crew: not in a git repo\n")
		return nil, nil, 1
	}
	cwds[p.Common] = dir
	return []bus.Paths{p}, cwds, 0
}

// review walks the items, one key each.
func (o Options) review(ctx context.Context, items []*Item, stdout, stderr io.Writer) int {
	if len(items) == 0 {
		say(stdout, "crew cleanup: nothing to review\n")
		return 0
	}
	in := bufio.NewReader(o.Stdin)
	var removed, kept, skipped, failed int
	left := 0
walk:
	for i, it := range items {
		say(stdout, "\n[%d/%d] ", i+1, len(items))
		it.render(stdout)
		for {
			say(stdout, "  [r]emove  [k]eep %dd  [d]iff  [s]kip  [q]uit > ", int(snooze.Default/(24*time.Hour)))
			line, err := in.ReadString('\n')
			key := firstKey(line)
			if err != nil && key == 0 {
				say(stdout, "\n")
				left = len(items) - i
				break walk
			}
			switch key {
			case 'r':
				dir, err := o.remove(ctx, it, stderr)
				if err == nil {
					removed++
					say(stdout, "removed %s — recovery bundle: %s\n", it.Branch, dir)
					break
				}
				failed++
				say(stderr, "crew cleanup: %s: %v\n", it.Branch, err)
				if dir != "" {
					say(stdout, "recovery bundle: %s\n", dir)
				}
			case 'k':
				now := o.now()
				if err := snooze.Set(it.paths.Dir, it.Branch, it.snoozeKey(), now, now.Add(snooze.Default)); err != nil {
					failed++
					say(stderr, "crew cleanup: %s: could not record the snooze: %v\n", it.Branch, err)
					break
				}
				kept++
				say(stdout, "kept %s — not raised again until %s\n", it.Branch, now.Add(snooze.Default).UTC().Format("2006-01-02"))
			case 'd':
				if err := o.showDiff(ctx, it, stderr); err != nil {
					say(stderr, "crew cleanup: %s: %v\n", it.Branch, err)
				}
				continue
			case 's':
				skipped++
				say(stdout, "skipped %s\n", it.Branch)
			case 'q':
				left = len(items) - i
				break walk
			default:
				say(stdout, "  r, k, d, s or q\n")
				continue
			}
			break
		}
	}
	say(stdout, "crew cleanup: removed %d, kept %d, skipped %d", removed, kept, skipped)
	if failed > 0 {
		say(stdout, ", failed %d", failed)
	}
	if left > 0 {
		say(stdout, ", %d left unreviewed", left)
	}
	say(stdout, "\n")
	if failed > 0 {
		return 1
	}
	return 0
}

func firstKey(line string) rune {
	for _, r := range line {
		if !unicode.IsSpace(r) {
			return unicode.ToLower(r)
		}
	}
	return 0
}

// showDiff pages the full uncommitted diff, untracked files included.
func (o Options) showDiff(ctx context.Context, it *Item, stderr io.Writer) error {
	tmp, err := os.MkdirTemp("", "crew-cleanup-diff-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	text, err := o.patchText(ctx, it, tmp, stderr)
	if err != nil {
		return err
	}
	if text == "" {
		text = "no uncommitted changes\n"
	}
	if o.Pager != nil {
		return o.Pager(ctx, text)
	}
	return o.runPager(ctx, text)
}

func (o Options) runPager(ctx context.Context, text string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmp.Or(o.Getenv("PAGER"), "less -R"))
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// ParseAge reads "30d", "12h" or "2w".
func ParseAge(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("--older-than %q: want a number and h, d or w (30d)", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	unit := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[s[len(s)-1]]
	if err != nil || n < 0 || unit == 0 || n > int64(time.Duration(1<<62)/unit) {
		return 0, fmt.Errorf("--older-than %q: want a number and h, d or w (30d)", s)
	}
	return time.Duration(n) * unit, nil
}
