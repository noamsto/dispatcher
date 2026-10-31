// Package classes keeps the per-engine model classes (cheap, standard,
// premium): the class-state file that records each class's current default and
// its change history, and the resolver wrapper that asks model-resolve which
// models a tier admits. The state file is read by adapters/core/model-resolve.sh
// too, which looks only at .default, so the Go side adds fields and never
// renames that one.
package classes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

// Entry is one engine/class: the default model plus how it got there. Vetoed
// holds ids the owner rejected, so auto-promotion does not pick them again.
type Entry struct {
	Default string   `json:"default"`
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
// dropped. Unusable without a warning means absent, the one case it may seed
// over.
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
	if err := json.Unmarshal(data, &engines); err != nil {
		warn("%s is not a JSON object: %v", path, err)
		return nil, false, warnings
	}
	st = State{}
	for engine, raw := range engines {
		var classes map[string]json.RawMessage
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

// parseEntry decodes one class entry, dropping malformed history and vetoed
// items individually. It reports false when the entry has no usable default.
func parseEntry(raw json.RawMessage, warn func(string, ...any)) (Entry, bool) {
	var fields struct {
		Default *string           `json:"default"`
		History []json.RawMessage `json:"history"`
		Vetoed  []json.RawMessage `json:"vetoed"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil || fields.Default == nil {
		warn("dropped entry: no string default")
		return Entry{}, false
	}
	e := Entry{Default: *fields.Default}
	for _, item := range fields.History {
		var c Change
		if err := json.Unmarshal(item, &c); err != nil || c.From == "" || c.To == "" {
			warn("dropped history item: needs string from and to")
			continue
		}
		e.History = append(e.History, c)
	}
	for _, item := range fields.Vetoed {
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

// SeedState builds the initial state from settings: a class's default is the
// modelMap default of the tiers mapped to it, recorded only when every such tier
// agrees on one non-empty model. A class whose tiers disagree has no single
// default to seed, so it is left out and model-resolve falls back to the tier
// rows.
func SeedState(s Settings) State {
	st := State{}
	for _, engine := range s.Engines() {
		classes := map[string]bool{}
		for _, tc := range s.Classes[engine].Tiers {
			classes[tc.Class] = true
		}
		for class := range classes {
			def, ok := seedDefault(s, engine, class)
			if !ok {
				continue
			}
			if st[engine] == nil {
				st[engine] = map[string]Entry{}
			}
			st[engine][class] = Entry{Default: def}
		}
	}
	return st
}

func seedDefault(s Settings, engine, class string) (string, bool) {
	var def string
	for _, tier := range s.TiersOf(engine, class) {
		d := s.ModelMap[engine][tier].Default
		if d == "" || (def != "" && d != def) {
			return "", false
		}
		def = d
	}
	return def, def != ""
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
