//go:build windows

package cdp

import "os/exec"

// platformSupported is false on Windows because exec.Cmd.ExtraFiles cannot pass
// the pipe fds. Spawn refuses to launch until a Windows handle-inheritance
// transport is implemented; the package still compiles.
const platformSupported = false

// setSysProcAttr is a no-op on Windows; the process-group model does not apply.
func setSysProcAttr(*exec.Cmd) {}

// killGroup best-effort kills the single process on Windows.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
