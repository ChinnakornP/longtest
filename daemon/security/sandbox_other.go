//go:build !linux

package security

import (
	"errors"
	"os/exec"
	"syscall"
)

// Only Linux can enforce this package's process sandbox today. macOS has
// sandbox_init, but it is deprecated and undocumented; Windows has no
// equivalent primitive we would trust. A developer on either can still run the
// pipeline with Spec.AllowUnsandboxed, which the daemon reports in `hello` so
// the platform can refuse to schedule production runs on that runtime.
func sandboxSupported() bool { return false }

func applyProcAttr(cmd *exec.Cmd, _ Spec) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func applyStub(_ stubSpec) error {
	return ErrSandboxUnsupported
}

func execve(_ string, _, _ []string) error {
	return errors.New("security: exec stub is not supported on this platform")
}
