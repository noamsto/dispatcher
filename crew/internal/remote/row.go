package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// DispatchRow is `crew dispatch-row`: it prints the newest dispatch row on the
// bus at log with crew_id crew and session session, as compact JSON in its
// original key order. It exits 1 when there is none and 2 on bad arguments or
// an unreadable bus. It is how a dispatcher whose ssh link dropped mid-dispatch
// learns whether the remote created the worker.
//
// The bus is read a line at a time, skipping lines that are not JSON objects,
// so a torn line from a concurrent writer cannot hide a later row.
func DispatchRow(log, crew, session string, stdout, stderr io.Writer) int {
	if !bus.ValidCrewID(crew) {
		_, _ = fmt.Fprintf(stderr, "crew: dispatch-row: invalid crew id %q\n", crew)
		return exitUsage
	}
	if !SessionRe.MatchString(session) {
		_, _ = fmt.Fprintf(stderr, "crew: dispatch-row: invalid session %q (want s<epoch>-<pid>)\n", session)
		return exitUsage
	}
	if st, err := os.Stat(log); err != nil || !st.Mode().IsRegular() {
		return 1
	}
	f, err := os.Open(log)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "crew: dispatch-row: %v\n", err)
		return exitUsage
	}
	defer func() { _ = f.Close() }()

	var found []byte
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if row, ok := match(line, crew, session); ok {
			found = row
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "crew: dispatch-row: %s: %v\n", log, err)
			return exitUsage
		}
	}
	if found == nil {
		return 1
	}
	_, _ = stdout.Write(append(found, '\n'))
	return 0
}

// match compacts line when it is a dispatch row for crew and session.
func match(line []byte, crew, session string) ([]byte, bool) {
	var row struct {
		Kind    bus.Kind `json:"kind"`
		CrewID  string   `json:"crew_id"`
		Session string   `json:"session"`
	}
	if json.Unmarshal(line, &row) != nil || row.Kind != bus.KindDispatch || row.CrewID != crew || row.Session != session {
		return nil, false
	}
	var out bytes.Buffer
	if json.Compact(&out, line) != nil {
		return nil, false
	}
	return out.Bytes(), true
}
