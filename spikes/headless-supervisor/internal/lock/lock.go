// Package lock holds a non-blocking flock(2) session lock, compatible with flock(1).
package lock

import (
	"errors"
	"os"
	"syscall"
)

var ErrHeld = errors.New("lock held by another process")

type Lock struct {
	f *os.File
}

func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// File is the locked file. A child given it as an extra fd shares the open
// file description, so the flock lives until every holder has closed it.
func (l *Lock) File() *os.File { return l.f }

// Release closes this process's handle. It never LOCK_UNs: that would drop the
// flock for a child still holding the shared description.
func (l *Lock) Release() error {
	return l.f.Close()
}
