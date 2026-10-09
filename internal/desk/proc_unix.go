//go:build unix

package desk

import (
	"os/exec"
	"syscall"
)

// inGroup puts the turn's agent in its own process group, and makes cancelling the turn
// stop the whole group: the agent's MCP servers and tools too.
func inGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
