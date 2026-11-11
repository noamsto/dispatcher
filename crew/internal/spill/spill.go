// Package spill keeps a msg body that does not fit one bus line whole. A bus
// line is capped at bus.LineMax so an append stays one atomic write; a longer
// body used to be cut mid-text. The writer instead stores the full body in a
// file under the crew's own directory and the row carries a short head plus
// a body_file field naming that file; the readers that hand a msg to an agent
// swap the full body back in.
//
// body_file is worker-writable data, and so is the crew tree it names a file
// in, so both sides go through an os.Root on the crew dir: a symlink that
// leaves the dir is refused (one that stays inside it is followed), and the
// file is opened O_NONBLOCK and checked for a regular file on the open
// descriptor. A hard link to a file elsewhere is not detected. The
// directory lives in the crew dir, which `crew deregister` removes as a unit,
// so the files need no collection of their own.
package spill

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/bus"
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
// symlink.
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
	return os.OpenRoot(crewDir)
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
	named, ok := row[Field]
	if !ok {
		return
	}
	rel, _ := named.(string)
	crewDir := ""
	if bus.ValidCrewID(crew) {
		crewDir = paths.CrewDir(crew)
	}
	full, err := read(crewDir, rel)
	if err != nil {
		row[NoteField] = fmt.Sprintf("full body unavailable (%v); body is the head only", err)
		return
	}
	row["body"] = full
	delete(row, Field)
}

// ExpandEvents expands every msg row of a `crew watch` batch ({cursor, events})
// the way Expand does one row. The batch is the dispatcher's whole view of its
// workers' messages, and a relay folds it on the host that owns the files.
func ExpandEvents(batch map[string]any, paths bus.Paths, crew string) {
	events, _ := batch["events"].([]any)
	for _, e := range events {
		if row, ok := e.(map[string]any); ok && row["kind"] == "msg" {
			Expand(row, paths, crew)
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
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("body_file %q is missing", rel)
	}
	if err != nil {
		return "", fmt.Errorf("body_file %q is unreadable", rel)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("body_file %q is not a regular file", rel)
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
