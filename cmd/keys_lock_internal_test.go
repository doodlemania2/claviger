//go:build unix

package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Fork-local (Claviger #75, M3).

func TestOfflineLockIsHeldUntilReleased(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, singleWriterLockName)

	// no lock file (no entrypoint): nothing to hold, nothing created
	acquireOfflineLock(dir)()
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatal("the lock file must not be created")
	}
	if ServerLiveReason(dir) != "" {
		t.Fatal("an empty directory is not a live server")
	}

	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ServerLiveReason(dir) != "" {
		t.Fatal("an unheld lock file is not a live server")
	}

	release := acquireOfflineLock(dir)

	// while the command runs, a starting replica cannot take the lock, and
	// another keys command sees a live server
	f, _ := os.Open(lockPath)
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("the lock must be held while the command runs")
	}
	if ServerLiveReason(dir) == "" {
		t.Fatal("expected the held lock to read as a live server")
	}

	release()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
}
