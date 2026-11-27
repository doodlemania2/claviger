//go:build unix

package handover

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive, non-blocking flock on f. held is true when
// somebody else owns it. Never blocks: a blocking flock on NFS re-polls with
// exponential backoff and misses releases (#54).
func tryLock(f *os.File) (held bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, err
}
