package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultSegBytes = 1 << 20
	defaultKeepSegs = 4

	daemonInfoName  = "daemon.json"
	followStateName = "follow.json"
	logDirName      = "out"
)

type segment struct {
	start int64
	size  int64
}

func segName(start int64) string { return fmt.Sprintf("%020d.jsonl", start) }

func listSegments(dir string) ([]segment, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var segs []segment
	for _, e := range ents {
		n, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok {
			continue
		}
		start, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		segs = append(segs, segment{start, info.Size()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].start < segs[j].start })
	return segs, nil
}

// segLog appends whole lines to size-rotated segment files whose names carry
// the logical byte offset of their first byte.
type segLog struct {
	dir      string
	segBytes int64
	keepSegs int
	f        *os.File
	start    int64
	size     int64
}

func openSegLog(dir string, segBytes int64, keepSegs int) (*segLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	segs, err := listSegments(dir)
	if err != nil {
		return nil, err
	}
	l := &segLog{dir: dir, segBytes: segBytes, keepSegs: keepSegs}
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		l.start, l.size = last.start, last.size
	}
	if l.f, err = os.OpenFile(filepath.Join(dir, segName(l.start)), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *segLog) Write(p []byte) (int, error) {
	n, err := l.f.Write(p)
	l.size += int64(n)
	if err != nil || l.size < l.segBytes {
		return n, err
	}
	return n, l.rotate()
}

func (l *segLog) rotate() error {
	next := l.start + l.size
	f, err := os.OpenFile(filepath.Join(l.dir, segName(next)), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	l.f.Close()
	l.f, l.start, l.size = f, next, 0
	segs, err := listSegments(l.dir)
	if err != nil {
		return err
	}
	for i := 0; i < len(segs)-l.keepSegs; i++ {
		if err := os.Remove(filepath.Join(l.dir, segName(segs[i].start))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (l *segLog) end() int64 { return l.start + l.size }

func (l *segLog) Close() error { return l.f.Close() }

// readLog returns the complete lines at or after off. A segment pruned
// between listing and open triggers one re-list.
func readLog(dir string, off int64) (chunk []byte, next, oldest, end int64, err error) {
	for attempt := 0; ; attempt++ {
		chunk, next, oldest, end, err = readLogOnce(dir, off)
		if errors.Is(err, fs.ErrNotExist) && attempt == 0 {
			continue
		}
		return
	}
}

func readLogOnce(dir string, off int64) (chunk []byte, next, oldest, end int64, err error) {
	segs, err := listSegments(dir)
	if err != nil || len(segs) == 0 {
		return nil, off, 0, 0, err
	}
	oldest = segs[0].start
	last := segs[len(segs)-1]
	end = last.start + last.size
	if off > end {
		return nil, off, oldest, end, nil
	}
	from := max(off, oldest)
	var buf bytes.Buffer
	for _, s := range segs {
		if s.start+s.size <= from {
			continue
		}
		f, err := os.Open(filepath.Join(dir, segName(s.start)))
		if err != nil {
			return nil, off, oldest, end, err
		}
		_, err = f.Seek(max(from-s.start, 0), io.SeekStart)
		if err == nil {
			_, err = io.Copy(&buf, io.LimitReader(f, s.size-max(from-s.start, 0)))
		}
		f.Close()
		if err != nil {
			return nil, off, oldest, end, err
		}
	}
	chunk = buf.Bytes()
	if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
		chunk = chunk[:i+1]
	} else {
		chunk = nil
	}
	return chunk, from + int64(len(chunk)), oldest, end, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

type daemonInfo struct {
	PID         int64 `json:"pid"`
	StartedMS   int64 `json:"started_ms"`
	StartOffset int64 `json:"start_offset"`
}

type followState struct {
	Offset    int64 `json:"offset"`
	StartedMS int64 `json:"started_ms"`
}

func readJSONFile[T any](path string) (v T, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &v) != nil {
		var zero T
		return zero, false
	}
	return v, true
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func readDaemonInfo(path string) (daemonInfo, bool)   { return readJSONFile[daemonInfo](path) }
func writeDaemonInfo(path string, d daemonInfo) error { return writeJSONFile(path, d) }

func readFollowState(path string) (followState, bool)   { return readJSONFile[followState](path) }
func writeFollowState(path string, s followState) error { return writeJSONFile(path, s) }
