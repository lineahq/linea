package worker

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Apply makes cmd start as u with no supplementary groups. The process
// needs CAP_SETUID and CAP_SETGID; the child keeps no capability.
func (u ChildUser) Apply(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: []uint32{}}
	return nil
}

// prSetDumpable is PR_SET_DUMPABLE of prctl(2).
const prSetDumpable = 4

// protectProcess makes this process non-dumpable: its /proc files belong
// to root and other processes of its uid cannot ptrace it.
func protectProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE): %w", errno)
	}
	return nil
}

// runningAsRoot reports whether this process's uid is 0.
func runningAsRoot() bool { return os.Getuid() == 0 }

// exposedTo reports why u can read the file at path, or "".
func exposedTo(path string, u ChildUser) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", nil
	}
	mode := fi.Mode().Perm()
	switch {
	case st.Uid == u.UID && mode&0o400 != 0:
		return fmt.Sprintf("it belongs to uid %d", u.UID), nil
	case st.Gid == u.GID && mode&0o040 != 0:
		return fmt.Sprintf("its group %d may read it", u.GID), nil
	case mode&0o004 != 0:
		return fmt.Sprintf("its mode %04o lets every user read it", mode), nil
	}
	return "", nil
}
