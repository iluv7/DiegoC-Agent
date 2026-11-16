//go:build windows

package backend

import "os/exec"

// configureProcessGroup is a no-op on Windows; there is no POSIX process
// group to join.
func configureProcessGroup(*exec.Cmd) {}

// terminateProcessGroup kills the direct child. Windows process-tree
// termination needs a job object, which this backend does not use.
func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
