//go:build !unix

package handover

import (
	"errors"
	"os"
)

func tryLock(f *os.File) (held bool, err error) {
	return false, errors.New("file locks are not supported on this platform")
}
