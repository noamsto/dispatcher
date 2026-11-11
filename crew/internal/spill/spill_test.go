package spill

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
		// os.Root follows a link that stays inside the crew dir.
		"in-root symlink": func(crewDir string) {
			mkdir(t, filepath.Join(crewDir, Dir))
			if err := os.WriteFile(filepath.Join(crewDir, "pid"), []byte("1234"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../pid", filepath.Join(crewDir, Dir, "secret")); err != nil {
				t.Fatal(err)
			}
		},
		// The in-root link makes any file of the crew dir look like a spill.
		"msg-bodies linked to the crew dir": func(crewDir string) {
			if err := os.WriteFile(filepath.Join(crewDir, "secret"), []byte("pid"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(".", filepath.Join(crewDir, Dir)); err != nil {
				t.Fatal(err)
			}
		},
		// os.Root cannot see a hard link, and the target can be anywhere on the filesystem.
		"hard link": func(crewDir string) {
			mkdir(t, filepath.Join(crewDir, Dir))
			if err := os.Link(filepath.Join(outside, "secret"), filepath.Join(crewDir, Dir, "secret")); err != nil {
				t.Fatal(err)
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

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
