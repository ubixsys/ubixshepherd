//go:build !windows

package chat

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own group so Ctrl-C stops what it started too.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
