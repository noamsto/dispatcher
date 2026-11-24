package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/snooze"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const day = 24 * 3600

type autoLogLine struct {
	Action  string `json:"action"`
	Branch  string `json:"branch"`
	Archive string `json:"archive"`
	Reason  string `json:"reason"`
	Removed int    `json:"removed"`
	Skipped int    `json:"skipped"`
	Errors  int    `json:"errors"`
	Pruned  int    `json:"pruned"`
}

func (f *fx) archiveRoot() string {
	return filepath.Join(f.env["XDG_DATA_HOME"], "crew", "cleanup-archive")
}

// auto runs `crew cleanup --auto` with the given settings (nil: the defaults).
func (f *fx) auto(req Request, s *Settings, settingsErr error) int {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	o := f.opts()
	if s != nil || settingsErr != nil {
		o.Settings = func(context.Context) (Settings, error) {
			if s == nil {
				return Settings{}, settingsErr
			}
			return *s, settingsErr
		}
	}
	req.Auto = true
	return Run(context.Background(), req, f.main, &f.stdout, &f.stderr, o)
}

func (f *fx) autoLog() []autoLogLine {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.archiveRoot(), "auto.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []autoLogLine
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var e autoLogLine
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			f.t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

func (f *fx) wantKept() {
	f.t.Helper()
	if _, err := os.Stat(f.wt); err != nil {
		f.t.Fatalf("worktree gone: %v", err)
	}
	if got := runGit(f.t, f.main, "branch", "--list", branch); got == "" {
		f.t.Fatal("branch gone")
	}
}

func (f *fx) wantSkip(prefix string) {
	f.t.Helper()
	f.wantKept()
	for _, e := range f.autoLog() {
		if e.Action == "skipped" && e.Branch == branch && strings.HasPrefix(e.Reason, prefix) {
			return
		}
	}
	f.t.Fatalf("no skipped line starting %q in %+v", prefix, f.autoLog())
}

func TestAutoRemovesSettledStaleItem(t *testing.T) {
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	f.dirty(true)
	f.tty = false
	if rc := f.auto(Request{}, nil, nil); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
		t.Fatalf("worktree survived: %v", err)
	}
	if got := runGit(t, f.main, "branch", "--list", branch); got != "" {
		t.Fatalf("branch survived: %q", got)
	}
	dirs := f.archiveDirs()
	if len(dirs) != 1 {
		t.Fatalf("archive dirs %v", dirs)
	}
	runGit(t, f.main, "bundle", "verify", filepath.Join(dirs[0], "commits.bundle"))
	if fi, err := os.Stat(filepath.Join(dirs[0], "uncommitted.patch")); err != nil || fi.Size() == 0 {
		t.Fatalf("patch %v %v", fi, err)
	}
	log := f.autoLog()
	if len(log) != 2 || log[0].Action != "removed" || log[0].Branch != branch || log[0].Archive != dirs[0] ||
		log[1].Action != "summary" || log[1].Removed != 1 || log[1].Skipped != 0 || log[1].Errors != 0 {
		t.Fatalf("log %+v", log)
	}
	fi, err := os.Stat(filepath.Join(f.archiveRoot(), "auto.log"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("auto.log %v %v", fi, err)
	}
}

func TestAutoSkipsAreLoggedWithReason(t *testing.T) {
	t.Run("open issue", func(t *testing.T) {
		f := newFx(t)
		f.bus(15 * day)
		f.dirty(false)
		f.gh["issue view 5 --json state --jq .state"] = "OPEN"
		if rc := f.auto(Request{}, nil, nil); rc != 0 {
			t.Fatalf("rc=%d %s", rc, f.stderr.String())
		}
		f.wantSkip("issue #5")
	})
	t.Run("inside grace", func(t *testing.T) {
		f := newFx(t)
		f.bus(3 * day)
		f.dirty(false)
		if rc := f.auto(Request{}, nil, nil); rc != 0 {
			t.Fatalf("rc=%d %s", rc, f.stderr.String())
		}
		f.wantSkip("inside the grace period")
	})
	t.Run("older-than overrides the grace", func(t *testing.T) {
		f := newFx(t)
		f.bus(3 * day)
		f.dirty(false)
		if rc := f.auto(Request{OlderThan: "2d"}, nil, nil); rc != 0 {
			t.Fatalf("rc=%d %s", rc, f.stderr.String())
		}
		if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
			t.Fatalf("worktree survived: %v", err)
		}
	})
	t.Run("snoozed", func(t *testing.T) {
		f := newFx(t)
		f.bus(15 * day)
		f.dirty(false)
		key := snooze.Key{Tip: runGit(t, f.wt, "rev-parse", "HEAD"), Worktree: f.wt}
		if err := snooze.Set(f.paths.Dir, branch, key, f.now, f.now.Add(snooze.Default)); err != nil {
			t.Fatal(err)
		}
		if rc := f.auto(Request{}, nil, nil); rc != 0 {
			t.Fatalf("rc=%d %s", rc, f.stderr.String())
		}
		f.wantSkip("snoozed until ")
	})
	t.Run("failed archive", func(t *testing.T) {
		f := newFx(t)
		f.bus(15 * day)
		f.dirty(false)
		if err := os.MkdirAll(f.archiveRoot(), 0o755); err != nil {
			t.Fatal(err)
		}
		// Snapshot cannot make its per-repo dir under a regular file.
		if err := os.WriteFile(filepath.Join(f.archiveRoot(), "main"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if rc := f.auto(Request{}, nil, nil); rc != 0 {
			t.Fatalf("rc=%d %s", rc, f.stderr.String())
		}
		f.wantSkip("archive failed")
		if dirs := f.archiveDirs(); len(dirs) != 0 {
			t.Fatalf("archive left behind: %v", dirs)
		}
	})
}

func TestAutoDisabledAndSettingsError(t *testing.T) {
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	if rc := f.auto(Request{}, &Settings{Auto: false, AutoAfterDays: 14, ArchiveDays: 365}, nil); rc != 0 {
		t.Fatalf("disabled rc=%d %s", rc, f.stderr.String())
	}
	if !strings.Contains(f.stdout.String(), "disabled") {
		t.Fatalf("stdout %q", f.stdout.String())
	}
	f.wantKept()
	if rc := f.auto(Request{}, nil, errors.New("dispatch-config exploded")); rc != 1 || !strings.Contains(f.stderr.String(), "dispatch-config exploded") {
		t.Fatalf("settings error rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantKept()
	if log := f.autoLog(); len(log) != 0 {
		t.Fatalf("log %+v", log)
	}
}

func TestAutoWithoutTTYAndWorkerRefusal(t *testing.T) {
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	f.tty = false
	if rc := f.auto(Request{}, nil, nil); rc != 0 {
		t.Fatalf("no tty rc=%d %s", rc, f.stderr.String())
	}
	if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
		t.Fatalf("worktree survived: %v", err)
	}
	g := newFx(t)
	g.bus(15 * day)
	g.dirty(false)
	g.env["CREW_WORKER_ID"] = "worker:x#s1"
	if rc := g.auto(Request{}, nil, nil); rc != 1 || !strings.Contains(g.stderr.String(), "CREW_WORKER_ID is set") {
		t.Fatalf("worker rc=%d stderr=%q", rc, g.stderr.String())
	}
	g.wantKept()
}

func TestAutoPrunesExpiredArchives(t *testing.T) {
	for _, tc := range []struct {
		name        string
		archiveDays int
		survives    bool
	}{{"365 days", 365, false}, {"0 keeps forever", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			f.bus(3 * day)
			f.dirty(false)
			old := filepath.Join(f.archiveRoot(), "main", "feat-old-20200101T000000Z")
			if err := os.MkdirAll(old, 0o755); err != nil {
				t.Fatal(err)
			}
			if rc := f.auto(Request{}, &Settings{Auto: true, AutoAfterDays: 14, ArchiveDays: tc.archiveDays}, nil); rc != 0 {
				t.Fatalf("rc=%d %s", rc, f.stderr.String())
			}
			_, err := os.Stat(old)
			if tc.survives != (err == nil) {
				t.Fatalf("archive present=%v, want %v", err == nil, tc.survives)
			}
			var pruned int
			for _, e := range f.autoLog() {
				if e.Action == "pruned" && e.Archive == old {
					pruned++
				}
			}
			if tc.survives == (pruned == 1) {
				t.Fatalf("pruned lines %d in %+v", pruned, f.autoLog())
			}
		})
	}
}

func TestAutoDryRunChangesNothing(t *testing.T) {
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	f.dirty(true)
	old := filepath.Join(f.archiveRoot(), "main", "feat-old-20200101T000000Z")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if rc := f.auto(Request{DryRun: true}, nil, nil); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	f.wantKept()
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("dry run pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.archiveRoot(), "auto.log")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote a log: %v", err)
	}
	if dirs := f.archiveDirs(); len(dirs) != 1 || dirs[0] != old {
		t.Fatalf("archives %v", dirs)
	}
	out := f.stdout.String()
	if !strings.Contains(out, "would remove") || !strings.Contains(out, branch) || !strings.Contains(out, "would prune") {
		t.Fatalf("stdout %q", out)
	}
}

func TestAutoOneBrokenBusDoesNotStopTheRest(t *testing.T) {
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	broken := filepath.Join(f.root, "broken")
	runGit(t, f.root, "init", "-q", "-b", "main", broken)
	if err := os.MkdirAll(filepath.Join(broken, ".git", "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A worktree record that is not a regular file: reap refuses that repo, and
	// the other still runs.
	rec, err := wtgit.AnchorPath(broken)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".git", "crew", "events.jsonl"), []byte(`{"ts":1,"crew_id":"c1","from":"worker:feat/x#s1","to":"dispatcher:c1","kind":"status","body":{"state":"failed"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(f.root, "repos")
	if err := os.WriteFile(registry, []byte(filepath.Join(broken, ".git")+"\n"+f.paths.Common+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := f.opts()
	o.Fleet = reap.FleetOptions{Registry: registry, Getenv: func(k string) string { return f.env[k] }}
	var stdout, stderr bytes.Buffer
	rc := Run(context.Background(), Request{Auto: true, Fleet: true}, f.root, &stdout, &stderr, o)
	if rc != 1 {
		t.Fatalf("rc=%d stdout=%s stderr=%s", rc, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
		t.Fatalf("healthy repo not processed: %v\n%s", err, stderr.String())
	}
	var sawErr bool
	for _, e := range f.autoLog() {
		sawErr = sawErr || e.Action == "error"
	}
	if !sawErr {
		t.Fatalf("no error line in %+v", f.autoLog())
	}
}

func TestListShowsLastAutoSummary(t *testing.T) {
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	if rc := f.auto(Request{}, nil, nil); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	if rc := f.run(Request{List: true}); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	if !strings.Contains(f.stdout.String(), "last auto run") || !strings.Contains(f.stdout.String(), "removed 1") {
		t.Fatalf("stdout %q", f.stdout.String())
	}
	if rc := f.run(Request{List: true, JSON: true}); rc != 0 || strings.Contains(f.stdout.String(), "last auto run") {
		t.Fatalf("json rc=%d %q", rc, f.stdout.String())
	}
}

func TestVerifyArchiveRefusesIncompleteArchives(t *testing.T) {
	f := newFx(t)
	o := f.opts()
	it := &Item{Branch: branch, Worktree: f.wt, paths: f.paths}
	tip := runGit(t, f.wt, "rev-parse", "HEAD")
	headTree := runGit(t, f.wt, "rev-parse", "HEAD^{tree}")
	clean := f.cleanState(it)
	for _, tc := range []struct {
		name   string
		files  map[string]string
		before string
		ok     bool
	}{
		{"corrupt bundle", map[string]string{bundleName: "junk", infoName: `{}`}, headTree + "\n" + tip, false},
		{"missing patch", map[string]string{infoName: `{"has_uncommitted":true}`}, headTree + "\n" + tip, false},
		{"empty patch", map[string]string{infoName: `{"has_uncommitted":true}`, patchName: ""}, headTree + "\n" + tip, false},
		{"dirty tree without a patch", map[string]string{infoName: `{}`}, "0000000000000000000000000000000000000000\n" + tip, false},
		{"clean", map[string]string{infoName: `{}`}, clean, true},
		{"commits without a bundle", map[string]string{infoName: `{"has_commits":true}`}, clean, false},
		{"patch saved", map[string]string{infoName: `{"has_uncommitted":true}`, patchName: "diff"}, headTree + "\n" + tip, true},
	} {
		dir := t.TempDir()
		for name, body := range tc.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := o.verifyArchive(context.Background(), it, dir, tc.before, io.Discard); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// cleanState is treeState's value for the item's untouched worktree.
func (f *fx) cleanState(it *Item) string {
	f.t.Helper()
	admin, err := wtgit.AdminDir(f.paths.Common, f.wt)
	if err != nil {
		f.t.Fatal(err)
	}
	st, err := f.opts().treeState(context.Background(), admin, it, io.Discard)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

// commitOnly leaves one unpushed commit and a clean tree.
func (f *fx) commitOnly() {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.wt, "done.txt"), []byte("committed work\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, f.wt, "add", "done.txt")
	runGit(f.t, f.wt, "commit", "-q", "-m", "work")
}

func TestRemovesCleanWorktreeWithUnpushedCommit(t *testing.T) {
	for _, mode := range []string{"auto", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			f := newFx(t)
			f.bus(15 * day)
			f.commitOnly()
			var rc int
			if mode == "auto" {
				rc = f.auto(Request{}, nil, nil)
			} else {
				f.input = "r\n"
				rc = f.run(Request{})
			}
			if rc != 0 {
				t.Fatalf("rc=%d %s", rc, f.stderr.String())
			}
			if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
				t.Fatalf("worktree survived: %v\n%s%s", err, f.stdout.String(), f.stderr.String())
			}
			dirs := f.archiveDirs()
			if len(dirs) != 1 {
				t.Fatalf("archive dirs %v", dirs)
			}
			runGit(t, f.main, "bundle", "verify", filepath.Join(dirs[0], "commits.bundle"))
		})
	}
}

func TestAutoCountsErrorsPastTheArchiveAsFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	// git worktree remove cannot delete the worktree under a read-only parent.
	if err := os.Chmod(f.root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.root, 0o755) })
	rc := f.auto(Request{}, nil, nil)
	_ = os.Chmod(f.root, 0o755)
	if rc != 1 {
		t.Fatalf("rc=%d %s%s", rc, f.stdout.String(), f.stderr.String())
	}
	var errLine, sum bool
	for _, e := range f.autoLog() {
		errLine = errLine || e.Action == "error" && e.Branch == branch
		sum = sum || e.Action == "summary" && e.Errors == 1 && e.Skipped == 0
	}
	if !errLine || !sum {
		t.Fatalf("log %+v", f.autoLog())
	}
}

func TestAutoSkipsAnItemClaimingAnUncheckableIssue(t *testing.T) {
	f := newFx(t)
	f.bus(15 * day)
	f.dirty(false)
	row := fmt.Sprintf(`{"ts":2,"crew_id":"c1","kind":"claim-issue","branch":"%s","issue":"https://github.com/other/repo/issues/9"}`, branch)
	b, err := os.ReadFile(f.paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.paths.Log, append(b, row+"\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := f.auto(Request{}, nil, nil); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	f.wantSkip("it claims an issue gh cannot check")
}

func TestAutoTrimsOldAutoLogLines(t *testing.T) {
	f := newFx(t)
	f.bus(3 * day)
	if err := os.MkdirAll(f.archiveRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"ts":"2020-01-01T00:00:00Z","action":"skipped","branch":"old"}` + "\n"
	if err := os.WriteFile(filepath.Join(f.archiveRoot(), "auto.log"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := f.auto(Request{}, nil, nil); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	for _, e := range f.autoLog() {
		if e.Branch == "old" {
			t.Fatalf("old line kept: %+v", f.autoLog())
		}
	}
}
