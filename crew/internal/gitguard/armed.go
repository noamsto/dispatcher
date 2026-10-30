package gitguard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ArmedPath is the marker recording that common once had a baseline. It lives
// outside common, so a worker that can write the repo cannot clear it.
func ArmedPath(common string) (string, error) {
	_, path, err := armedKey(common)
	return path, err
}

func armedKey(common string) (real, path string, err error) {
	if real, err = filepath.EvalSymlinks(common); err != nil {
		return "", "", err
	}
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".local/share")
	}
	sum := sha256.Sum256([]byte(real))
	return real, filepath.Join(dir, "crew", "git-baseline-armed", hex.EncodeToString(sum[:])), nil
}

// arm records that common has a baseline; it is idempotent.
func arm(common string) error {
	real, path, err := armedKey(common)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(real+"\n"), 0o666)
}

// isArmed fails closed: a marker that cannot be statted counts as armed.
func isArmed(common string) (bool, error) {
	path, err := ArmedPath(common)
	if err != nil {
		return true, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return true, err
	}
	return true, nil
}
