// Package filetest is the shell's `test -d` and `test -f`: both follow
// symlinks, and anything Stat cannot read is false.
package filetest

import "os"

// IsDir is `[ -d path ]`.
func IsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// IsRegular is `[ -f path ]`.
func IsRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
