//go:build windows

package proc

import (
	"os/exec"
	"strconv"
	"syscall"
)

// Windows has no process groups in the POSIX sense and no SIGTERM. A new
// console process group makes the child ignorable by the daemon's own Ctrl+C,
// and the tree is torn down with taskkill /T, which is what every Go program
// that needs this ends up doing.
const (
	termSignal = syscall.Signal(0)
	killSignal = syscall.Signal(1)
)

func setGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func (c *Cmd) signalGroup(sig syscall.Signal) error {
	pid := c.PID()
	if pid <= 0 {
		return nil
	}
	if sig == termSignal {
		// No polite signal exists that a non-console child would honour;
		// the grace window is spent waiting for stdin close to land.
		return nil
	}
	//nolint:gosec // G204: the only interpolated value is our own child's pid.
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	return kill.Run()
}
