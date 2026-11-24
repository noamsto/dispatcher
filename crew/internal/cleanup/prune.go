package cleanup

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// stampRe is the archive dir's trailing UTC stamp.
var stampRe = regexp.MustCompile(`-(\d{8}T\d{6}Z)$`)

// prune is `--prune-archive`: sweep, then report.
func (o Options) prune(age time.Duration, stdout, stderr io.Writer) int {
	pruned, failed, err := o.sweep(age, false, stderr)
	if errors.Is(err, fs.ErrNotExist) {
		say(stdout, "crew cleanup: no archive to prune\n")
		return 0
	}
	if err != nil {
		say(stderr, "crew cleanup: %v\n", err)
		return 1
	}
	for _, dir := range pruned {
		say(stdout, "pruned %s\n", dir)
	}
	say(stdout, "crew cleanup: pruned %d archive(s) older than %s\n", len(pruned), age)
	if failed > 0 {
		return 1
	}
	return 0
}

// sweep deletes the archives older than age: the <repo>/<branch>-<ts> dirs
// under the archive root, judged by their stamp (mtime when it has none). It
// returns the dirs it pruned (would prune, when dry) and how many it failed
// on; err is an unreadable archive root.
func (o Options) sweep(age time.Duration, dry bool, stderr io.Writer) (pruned []string, failed int, err error) {
	root := o.archiveRoot()
	repos, err := os.ReadDir(root)
	if err != nil {
		return nil, 0, err
	}
	cutoff := o.now().Add(-age)
	for _, repo := range repos {
		repoDir := filepath.Join(root, repo.Name())
		if !repo.IsDir() {
			continue
		}
		// A scratch dir a killed cleanup left behind; a fresh one is in use.
		if strings.HasPrefix(repo.Name(), ".tmp-") {
			if fi, err := os.Stat(repoDir); err == nil && !dry && fi.ModTime().Before(o.now().Add(-time.Hour)) {
				_ = os.RemoveAll(repoDir)
			}
			continue
		}
		entries, err := os.ReadDir(repoDir)
		if err != nil {
			say(stderr, "crew cleanup: %v\n", err)
			failed++
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(repoDir, e.Name())
			// Lstat-typed: a symlink is never followed out of the archive.
			if !e.Type().IsDir() || !archivedBefore(dir, e.Name(), cutoff) {
				continue
			}
			if !dry {
				if err := os.RemoveAll(dir); err != nil {
					say(stderr, "crew cleanup: %v\n", err)
					failed++
					continue
				}
			}
			pruned = append(pruned, dir)
		}
		if !dry {
			_ = os.Remove(repoDir) // only succeeds once empty
		}
	}
	return pruned, failed, nil
}

func archivedBefore(dir, name string, cutoff time.Time) bool {
	if m := stampRe.FindStringSubmatch(name); m != nil {
		if t, err := time.Parse("20060102T150405Z", m[1]); err == nil {
			return t.Before(cutoff)
		}
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.ModTime().Before(cutoff)
}
