//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

const (
	termSignal = syscall.SIGTERM
	killSignal = syscall.SIGKILL
)

// setGroup asks the kernel for a new process group whose id is the child's
// pid, so a negative-pid signal reaches every descendant that has not
// deliberately left the group.
func setGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func (c *Cmd) signalGroup(sig syscall.Signal) error {
	pid := c.PID()
	if pid <= 0 {
		return nil
	}
	// Read the group back rather than assuming it equals the pid: if setpgid
	// somehow did not take, signalling -pid would hit an unrelated group.
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		// The child is already gone; its group is nothing to signal.
		return nil //nolint:nilerr // ESRCH here means "already reaped", not a failure.
	}
	if pgid == pid {
		return syscall.Kill(-pgid, sig)
	}
	return syscall.Kill(pid, sig)
}
