// Package classes keeps the per-engine model classes (cheap, standard,
// premium): the class-state file that records each class's current default and
// its change history, and the resolver wrapper that asks model-resolve which
// models a tier admits. The state file is read by adapters/core/model-resolve.sh
// too, which looks only at .default, so the Go side adds fields and never
// renames that one.
package classes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
)

// StateCap is model-resolve's size cap on the state file. The file is untrusted
// input to both readers, so Load enforces the same bound instead of slurping
// whatever is on disk.
const StateCap = 65536

// HistoryKeep bounds Entry.History so the file stays far under StateCap.
const HistoryKeep = 10

// LockName is the mkdir-lock directory, under DataDir, that serialises
// read-modify-write cycles over the state file.
const LockName = "model-class-state.lock.d"

const warnPrefix = "class-state: "

// Change is one recorded switch of a class default.
type Change struct {
	From string `json:"from"`
	To   string `json:"to"`
	TS   int64  `json:"ts"`
	By   string `json:"by"` // "auto" | "promote"
	Why  string `json:"why,omitempty"`
}

// Entry is one engine/class: the default model plus how it got there. An empty
// Default follows the seed default. Vetoed holds ids the owner rejected, so
// auto-promotion does not pick them again.
type Entry struct {
	Default string   `json:"default,omitempty"`
	History []Change `json:"history,omitempty"`
	Vetoed  []string `json:"vetoed,omitempty"`
}

// State is the whole file: engine → class → entry.
type State map[string]map[string]Entry

// DataDir is the crew data directory, honouring XDG_DATA_HOME.
func DataDir(getenv func(string) string) string {
	base := getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(getenv("HOME"), ".local", "share")
	}
	return filepath.Join(base, "crew")
}

// StatePath is the class-state file.
func StatePath(getenv func(string) string) string {
	return filepath.Join(DataDir(getenv), "model-class-state.json")
}

// CatalogPath is the per-engine model catalog that refresh-models writes.
func CatalogPath(getenv func(string) string) string {
	return filepath.Join(DataDir(getenv), "model-catalog.json")
}

// Load reads the state file. The file is untrusted: a missing file is silently
// not usable, a file that is not a bounded regular JSON object is not usable
// with one warning, and a malformed engine, class, history or vetoed item is
// dropped alone with a warning so the rest survives. A writer must refuse on
// any warning, usable or not: saving the survivors would erase what was
// dropped. Unusable without a warning means absent: a writer starts from an
// empty state.
func Load(path string) (st State, usable bool, warnings []string) {
	warn := func(format string, args ...any) {
		warnings = append(warnings, warnPrefix+fmt.Sprintf(format, args...))
	}
	// Lstat refuses a FIFO before open could block on it, and a symlink too:
	// stricter than model-resolve's -f, which follows one.
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		warn("%v", err)
		return nil, false, warnings
	}
	if !fi.Mode().IsRegular() {
		warn("%s is not a regular file", path)
		return nil, false, warnings
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		warn("%v", err)
		return nil, false, warnings
	}
	defer func() { _ = f.Close() }()
	fi, err = f.Stat()
	if err != nil {
		warn("%v", err)
		return nil, false, warnings
	}
	if !fi.Mode().IsRegular() {
		warn("%s is not a regular file", path)
		return nil, false, warnings
	}
	data, err := io.ReadAll(io.LimitReader(f, StateCap+1))
	if err != nil {
		warn("%v", err)
		return nil, false, warnings
	}
	if len(data) > StateCap {
		warn("%s exceeds %d bytes", path, StateCap)
		return nil, false, warnings
	}
	var engines map[string]json.RawMessage
	if !isObject(data) {
		warn("%s is not a JSON object", path)
		return nil, false, warnings
	}
	if hasLoneSurrogate(data) {
		warn("%s has an unpaired surrogate escape", path)
		return nil, false, warnings
	}
	if err := json.Unmarshal(data, &engines); err != nil {
		warn("%s is not a JSON object: %v", path, err)
		return nil, false, warnings
	}
	st = State{}
	for engine, raw := range engines {
		var classes map[string]json.RawMessage
		if !isObject(raw) {
			warn("dropped engine %q: not an object", engine)
			continue
		}
		if err := json.Unmarshal(raw, &classes); err != nil {
			warn("dropped engine %q: not an object", engine)
			continue
		}
		for class, raw := range classes {
			entry, ok := parseEntry(raw, func(format string, args ...any) {
				warn("%s/%s: %s", cleanID(engine), cleanID(class), fmt.Sprintf(format, args...))
			})
			if !ok {
				continue
			}
			if st[engine] == nil {
				st[engine] = map[string]Entry{}
			}
			st[engine][class] = entry
		}
	}
	return st, true, warnings
}

// isObject reports whether raw is a JSON object. Unmarshal accepts null into a
// map, which model-resolve's jq treats as a non-object.
func isObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// hasLoneSurrogate reports whether raw has a high-surrogate \u escape that is not
// followed by a low-surrogate escape. json.Unmarshal turns such an escape into
// U+FFFD, but model-resolve's jq rejects the file, so Load must refuse it. A lone
// low surrogate is accepted by both, so only the high half is checked.
func hasLoneSurrogate(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			inString = c == '"'
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			unit, ok := unicodeEscape(raw, i)
			if !ok {
				// Skipping the escaped byte keeps \\ and \" from being read as escapes.
				i++
				continue
			}
			if unit < 0xD800 || unit > 0xDBFF {
				i += 5
				continue
			}
			low, ok := unicodeEscape(raw, i+6)
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 11
		}
	}
	return false
}

// unicodeEscape reads the \uXXXX escape starting at raw[i].
func unicodeEscape(raw []byte, i int) (uint64, bool) {
	if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
		return 0, false
	}
	unit, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
	return unit, err == nil
}

// parseChange decodes one history item with exact keys.
func parseChange(raw json.RawMessage) (Change, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Change{}, false
	}
	var c Change
	for key, dst := range map[string]any{"from": &c.From, "to": &c.To, "ts": &c.TS, "by": &c.By, "why": &c.Why} {
		if v, ok := fields[key]; ok {
			if err := json.Unmarshal(v, dst); err != nil {
				return Change{}, false
			}
		}
	}
	return c, c.From != "" && c.To != ""
}

// parseEntry decodes one class entry, dropping malformed history and vetoed
// items individually. It reports false when a default is present but not a
// string; an entry with no default follows the seed default.
func parseEntry(raw json.RawMessage, warn func(string, ...any)) (Entry, bool) {
	if !isObject(raw) {
		warn("dropped entry: not an object")
		return Entry{}, false
	}
	// A map decode matches keys exactly, as jq does; a struct decode would
	// match "DEFAULT" to default case-insensitively.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		warn("dropped entry: no string default")
		return Entry{}, false
	}
	var e Entry
	if def, ok := fields["default"]; ok {
		if err := json.Unmarshal(def, &e.Default); err != nil || string(def) == "null" {
			warn("dropped entry: no string default")
			return Entry{}, false
		}
	}
	var history, vetoed []json.RawMessage
	if err := json.Unmarshal(fields["history"], &history); fields["history"] != nil && err != nil {
		warn("dropped entry: history is not an array")
		return Entry{}, false
	}
	if err := json.Unmarshal(fields["vetoed"], &vetoed); fields["vetoed"] != nil && err != nil {
		warn("dropped entry: vetoed is not an array")
		return Entry{}, false
	}
	for _, item := range history {
		c, ok := parseChange(item)
		if !ok {
			warn("dropped history item: needs string from and to")
			continue
		}
		e.History = append(e.History, c)
	}
	for _, item := range vetoed {
		var id string
		if err := json.Unmarshal(item, &id); err != nil {
			warn("dropped vetoed item: not a string")
			continue
		}
		e.Vetoed = append(e.Vetoed, id)
	}
	return e, true
}

// Save writes st atomically: a temp file in the same directory renamed over the
// target, so model-resolve never reads a half-written file and a crash leaves
// the previous one intact.
func Save(path string, st State) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".model-class-state.*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	// CreateTemp makes 0600; the resolver and the owner's tools read the file.
	if err = tmp.Chmod(0o644); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Push records a change, keeping the last HistoryKeep. The model switched to is
// no longer vetoed: a deliberate promotion overrides an earlier rejection.
func (e *Entry) Push(c Change) {
	e.History = append(e.History, c)
	if n := len(e.History); n > HistoryKeep {
		e.History = slices.Clone(e.History[n-HistoryKeep:])
	}
	e.Vetoed = slices.DeleteFunc(e.Vetoed, func(id string) bool { return id == c.To })
}

// IsVetoed reports whether id was rejected for this class.
func (e Entry) IsVetoed(id string) bool {
	return slices.Contains(e.Vetoed, id)
}
