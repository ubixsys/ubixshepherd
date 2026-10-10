//go:build windows

package chat

import "os/exec"

// setProcessGroup does nothing on Windows: the command is killed with its context.
func setProcessGroup(*exec.Cmd) {}
