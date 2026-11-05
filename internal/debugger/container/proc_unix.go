//go:build unix

package container

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in its own process group, so that a cancelled build
// takes the compilers the go command started with it.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
