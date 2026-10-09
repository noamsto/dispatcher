package stall

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

// epochRe is the arm's `^[0-9]{1,12}$` guard on an epoch it does arithmetic on.
var epochRe = regexp.MustCompile(`^[0-9]{1,12}$`)

// epoch reads a guarded epoch base 10, so a stamp like `09` is nine.
func epoch(s string) (int64, bool) {
	if !epochRe.MatchString(s) {
		return 0, false
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n, true
}

// cacheValue is the budget cache as one JSON value; ok=false when it is
// missing, unreadable or not exactly one value, which every jq read of it
// treated as no answer.
func (w *watch) cacheValue() (jsonv.Value, bool) {
	data, err := os.ReadFile(w.o.BudgetFile)
	if err != nil {
		return jsonv.Value{}, false
	}
	vs, err := jsonv.DecodeStream(bytes.NewReader(data))
	if err != nil || len(vs) != 1 {
		return jsonv.Value{}, false
	}
	return vs[0], true
}

// lastTry is `_budget_last_try`: the later of the cache's fetched_epoch and
// the last refresh attempt's stamp; an unreadable value counts as never.
func (w *watch) lastTry() int64 {
	var fetched int64
	if v, ok := w.cacheValue(); ok {
		fe, _ := v.Get("fetched_epoch")
		if x, ok := fe.AsFloat(); ok {
			if f := math.Floor(x); f >= 0 && f < 1e12 {
				fetched = int64(f)
			}
		}
	}
	var stamp int64
	if data, err := os.ReadFile(w.o.BudgetFile + ".refresh-at"); err == nil {
		stamp, _ = epoch(strings.TrimRight(string(data), "\n"))
	}
	return max(fetched, stamp)
}

// refreshMaybe is `_budget_refresh_maybe`: run refresh-budget when the cache
// is --budget-refresh old. Otherwise only a human or the dispatcher's session
// start refreshes it, so a window crossed mid-run would go unseen until the
// gates read it as stale. Single flight host-wide (the lock) plus the attempt
// stamp: N watchers cost one probe per interval, and a failing probe backs off
// the full interval instead of being retried every minute by every watcher.
// Synchronous on purpose: one tick slipping ≤120s once per interval is far
// inside every detector threshold. true means the probe ran, and the caller
// re-reads the bus.
//
// The lock is released on every path once held — a signal cancels ctx, the
// probe's process group is killed, and the deferred release still runs; the
// arm leaked the directory when it was killed mid-probe.
func (w *watch) refreshMaybe() (bool, error) {
	every := w.cfg.budgetRefresh
	if every == 0 || w.now-w.lastTry() < every {
		return false, nil
	}
	file := w.o.BudgetFile
	// The arm ran this in `if` context, errexit off: a failed mkdir only fails
	// the lock below, and a stamp that is not written still lets the probe run.
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
	dir := file + ".refresh.d"
	if !lock.Acquire(dir, strconv.Itoa(w.o.PID)) {
		return false, nil
	}
	defer lock.Release(dir)
	if w.now-w.lastTry() < every {
		return false, nil
	}
	stamp := file + ".refresh-at"
	tmp := stamp + "." + strconv.Itoa(w.o.PID)
	_ = os.WriteFile(tmp, []byte(strconv.FormatInt(w.now, 10)+"\n"), 0o644)
	_ = os.Rename(tmp, stamp)
	w.p.RefreshBudget(w.ctx)
	if err := w.cancelled(); err != nil {
		return false, err
	}
	return true, nil
}

// reltime is `_budget_reltime`, which mirrors refresh-budget's `reltime` jq
// def so the watchdog row and the budget report word a reset alike.
func reltime(secs int64) string {
	d, h, m := secs/86400, secs%86400/3600, secs%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// resetNote is `_budget_reset_note`. jq can hand back a float or an exponent
// literal (`1E+10`), so anything but a plain future epoch drops the relative
// part.
func resetNote(resets, iso string, now int64) string {
	r, _, _ := strings.Cut(resets, ".")
	if n, ok := epoch(r); ok && n > now {
		return fmt.Sprintf(" (resets %s, in %s)", iso, reltime(n-now))
	}
	return fmt.Sprintf(" (resets %s)", iso)
}

// readTab is `IFS=$'\t' read -r` into n names: tab is IFS whitespace, so a
// run of tabs is one separator and leading and trailing tabs are dropped; the
// last name takes the rest of the line.
func readTab(line string, n int) []string {
	out := make([]string, n)
	rest := strings.Trim(line, "\t")
	for i := 0; i < n-1 && rest != ""; i++ {
		field, after, _ := strings.Cut(rest, "\t")
		out[i] = field
		rest = strings.TrimLeft(after, "\t")
	}
	out[n-1] = rest
	return out
}

// budgetDetail is `_budget_detail`: the budget: detail for the first gating
// window, else the limit. rc 0 exhausted, 1 clear, 2 can't tell.
func (w *watch) budgetDetail() (string, int, error) {
	now := strconv.FormatInt(w.now, 10)
	win, wrc, wran, err := w.shCall("budget", "windows", w.o.BudgetFile, w.cfg.engine, now)
	if err != nil {
		return "", 0, err
	}
	lim, lrc, lran, err := w.shCall("budget", "limit", w.o.BudgetFile, w.cfg.engine, now)
	if err != nil {
		return "", 0, err
	}
	// A predicate that failed to run can't tell: it holds the episode, where
	// reading its status as clear would close it falsely.
	if !wran {
		wrc = 2
	}
	if !lran {
		lrc = 2
	}
	var detail string
	switch {
	case wrc == 0:
		line, _, _ := strings.Cut(win, "\n")
		f := readTab(line, 4)
		key, pct, resets, iso := f[0], f[1], f[2], f[3]
		detail = "budget: " + w.cfg.engine + " " + key + " at " + pct + "%"
		if resets != "" {
			detail += resetNote(resets, iso, w.now)
		} else {
			detail += " (no reset time)"
		}
	case lrc == 0:
		line, _, _ := strings.Cut(lim, "\n")
		f := readTab(line, 3)
		reason, resets, iso := f[0], f[1], f[2]
		detail = "budget: " + w.cfg.engine + " limit reached: " + reason
		if resets != "" {
			detail += resetNote(resets, iso, w.now)
		}
	case wrc == 2 || lrc == 2:
		return "", 2, nil
	default:
		return "", 1, nil
	}
	if w.creditsCover() {
		detail += " — credits cover: may be drawing paid credits"
	}
	return detail, 0, nil
}

// creditsCover is `.engines[$e].credits_cover == true` over the cache.
func (w *watch) creditsCover() bool {
	v, ok := w.cacheValue()
	if !ok {
		return false
	}
	engines, _ := v.Get("engines")
	e, _ := engines.Get(w.cfg.engine)
	cover, _ := e.Get("credits_cover")
	return cover.Kind() == jsonv.KindTrue
}
