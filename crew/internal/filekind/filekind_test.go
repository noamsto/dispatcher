package filekind

import (
	"errors"
	"os"
	"testing"
)

func TestIsDirAndIsRegular(t *testing.T) {
	d := t.TempDir()
	if err := errors.Join(os.Mkdir(d+"/dir", 0o755), os.WriteFile(d+"/file", nil, 0o644),
		os.Symlink(d+"/dir", d+"/dirlink"), os.Symlink(d+"/file", d+"/filelink")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][2]bool{"dir": {true, false}, "dirlink": {true, false},
		"file": {false, true}, "filelink": {false, true}, "missing": {}} {
		if got := [2]bool{IsDir(d + "/" + name), IsRegular(d + "/" + name)}; got != want {
			t.Errorf("%s: IsDir, IsRegular = %v, want %v", name, got, want)
		}
	}
}
