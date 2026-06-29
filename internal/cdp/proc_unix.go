//go:build unix

package cdp

import (
	"os/exec"
	"syscall"
)

// platformSupported reports that the pipe transport can run on this OS.
const platformSupported = true

// setSysProcAttr puts Chromium in its own process group so teardown can signal
// that group. Helpers that leave the group are expected to exit when Chromium
// closes its own IPC. Parent death is handled by the command pipe closing: the OS
// closes fd 3, and Chromium exits after reading EOF.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup SIGKILLs the child's process group. With Setpgid and no explicit Pgid,
// the child is its own group leader, so the group id equals the child pid.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// A non-positive pid would make syscall.Kill(-pid) target the caller's own
	// process group. A started process always has pid > 0; guard defensively.
	if cmd.Process.Pid <= 0 {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
