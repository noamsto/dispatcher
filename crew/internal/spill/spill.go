// Package spill keeps a msg body that does not fit one bus line whole. A bus
// line is capped at bus.LineMax so an append stays one atomic write; a longer
// body used to be cut mid-text. The writer instead stores the full body in a
// file under the crew's own directory and the row carries a short head plus
// a body_file field naming that file; the readers that hand a msg to an agent
// swap the full body back in.
//
// body_file is worker-writable data, so a reader resolves it only inside the
// crew's msg-bodies directory and never follows a symlink. The directory
// lives in the crew dir, which `crew deregister` removes as a unit, so the
// files need no collection of their own.
package spill

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

const (
	// Field is the row key that names a spilled body.
	Field = "body_file"
	// NoteField carries the reason a named body could not be read back.
	NoteField = "body_note"
	// Dir is the msg-bodies directory under the crew dir.
	Dir = "msg-bodies"

	// maxBody bounds what a reader loads: the file is worker-writable.
	maxBody = 8 << 20
)

// Write stores body under crewDir and returns the path relative to it.
func Write(crewDir string, ts int64, body string) (string, error) {
	dir := filepath.Join(crewDir, Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var salt [6]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	rel := Dir + "/" + strconv.FormatInt(ts, 10) + "-" + hex.EncodeToString(salt[:]) + ".txt"
	f, err := os.OpenFile(filepath.Join(crewDir, rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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

// read loads rel from crewDir's msg-bodies directory.
func read(crewDir, rel string) (string, error) {
	if crewDir == "" {
		return "", fmt.Errorf("no crew directory")
	}
	if !filepath.IsLocal(rel) || filepath.Dir(rel) != Dir {
		return "", fmt.Errorf("body_file %q is not a file in %s/", rel, Dir)
	}
	path := filepath.Join(crewDir, rel)
	st, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("body_file %q is missing", rel)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("body_file %q is not a regular file", rel)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("body_file %q is unreadable", rel)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("body_file %q is unreadable", rel)
	}
	if len(data) > maxBody {
		return "", fmt.Errorf("body_file %q is over %d bytes", rel, maxBody)
	}
	return string(data), nil
}
