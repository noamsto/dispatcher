// Package spill keeps a msg body that does not fit one bus line whole. A bus
// line is capped at bus.LineMax so an append stays one atomic write; a longer
// body used to be cut mid-text. The writer instead stores the full body in a
// file under the crew's own directory and the row carries a short head plus
// a body_file field naming that file; the readers that hand a msg to an agent
// swap the full body back in.
//
// body_file is worker-writable data, and so is the crew tree it names a file
// in, so both sides go through an os.Root on the crew dir, which refuses a
// symlink that leaves the dir. The root follows one that stays inside it and
// cannot see a hard link, so the reader also Lstats the file, requires a
// regular file with a link count of 1, opens it O_NONBLOCK and checks that the
// open descriptor is the file it Lstatted. The directory lives in the crew
// dir, which `crew deregister` removes as a unit, so the files need no
// collection of their own.
package spill

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

const (
	// Field is the row key that names a spilled body.
	Field = "body_file"
	// NoteField carries the reason a named body could not be read back.
	NoteField = "body_note"
	// Dir is the msg-bodies directory under the crew dir.
	Dir = "msg-bodies"

	// MaxBody bounds a spilled body, written or read: a single argv word is at
	// most 128 KiB, so the writer never needs more, and a reader must not pour
	// a hand-placed multi-megabyte file into an agent's context.
	MaxBody = 256 << 10
)

// openCrewRoot opens crewDir as a root, refusing a crew dir that is itself a
// symlink or that was swapped for another directory between the check and the
// open.
func openCrewRoot(crewDir string) (*os.Root, error) {
	if crewDir == "" {
		return nil, errors.New("no crew directory")
	}
	st, err := os.Lstat(crewDir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", crewDir)
	}
	root, err := os.OpenRoot(crewDir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("%s changed while it was opened", crewDir)
	}
	return root, nil
}

// Write stores body under crewDir and returns the path relative to it.
func Write(crewDir string, ts int64, body string) (string, error) {
	if len(body) > MaxBody {
		return "", fmt.Errorf("body is %d bytes, over the %d-byte spill limit", len(body), MaxBody)
	}
	if err := os.MkdirAll(crewDir, 0o755); err != nil {
		return "", err
	}
	root, err := openCrewRoot(crewDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(Dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	var salt [6]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	rel := Dir + "/" + strconv.FormatInt(ts, 10) + "-" + hex.EncodeToString(salt[:]) + ".txt"
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return "", err
	}
	return rel, f.Close()
}

// Expand replaces row's body with the spilled full body and drops body_file. A
// body that cannot be read back keeps its head and gains a body_note naming why;
// it never fails. A row with no body_file is left alone.
func Expand(row map[string]any, paths bus.Paths, crew string) {
	expand(row, paths, crew, math.MaxInt)
}

// expand is Expand under a byte budget: a body whose encoded size exceeds left
// stays a head with a body_note. It returns the budget remaining.
func expand(row map[string]any, paths bus.Paths, crew string, left int) int {
	named, ok := row[Field]
	if !ok {
		return left
	}
	if left <= 0 {
		row[NoteField] = "full body unavailable (batch over the relay size budget); body is the head only"
		return left
	}
	rel, _ := named.(string)
	crewDir := ""
	if bus.ValidCrewID(crew) {
		crewDir = paths.CrewDir(crew)
	}
	full, err := read(crewDir, rel)
	if err != nil {
		row[NoteField] = fmt.Sprintf("full body unavailable (%v); body is the head only", err)
		return left
	}
	// Charged as the relay encodes it: the output encoder makes hidden code
	// points visible first (a 2-byte soft hyphen becomes `\u{00AD}`, 9 bytes once
	// JSON-escaped), so the stored size is no measure.
	var enc bytes.Buffer
	if err := jsonval.Encode(&enc, invisible.Value(full), false); err != nil || enc.Len() > left {
		row[NoteField] = "full body unavailable (batch over the relay size budget); body is the head only"
		return left
	}
	row["body"] = full
	delete(row, Field)
	return left - enc.Len()
}

// ExpandEvents expands every msg row of a `crew watch` batch ({cursor, events})
// the way Expand does one row. The batch is the dispatcher's whole view of its
// workers' messages, and a relay folds it on the host that owns the files.
func ExpandEvents(batch map[string]any, paths bus.Paths, crew string) {
	ExpandEventsWithin(batch, paths, crew, math.MaxInt)
}

// ExpandEventsWithin is ExpandEvents with a cap on the encoded bytes of all
// bodies it swaps in: once it is spent the remaining spilled msgs keep their
// head and gain a body_note. A relay line is one JSON object that its reader
// scans under a size limit, and each body can be MaxBody.
func ExpandEventsWithin(batch map[string]any, paths bus.Paths, crew string, budget int) {
	events, _ := batch["events"].([]any)
	for _, e := range events {
		if row, ok := e.(map[string]any); ok && row["kind"] == "msg" {
			budget = expand(row, paths, crew, budget)
		}
	}
}

// read loads rel from crewDir's msg-bodies directory.
func read(crewDir, rel string) (string, error) {
	if !filepath.IsLocal(rel) || filepath.Dir(rel) != Dir {
		return "", fmt.Errorf("body_file %q is not a file in %s/", rel, Dir)
	}
	root, err := openCrewRoot(crewDir)
	if err != nil {
		return "", errors.New("no readable crew directory")
	}
	defer func() { _ = root.Close() }()
	// The root follows a symlink that stays inside the crew dir, so
	// `msg-bodies -> .` would make any file there a body: read through a root
	// opened on the real directory instead.
	dlst, err := root.Lstat(Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("body_file %q is missing", rel)
	}
	if err != nil || !dlst.IsDir() {
		return "", fmt.Errorf("%s is not a directory", Dir)
	}
	dir, err := root.OpenRoot(Dir)
	if err != nil {
		return "", fmt.Errorf("%s is unreadable", Dir)
	}
	defer func() { _ = dir.Close() }()
	if opened, err := dir.Stat("."); err != nil || !os.SameFile(dlst, opened) {
		return "", fmt.Errorf("%s changed while it was opened", Dir)
	}
	name := filepath.Base(rel)
	lst, err := dir.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("body_file %q is missing", rel)
	}
	if err != nil {
		return "", fmt.Errorf("body_file %q is unreadable", rel)
	}
	if !lst.Mode().IsRegular() {
		return "", fmt.Errorf("body_file %q is not a regular file", rel)
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("body_file %q is unreadable", rel)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || !os.SameFile(lst, st) {
		return "", fmt.Errorf("body_file %q is not a regular file", rel)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || uint64(sys.Nlink) != 1 {
		return "", fmt.Errorf("body_file %q is hard-linked", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBody+1))
	if err != nil {
		return "", fmt.Errorf("body_file %q is unreadable", rel)
	}
	if len(data) > MaxBody {
		return "", fmt.Errorf("body_file %q is over %d bytes", rel, MaxBody)
	}
	return string(data), nil
}
