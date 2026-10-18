package filetest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsDirAndIsRegular(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	file := filepath.Join(root, "file")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(root, "dirlink")
	fileLink := filepath.Join(root, "filelink")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, fileLink); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name         string
		path         string
		isDir, isReg bool
	}{
		{"dir", dir, true, false},
		{"file", file, false, true},
		{"symlink to dir", dirLink, true, false},
		{"symlink to file", fileLink, false, true},
		{"missing", filepath.Join(root, "missing"), false, false},
	} {
		if got := IsDir(tc.path); got != tc.isDir {
			t.Errorf("%s: IsDir = %v, want %v", tc.name, got, tc.isDir)
		}
		if got := IsRegular(tc.path); got != tc.isReg {
			t.Errorf("%s: IsRegular = %v, want %v", tc.name, got, tc.isReg)
		}
	}
}
