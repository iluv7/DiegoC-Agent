//go:build !windows

package backend

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the command in its own process group so a
// timeout can terminate the shell and every child it spawned. Killing only
// the direct child leaves grandchildren holding the stdout/stderr pipes,
// which makes Wait block until they exit on their own.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessGroup kills the whole process group. An already-exited
// group is not an error.
func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
