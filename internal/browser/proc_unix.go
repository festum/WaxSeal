//go:build !windows

package browser

import (
	"os"
	"syscall"
)

// holdProfileLock takes an exclusive advisory lock on marker and returns the open
// file holding it; the caller must keep the file open for as long as the lock is
// needed and close it (via profileHandle.cleanup) to release it. The bool reports
// whether the lock was acquired.
func holdProfileLock(marker string) (*os.File, bool) {
	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, false
	}
	return f, true
}

// markerLockable reports whether marker's advisory lock is free. It releases the
// probe lock immediately. A marker that cannot be opened is treated as locked.
func markerLockable(marker string) bool {
	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false // A live owner holds the lock.
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true
}
