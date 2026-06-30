//go:build windows

package browser

import "os"

// holdProfileLock reports that advisory profile locks are unavailable on Windows.
func holdProfileLock(string) (*os.File, bool) { return nil, false }

// markerLockable returns false on Windows because the startup reaper's advisory
// lock protocol is not implemented there.
func markerLockable(string) bool { return false }
