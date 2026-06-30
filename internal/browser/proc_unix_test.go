//go:build !windows

package browser

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// markProfileDir returns the open lock file instead of stashing it in a process
// global. cleanup then removes the profile directory and closes the lock, letting
// another daemon's reaper reclaim the slot.
func TestProfileHandleCleanup(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), profilePrefix)
	if err != nil {
		t.Fatal(err)
	}
	lock := markProfileDir(dir)
	if lock == nil {
		t.Fatal("markProfileDir returned a nil lock; the FD must be handed back for cleanup")
	}
	marker := filepath.Join(dir, creatorMarkerFile)
	if markerLockable(marker) {
		t.Error("marker should be locked while the handle holds it")
	}

	profileHandle{dir: dir, lock: lock}.cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove the profile dir: %v", err)
	}
	// cleanup must be nil-safe for partially initialized handles.
	profileHandle{}.cleanup()
}

func TestMarkerLockable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), creatorMarkerFile)
	if err := os.WriteFile(marker, []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !markerLockable(marker) {
		t.Fatal("fresh marker should be lockable (no owner)")
	}

	f, err := os.OpenFile(marker, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	if markerLockable(marker) {
		t.Error("held marker should not be lockable (live owner)")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if !markerLockable(marker) {
		t.Error("released marker should be lockable again")
	}
}

func TestReapStaleProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mk := func(name string, marker bool) string {
		dir := filepath.Join(home, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if marker {
			if err := os.WriteFile(filepath.Join(dir, creatorMarkerFile), []byte("1"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	dead := mk(".waxseal-11111111", true)
	live := mk(".waxseal-22222222", true)
	markerless := mk(".waxseal-33333333", false)
	backup := mk(".waxseal-backup", false)
	sentinel := filepath.Join(backup, "important.txt")
	if err := os.WriteFile(sentinel, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hold the live profile's marker lock, simulating an in-use sibling.
	lf, err := os.OpenFile(filepath.Join(live, creatorMarkerFile), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	ReapStaleProfiles(nil)

	gone := func(p string) bool { _, err := os.Stat(p); return os.IsNotExist(err) }
	for _, c := range []struct {
		dir      string
		wantGone bool
		why      string
	}{
		{dead, true, "marked + lock free"},
		{live, false, "marked + lock held by a live owner"},
		{markerless, false, "unmarked profile"},
		{backup, false, "unrelated .waxseal-backup"},
	} {
		if gone(c.dir) != c.wantGone {
			t.Errorf("%s: gone=%v, want %v (%s)", filepath.Base(c.dir), gone(c.dir), c.wantGone, c.why)
		}
	}
	if gone(sentinel) {
		t.Error("user data inside .waxseal-backup was deleted")
	}
}
