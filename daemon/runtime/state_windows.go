//go:build windows

package runtime

import "os"

// processAlive on Windows: OpenProcess succeeds only for a live process, and
// os.FindProcess wraps it.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer func() { _ = process.Release() }()
	return true
}
