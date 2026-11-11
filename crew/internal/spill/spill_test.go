package spill

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

func TestWriteThenReadRoundTrips(t *testing.T) {
	crewDir := filepath.Join(t.TempDir(), "crews", "c1")
	body := strings.Repeat("é\n", 5000)
	rel, err := Write(crewDir, 42, body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := read(crewDir, rel)
	if err != nil || got != body {
		t.Fatalf("read = %d bytes, %v; want the %d-byte body", len(got), err, len(body))
	}
}

func TestWriteRefusesABodyOverTheLimit(t *testing.T) {
	if _, err := Write(t.TempDir(), 1, strings.Repeat("x", MaxBody+1)); err == nil {
		t.Fatal("an over-limit body was written")
	}
}

// The crew tree is worker-writable, so a name that passes the path check can
// still lead out of the crew dir through a link, or hang the reader.
func TestReadRefusesHostileFiles(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(crewDir string){
		"symlinked directory": func(crewDir string) {
			if err := os.Symlink(outside, filepath.Join(crewDir, Dir)); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked file": func(crewDir string) {
			mkdir(t, filepath.Join(crewDir, Dir))
			if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(crewDir, Dir, "secret")); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to another file in the crew dir": func(crewDir string) {
			mkdir(t, filepath.Join(crewDir, Dir))
			if err := os.WriteFile(filepath.Join(crewDir, "pid"), []byte("1234"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../pid", filepath.Join(crewDir, Dir, "secret")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked directory inside the crew dir": func(crewDir string) {
			if err := os.WriteFile(filepath.Join(crewDir, "secret"), []byte("1234"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(".", filepath.Join(crewDir, Dir)); err != nil {
				t.Fatal(err)
			}
		},
		"hard link to a file elsewhere": func(crewDir string) {
			mkdir(t, filepath.Join(crewDir, Dir))
			if err := os.Link(filepath.Join(outside, "secret"), filepath.Join(crewDir, Dir, "secret")); err != nil {
				t.Skipf("cannot hard link across these temp dirs: %v", err)
			}
		},
		"fifo": func(crewDir string) {
			mkdir(t, filepath.Join(crewDir, Dir))
			if err := syscall.Mkfifo(filepath.Join(crewDir, Dir, "secret"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"over the limit": func(crewDir string) {
			mkdir(t, filepath.Join(crewDir, Dir))
			big := strings.Repeat("x", MaxBody+1)
			if err := os.WriteFile(filepath.Join(crewDir, Dir, "secret"), []byte(big), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			crewDir := filepath.Join(t.TempDir(), "c1")
			mkdir(t, crewDir)
			setup(crewDir)
			if got, err := read(crewDir, Dir+"/secret"); err == nil {
				t.Fatalf("read succeeded with %.20q", got)
			}
		})
	}
}

func TestReadRefusesASymlinkedCrewDir(t *testing.T) {
	real := t.TempDir()
	mkdir(t, filepath.Join(real, Dir))
	if err := os.WriteFile(filepath.Join(real, Dir, "x"), []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "c1")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := read(link, Dir+"/x"); err == nil {
		t.Fatal("read through a symlinked crew dir succeeded")
	}
}

// A batch is one relay line, so rows past the budget keep their head and file
// and say how to read the rest; rows within it are expanded.
func TestExpandEventsStopsAtTheBatchBudget(t *testing.T) {
	root := t.TempDir()
	paths := bus.Paths{Dir: root}
	crewDir := paths.CrewDir("c1")
	body := strings.Repeat("x", MaxBody)
	const rows = 20 // 5 MiB of bodies against the 4 MiB budget
	events := make([]any, rows)
	for i := range events {
		rel, err := Write(crewDir, int64(i), body)
		if err != nil {
			t.Fatal(err)
		}
		events[i] = map[string]any{"kind": "msg", "body": "head", Field: rel}
	}
	batch := map[string]any{"events": events}
	ExpandEvents(batch, paths, "c1")

	expanded := 0
	for i, e := range events {
		row := e.(map[string]any)
		if row["body"] == body {
			expanded++
			if _, kept := row[Field]; kept || row[NoteField] != nil {
				t.Errorf("row %d expanded but kept %v", i, row)
			}
			continue
		}
		if row["body"] != "head" || row[Field] == nil || row[NoteField] == nil {
			t.Errorf("row %d = %v, want the head, body_file and a note", i, row)
		}
	}
	if want := BatchBudget / encodedLen(body); expanded != want {
		t.Errorf("%d rows expanded, want %d", expanded, want)
	}
}

// The budget counts what the read paths write: a hidden code point is rewritten
// as text that JSON escapes again, several times its stored size.
func TestExpandEventsBudgetCountsHiddenCodePointEscaping(t *testing.T) {
	paths := bus.Paths{Dir: t.TempDir()}
	crewDir := paths.CrewDir("c1")
	body := strings.Repeat("­", MaxBody/2) // soft hyphen: 2 bytes stored
	events := make([]any, 40)
	for i := range events {
		rel, err := Write(crewDir, int64(i), body)
		if err != nil {
			t.Fatal(err)
		}
		events[i] = map[string]any{"kind": "msg", "body": "head", Field: rel}
	}
	batch := map[string]any{"events": events}
	ExpandEvents(batch, paths, "c1")

	var line bytes.Buffer
	if err := jsonval.Encode(&line, invisible.Value(batch), false); err != nil {
		t.Fatal(err)
	}
	if limit := BatchBudget + 40*512; line.Len() > limit {
		t.Errorf("batch line is %d bytes, want at most %d", line.Len(), limit)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
