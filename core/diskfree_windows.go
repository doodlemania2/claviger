//go:build windows

package core

import "errors"

// freeDiskSpace is not implemented on Windows (the check is skipped).
func freeDiskSpace(path string) (uint64, error) {
	return 0, errors.New("not supported")
}
