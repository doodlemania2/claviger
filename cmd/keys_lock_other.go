//go:build !unix

package cmd

import "errors"

// tryLockFile is not supported here: the single-writer lock is taken with
// flock(1) by entrypoint.sh, which only exists on unix.
func tryLockFile(path string) (held bool, release func(), err error) {
	return false, nil, errors.New("file locks are not supported on this platform")
}
