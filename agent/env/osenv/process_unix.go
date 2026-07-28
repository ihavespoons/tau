//go:build !windows

package osenv

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so the whole tree
// can be signalled at once. Without this, killing the shell orphans anything
// it spawned (Pi does the same via spawn's `detached: true`).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree SIGKILLs the child's entire process group, falling back to
// the single process if the group is already gone. Negating the pid addresses
// the group — this is what stops grandchildren from being orphaned.
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
