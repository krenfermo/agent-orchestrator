//go:build unix

package skillrunner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the CLI as the leader of its own process group, so a
// kill reaches anything it spawned and nothing AO did not start.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to the CLI's process group. The group id is
// the CLI's own pid (Setpgid), so this can only reach processes the CLI
// started. Errors are ignored: the group may already be gone.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
