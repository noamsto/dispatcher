package data

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// RawEvent is one complete bus line: its stable id, decoded row and the byte
// offset just past it.
type RawEvent struct {
	ID  string
	Ev  Event
	End int64
}

// EventPage is the result of one ReadEvents call.
type EventPage struct {
	Events []RawEvent
	// Cursor resumes after the last returned row; it is valid even when Events
	// is empty.
	Cursor string
	// Reset reports that the cursor named a different or shorter file (rotated
	// or truncated log), so reading restarted from the top.
	Reset bool
	// More reports rows past the limit remain unread.
	More bool
}

// EventID is a row's stable id: the first 16 hex digits of the SHA-256 of its
// raw line bytes (newline excluded), so every reader of the same log agrees.
func EventID(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:8])
}

// ErrBadCursor marks a cursor ReadEvents cannot parse.
var ErrBadCursor = errors.New("malformed cursor")

func parseCursor(c string) (ino uint64, off int64, err error) {
	a, b, ok := strings.Cut(c, ":")
	if !ok {
		return 0, 0, ErrBadCursor
	}
	ino, err1 := strconv.ParseUint(a, 10, 64)
	off, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || off < 0 {
		return 0, 0, ErrBadCursor
	}
	return ino, off, nil
}

// ReadEvents reads complete lines of the log at path after cursor. An empty
// cursor reads the last limit rows (the tail); a cursor reads forward from its
// offset, at most limit rows when limit > 0. A torn last line is left for the
// next call, so no row is missed or replayed. A cursor naming another inode,
// or an offset past the end of file, restarts from the top with Reset set.
// A missing log is an empty page.
func ReadEvents(path, cursor string, limit int) (EventPage, error) {
	var wantIno uint64
	var off int64
	if cursor != "" {
		var err error
		if wantIno, off, err = parseCursor(cursor); err != nil {
			return EventPage{}, fmt.Errorf("%w %q", err, cursor)
		}
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return EventPage{Cursor: "0:0"}, nil
	}
	if err != nil {
		return EventPage{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return EventPage{}, err
	}
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	page := EventPage{}
	switch {
	case cursor == "":
	case wantIno != ino || off > fi.Size():
		off, page.Reset = 0, true
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return EventPage{}, err
	}
	r := bufio.NewReaderSize(f, 64*1024)
	pos := off
	var all []RawEvent
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // torn last line: retried by the next call
		}
		if err != nil {
			return EventPage{}, err
		}
		pos += int64(len(line))
		raw := line[:len(line)-1]
		if ev, ok := decodeLine(raw); ok {
			all = append(all, RawEvent{ID: EventID(raw), Ev: ev, End: pos})
		} else {
			// A blank or torn-in-the-middle line still consumes its bytes.
			all = append(all, RawEvent{End: pos})
		}
	}
	if cursor == "" && limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	} else if limit > 0 && len(all) > limit {
		all, page.More = all[:limit], true
	}
	end := pos
	if page.More {
		end = all[len(all)-1].End
	}
	for _, e := range all {
		if e.Ev != nil {
			page.Events = append(page.Events, e)
		}
	}
	page.Cursor = fmt.Sprintf("%d:%d", ino, end)
	return page, nil
}
