//go:build windows

package osenv

import (
	"os/exec"
	"strconv"
)

// setProcessGroup is a no-op on Windows; the tree is torn down with taskkill
// instead of a process-group signal.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree terminates the process and its descendants via taskkill,
// mirroring Pi's Windows branch in killProcessTree (utils/shell.ts).
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
