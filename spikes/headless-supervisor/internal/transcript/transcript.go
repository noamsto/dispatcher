// Package transcript writes the per-run JSONL log: one scrubbed frame per line.
package transcript

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type Writer struct {
	mu    sync.Mutex
	f     *os.File
	enc   *json.Encoder
	start time.Time
	s     *Scrubber
}

type line struct {
	T     int64           `json:"t"`
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
}

func New(path string, s *Scrubber) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return &Writer{f: f, enc: enc, start: time.Now(), s: s}, nil
}

// Log records a raw frame; dir is "in" (child to supervisor) or "out".
func (w *Writer) Log(dir string, frame json.RawMessage) error {
	scrubbed, err := w.s.Scrub(frame)
	if err != nil {
		// Leave a trace without the unscrubbed bytes so a lost frame is visible.
		marker, merr := json.Marshal(map[string]any{"scrub_error": err.Error(), "bytes": len(frame)})
		if merr != nil {
			return merr
		}
		if werr := w.write(dir, marker); werr != nil {
			return werr
		}
		return err
	}
	return w.write(dir, scrubbed)
}

// Note records a supervisor decision as dir "sup".
func (w *Writer) Note(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	scrubbed, err := w.s.Scrub(raw)
	if err != nil {
		return err
	}
	return w.write("sup", scrubbed)
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

func (w *Writer) write(dir string, frame json.RawMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(line{T: time.Since(w.start).Milliseconds(), Dir: dir, Frame: frame})
}
