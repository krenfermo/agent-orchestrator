//go:build !unix

package skillrunner

import "os/exec"

// setProcessGroup has no portable equivalent here; the CLI itself is still
// killed and the wait is still bounded.
func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
