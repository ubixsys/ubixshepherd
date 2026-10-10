package desk

import "os/exec"

// inGroup leaves the default on Windows: cancelling the turn kills the agent.
func inGroup(cmd *exec.Cmd) {}
