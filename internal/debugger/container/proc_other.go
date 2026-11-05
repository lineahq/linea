//go:build !unix

package container

import "os/exec"

// ownGroup kills the process on cancellation; without process groups the
// compilers the go command started end with their parent's pipes.
func ownGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
