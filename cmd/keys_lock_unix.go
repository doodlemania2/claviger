//go:build unix

package cmd

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive, non-blocking flock on path.
//
// held is true when somebody else holds it. On success release unlocks and
// closes the file. It never creates the file.
func tryLockFile(path string) (held bool, release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false, nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil, nil
		}
		return false, nil, err
	}

	return false, func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
