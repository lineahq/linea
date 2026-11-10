//go:build !linux

package worker

import (
	"errors"
	"os/exec"
)

var errChildUser = errors.New("starting children as another user is supported on Linux only")

// Apply fails: only Linux starts children as another user.
func (u ChildUser) Apply(*exec.Cmd) error { return errChildUser }

func protectProcess() error { return errChildUser }

func runningAsRoot() bool { return false }

func exposedTo(string, ChildUser) (string, error) { return "", nil }
