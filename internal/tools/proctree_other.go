//go:build !unix

package tools

import (
	"os/exec"
)

// Non-unix platforms have no process groups; kill just the child.
func setProcessGroup(cmd *exec.Cmd) error { return nil }

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
