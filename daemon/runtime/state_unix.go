//go:build !windows

package runtime

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether a pid is a live process this user could signal.
// Signal 0 performs the permission and existence checks without delivering
// anything, which is exactly the question status is asking.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the pid exists but belongs to another user — still alive.
	return errors.Is(err, os.ErrPermission)
}
