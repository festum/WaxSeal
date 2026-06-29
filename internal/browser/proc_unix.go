//go:build !windows

package browser

import (
	"os"
	"sync"
	"syscall"
)

// heldProfileLocks keeps marker file descriptors open so their locks remain held.
var heldProfileLocks struct {
	sync.Mutex
	files []*os.File
}

// holdProfileLock takes an exclusive advisory lock on marker and retains it for
// the process lifetime. It reports whether the lock was acquired.
func holdProfileLock(marker string) bool {
	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return false
	}
	heldProfileLocks.Lock()
	heldProfileLocks.files = append(heldProfileLocks.files, f)
	heldProfileLocks.Unlock()
	return true
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
