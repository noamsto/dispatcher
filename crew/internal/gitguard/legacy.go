package gitguard

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// Legacy remotes/ and branches/ files are read by git outside the config API.
// Their records stay distinct from key/value records: config keys cannot begin
// with #, and the fixed fields make the namespace/kind/name/hash boundary
// unambiguous. Names are restricted to ordinary path components; anything
// else is represented as unsafe metadata and is never opened.
const legacyPrefix = "#legacy\n"

// access(2) modes; package syscall does not name them.
const (
	rOK = 0x4
	xOK = 0x1
)

func legacyRecord(ns, kind, name string) string {
	return legacyPrefix + ns + "\n" + kind + "\n" + name
}

func legacyNameSafe(name string) bool {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") || !utf8.ValidString(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x21 || name[i] == 0x7f {
			return false
		}
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// LegacyRecords lists a record per direct entry of common's legacy remotes/ and
// branches/ namespaces: `#legacy\n<ns>\n<kind>\n<name>`, plus `\n<sha256>` for
// kind `file`. A symlinked or unlistable namespace is one record named `.`.
func LegacyRecords(common string) []string {
	var recs []string
	for _, ns := range []string{"remotes", "branches"} {
		dir := common + "/" + ns
		fi, err := os.Lstat(dir)
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			recs = append(recs, legacyRecord(ns, "symlink", "."))
			continue
		}
		if !fi.IsDir() {
			continue
		}
		// An unlistable namespace hides its entries, yet git still opens them by path.
		if syscall.Access(dir, rOK|xOK) != nil {
			recs = append(recs, legacyRecord(ns, "unreadable-dir", "."))
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			recs = append(recs, legacyRecord(ns, "unreadable-dir", "."))
			continue
		}
		for _, e := range entries {
			recs = append(recs, legacyEntryRecord(dir, ns, e.Name()))
		}
	}
	return recs
}

func legacyEntryRecord(dir, ns, name string) string {
	if !legacyNameSafe(name) {
		return legacyRecord(ns, "unsafe-name", Quote(name))
	}
	path := dir + "/" + name
	fi, err := os.Lstat(path)
	if err != nil {
		return legacyRecord(ns, "other", name)
	}
	m := fi.Mode()
	switch {
	case m&fs.ModeSymlink != 0:
		return legacyRecord(ns, "symlink", name)
	case m.IsRegular():
		hash, ok := hashFile(path)
		if !ok {
			return legacyRecord(ns, "unreadable", name)
		}
		return legacyRecord(ns, "file", name) + "\n" + hash
	case m.IsDir():
		return legacyRecord(ns, "directory", name)
	case m&fs.ModeNamedPipe != 0:
		return legacyRecord(ns, "fifo", name)
	case m&fs.ModeSocket != 0:
		return legacyRecord(ns, "socket", name)
	case m&fs.ModeDevice != 0:
		return legacyRecord(ns, "device", name)
	}
	return legacyRecord(ns, "other", name)
}

// hashFile opens without following a symlink or blocking on a fifo swapped in
// after the lstat, and hashes only a regular file.
func hashFile(path string) (string, bool) {
	if syscall.Access(path, rOK) != nil {
		return "", false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// cutField is bash's `${s%%$'\n'*}` / `${s#*$'\n'}` pair: with no newline the
// rest is s itself.
func cutField(s string) (string, string) {
	head, rest, ok := strings.Cut(s, "\n")
	if !ok {
		return head, s
	}
	return head, rest
}

// legacyFields splits a legacy record into namespace, kind, name and hash.
func legacyFields(rec string) (ns, kind, name, hash string) {
	ns, rest := cutField(strings.TrimPrefix(rec, legacyPrefix))
	kind, rest = cutField(rest)
	name, hash = cutField(rest)
	return ns, kind, name, hash
}

// LegacyEntry is one legacy entry a baseline does not cover.
type LegacyEntry struct {
	Kind string
	// Shown is the entry's path, already quoted for an operator.
	Shown string
	// Unsafe is set for any kind but `file`: it refuses whatever the baseline says.
	Unsafe bool
}

// LegacyDrift lists common's legacy entries that are unsafe, or files whose
// record base lacks.
func LegacyDrift(common string, base map[string]bool) []LegacyEntry {
	var drift []LegacyEntry
	for _, rec := range LegacyRecords(common) {
		ns, kind, name, _ := legacyFields(rec)
		shown := Quote(common + "/" + ns + "/" + name)
		// An unsafe-name record already stores its name quoted.
		if kind == "unsafe-name" {
			shown = Quote(common+"/"+ns+"/") + name
		}
		switch {
		case kind != "file":
			drift = append(drift, LegacyEntry{Kind: kind, Shown: shown, Unsafe: true})
		case !base[rec]:
			drift = append(drift, LegacyEntry{Kind: kind, Shown: shown})
		}
	}
	return drift
}
