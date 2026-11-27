package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	w, err := New(path, &Scrubber{Paths: []PathRule{{Prefix: "/home/alice", Placeholder: "$HOME"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, path
}

type decodedLine struct {
	T     int64           `json:"t"`
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("transcript must end with LF, got %q", data)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestWriterLineShape(t *testing.T) {
	w, path := newWriter(t)
	if err := w.Log("in", json.RawMessage(`{"type":"user","cwd":"/home/alice/x","k":"<&>"}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Log("out", json.RawMessage(`{ "type" : "control_request" }`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Note(map[string]string{"verdict": "deny"}); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	wantFrames := []string{
		`{"cwd":"$HOME/x","k":"<&>","type":"user"}`,
		`{"type":"control_request"}`,
		`{"verdict":"deny"}`,
	}
	wantDirs := []string{"in", "out", "sup"}
	var prev int64
	for i, l := range lines {
		var d decodedLine
		if err := json.Unmarshal([]byte(l), &d); err != nil {
			t.Fatalf("line %d not JSON: %v", i, err)
		}
		if d.Dir != wantDirs[i] {
			t.Errorf("line %d dir = %q, want %q", i, d.Dir, wantDirs[i])
		}
		if string(d.Frame) != wantFrames[i] {
			t.Errorf("line %d frame = %s, want %s", i, d.Frame, wantFrames[i])
		}
		if d.T < prev {
			t.Errorf("line %d t went backwards: %d < %d", i, d.T, prev)
		}
		prev = d.T
	}
}

func TestWriterConcurrentLinesStayWhole(t *testing.T) {
	w, path := newWriter(t)
	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := w.Log("in", json.RawMessage(`{"type":"text","text":"hello"}`)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var d decodedLine
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("interleaved line: %v", err)
		}
		count++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("want %d lines, got %d", n, count)
	}
}

func TestLogInvalidFrameWritesScrubErrorMarker(t *testing.T) {
	w, path := newWriter(t)
	if err := w.Log("in", json.RawMessage(`{"a":`)); err == nil {
		t.Fatal("want error for invalid frame")
	}
	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("want 1 marker line, got %d", len(lines))
	}
	var d struct {
		Dir   string `json:"dir"`
		Frame struct {
			ScrubError string `json:"scrub_error"`
			Bytes      int    `json:"bytes"`
		} `json:"frame"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &d); err != nil {
		t.Fatal(err)
	}
	if d.Dir != "in" || d.Frame.ScrubError == "" || d.Frame.Bytes != len(`{"a":`) {
		t.Fatalf("bad marker: %s", lines[0])
	}
	if strings.Contains(lines[0], `"a"`) {
		t.Fatalf("marker leaks frame bytes: %s", lines[0])
	}
}
