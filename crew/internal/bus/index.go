package bus

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/lock"
)

// IndexHeader is line 1 of a repo index file.
type IndexHeader struct {
	V      int    `json:"v"`
	Common string `json:"common"`
	Slug   string `json:"slug"`
}

// IndexEntry is each later line: a layout-2 crew that dispatched in the repo.
type IndexEntry struct {
	Crew string `json:"crew"`
	TS   int64  `json:"ts"`
}

const indexVersion = 1

func indexDir(state string) string { return state + "/index/repos" }

// IndexPath is the index file of the repo known by repoTag, keyed by the full
// sha256 hex of the tag as policy.Dir keys a repo's policies.
func IndexPath(state, repoTag string) string {
	sum := sha256.Sum256([]byte(repoTag))
	return indexDir(state) + "/" + hex.EncodeToString(sum[:])
}

// IndexAdd lists id in repoTag's index, writing the header first when the file
// is new. An id already listed is left alone (added false); otherwise the entry
// is one O_APPEND write. It runs under state/gc.lock.d, so an IndexRebuild
// replacing or removing the file cannot drop the append.
func IndexAdd(state, repoTag, slug, id string, now time.Time) (bool, error) {
	if !ValidCrewID(id) {
		return false, &InvalidCrewIDError{ID: id}
	}
	dir := indexDir(state)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	release, err := acquireGCLock(state)
	if err != nil {
		return false, err
	}
	defer release()
	path := IndexPath(state, repoTag)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		hdr, err := json.Marshal(IndexHeader{V: indexVersion, Common: repoTag, Slug: slug})
		if err != nil {
			return false, err
		}
		if err := linkNew(dir, ".index.", path, append(hdr, '\n')); err != nil && !errors.Is(err, fs.ErrExist) {
			return false, err
		}
	}
	if slices.Contains(readIndex(path), id) {
		return false, nil
	}
	line, err := json.Marshal(IndexEntry{Crew: id, TS: now.UnixMilli()})
	if err != nil {
		return false, err
	}
	return true, Append(path, string(line))
}

// IndexCrews is the ids repoTag's index lists, deduplicated, in file order.
func IndexCrews(state, repoTag string) []string { return readIndex(IndexPath(state, repoTag)) }

// IndexListed reports whether any repo index lists id.
func IndexListed(state, id string) bool {
	entries, err := os.ReadDir(indexDir(state))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if indexFileName(e.Name()) && slices.Contains(readIndex(indexDir(state)+"/"+e.Name()), id) {
			return true
		}
	}
	return false
}

// indexFileName excludes the dot-prefixed temp files IndexAdd and IndexRebuild
// write beside the index files.
func indexFileName(name string) bool { return !strings.HasPrefix(name, ".") }

// readIndex is the valid crew ids an index file lists, deduplicated, in file
// order. A file without a v1 header lists nothing; an unparsable entry line is
// skipped.
func readIndex(path string) []string {
	if !isFile(path) {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := bytes.Split(data, []byte("\n"))
	var hdr IndexHeader
	if json.Unmarshal(lines[0], &hdr) != nil || hdr.V != indexVersion {
		return nil
	}
	var ids []string
	for _, line := range lines[1:] {
		var e IndexEntry
		if json.Unmarshal(line, &e) != nil || !ValidCrewID(e.Crew) || slices.Contains(ids, e.Crew) {
			continue
		}
		ids = append(ids, e.Crew)
	}
	return ids
}

// gcLockWait bounds how long an index writer waits for another gc.lock.d
// holder.
var gcLockWait = 10 * time.Second

// acquireGCLock takes state/gc.lock.d, polling until gcLockWait runs out.
func acquireGCLock(state string) (release func(), err error) {
	lockDir := state + "/gc.lock.d"
	owner := strconv.Itoa(os.Getpid())
	deadline := time.Now().Add(gcLockWait)
	for !lock.Acquire(lockDir, owner) {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held by pid %s", lockDir, lock.Holder(lockDir))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() { lock.Release(lockDir) }, nil
}

// IndexRebuild regenerates every repo index from the logs: a crew with a
// crews/ or archive/ meta.json is listed for each repo its own dispatch rows
// name, so an entry no log backs is dropped. Each file is replaced by temp +
// rename and files for no repo are removed, all under state/gc.lock.d. slugOf
// gives each header's slug from the repo's common dir.
func IndexRebuild(state string, slugOf func(common string) string) error {
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	release, err := acquireGCLock(state)
	if err != nil {
		return err
	}
	defer release()

	repos := map[string]map[string]int64{} // repo → crew → earliest dispatch ts
	for _, root := range []string{state + "/crews", state + "/archive"} {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			id := e.Name()
			if !ValidCrewID(id) {
				continue
			}
			dir := root + "/" + id
			if _, err := readMeta(metaPath(dir), id); err != nil {
				continue
			}
			for repo, ts := range dispatchRepos(dir, id) {
				if repos[repo] == nil {
					repos[repo] = map[string]int64{}
				}
				if old, ok := repos[repo][id]; !ok || ts < old {
					repos[repo][id] = ts
				}
			}
		}
	}
	return writeIndexes(state, repos, slugOf)
}

// dispatchRepos is each repo the crew's own dispatch rows name in dir's log
// (plain and gzipped), with the earliest such row's ts.
func dispatchRepos(dir, id string) map[string]int64 {
	repos := map[string]int64{}
	for _, ev := range EventsFromLines(logLines(dir)) {
		obj, ok := ev.Doc.(map[string]any)
		if !ok || ev.Kind != KindDispatch || ev.CrewID != id {
			continue
		}
		repo := stringField(obj, "repo")
		if repo == "" {
			continue
		}
		var ts int64
		if n, ok := obj["ts"].(json.Number); ok {
			ts, _ = n.Int64()
		}
		if old, ok := repos[repo]; !ok || ts < old {
			repos[repo] = ts
		}
	}
	return repos
}

func logLines(dir string) []string {
	var data []byte
	if b, err := os.ReadFile(dir + "/events.jsonl"); err == nil {
		data = append(data, b...)
		data = append(data, '\n')
	}
	if f, err := os.Open(dir + "/events.jsonl.gz"); err == nil {
		if zr, err := gzip.NewReader(f); err == nil {
			b, _ := io.ReadAll(zr)
			data = append(data, b...)
		}
		_ = f.Close()
	}
	return strings.Split(string(data), "\n")
}

func writeIndexes(state string, repos map[string]map[string]int64, slugOf func(string) string) error {
	dir := indexDir(state)
	if len(repos) > 0 {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	keep := map[string]bool{}
	for repo, crews := range repos {
		entries := make([]IndexEntry, 0, len(crews))
		for id, ts := range crews {
			entries = append(entries, IndexEntry{Crew: id, TS: ts})
		}
		slices.SortFunc(entries, func(a, b IndexEntry) int {
			return cmp.Or(cmp.Compare(a.TS, b.TS), strings.Compare(a.Crew, b.Crew))
		})
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		if err := enc.Encode(IndexHeader{V: indexVersion, Common: repo, Slug: slugOf(repo)}); err != nil {
			return err
		}
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		path := IndexPath(state, repo)
		if err := replaceFile(dir, path, buf.Bytes()); err != nil {
			return err
		}
		keep[path] = true
	}
	stale, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range stale {
		if path := dir + "/" + e.Name(); indexFileName(e.Name()) && !keep[path] {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// replaceFile writes data to path by temp + rename, so a reader sees the old
// file or the new one, never a partial one.
func replaceFile(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".index.")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
