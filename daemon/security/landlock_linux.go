//go:build linux

package security

import (
	"errors"
	"fmt"
	"math"
	"os"
	"syscall"
	"unsafe"
)

// Landlock is the kernel's unprivileged filesystem sandbox (Linux 5.13+). It
// is what actually enforces "the AI CLI cannot read or write outside the run
// workspace" for the child process — as opposed to [Workspace], which enforces
// it for code the daemon runs itself.
//
// Everything below is raw syscalls because Landlock has no wrapper in the
// standard library and the daemon module deliberately carries no dependencies.
//
// Reference: Documentation/userspace-api/landlock.rst.
//
// wrapper in the standard library and takes struct pointers by address; passing
// them is the only way to call it. Each buffer is a fixed size laid out
// immediately above its use.
//
//nolint:gosec // G103 flags every unsafe.Pointer in this file. Landlock has no
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1 << 0
	landlockRulePathBeneath      = 1

	// O_PATH has no syscall-package constant. The value is stable at
	// 0o10000000 across every Linux architecture the daemon targets
	// (amd64, arm64, riscv64); the exotic ones that differ (alpha, sparc)
	// are not supported platforms.
	oPath = 0o10000000
)

// Filesystem access rights, by the ABI version that introduced them.
const (
	accessFSExecute    uint64 = 1 << 0
	accessFSWriteFile  uint64 = 1 << 1
	accessFSReadFile   uint64 = 1 << 2
	accessFSReadDir    uint64 = 1 << 3
	accessFSRemoveDir  uint64 = 1 << 4
	accessFSRemoveFile uint64 = 1 << 5
	accessFSMakeChar   uint64 = 1 << 6
	accessFSMakeDir    uint64 = 1 << 7
	accessFSMakeReg    uint64 = 1 << 8
	accessFSMakeSock   uint64 = 1 << 9
	accessFSMakeFifo   uint64 = 1 << 10
	accessFSMakeBlock  uint64 = 1 << 11
	accessFSMakeSym    uint64 = 1 << 12
	accessFSRefer      uint64 = 1 << 13 // ABI 2
	accessFSTruncate   uint64 = 1 << 14 // ABI 3
	accessFSIoctlDev   uint64 = 1 << 15 // ABI 5
)

// accessForABI returns every right this kernel understands. Asking to handle
// a right the running ABI does not know is EINVAL, so the mask is built from
// the reported version rather than from a constant.
func accessForABI(abi int) uint64 {
	all := accessFSExecute | accessFSWriteFile | accessFSReadFile | accessFSReadDir |
		accessFSRemoveDir | accessFSRemoveFile | accessFSMakeChar | accessFSMakeDir |
		accessFSMakeReg | accessFSMakeSock | accessFSMakeFifo | accessFSMakeBlock |
		accessFSMakeSym
	if abi >= 2 {
		all |= accessFSRefer
	}
	if abi >= 3 {
		all |= accessFSTruncate
	}
	if abi >= 5 {
		all |= accessFSIoctlDev
	}
	return all
}

// readOnlyAccess is what a path on the read-only list grants: open for
// reading, list, and execute. Notably not accessFSTruncate — an agent that
// can truncate a shared library it can read has a way to break the host.
func readOnlyAccess(abi int) uint64 {
	a := accessFSExecute | accessFSReadFile | accessFSReadDir
	_ = abi
	return a
}

type rulesetAttr struct {
	handledAccessFS  uint64
	handledAccessNet uint64 // ABI 4+
	scoped           uint64 // ABI 6+
}

// landlockABI returns the ABI version the running kernel supports, or an
// error if Landlock is unavailable.
func landlockABI() (int, error) {
	r, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// ErrLandlockUnavailable means the kernel has no Landlock support. The daemon
// treats it as a hard failure for an AI CLI invocation unless the operator
// opted out explicitly; see docs/SECURITY.md.
var ErrLandlockUnavailable = errors.New("security: landlock is not available on this kernel")

// applyLandlock confines the calling process (and everything it execs) to
// workspace for read-write and readOnly for read-execute.
//
// The ruleset is a denylist inversion: every right named in handled_access_fs
// is denied everywhere except where a rule grants it. A path that appears in
// neither list is unreachable — which is the point, and why the read-only
// list has to be complete enough for the CLI to start.
func applyLandlock(workspace string, readOnly, readWrite []string) error {
	abi, err := landlockABI()
	if err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
			return ErrLandlockUnavailable
		}
		return fmt.Errorf("query landlock abi: %w", err)
	}

	handled := accessForABI(abi)
	attr := rulesetAttr{handledAccessFS: handled}
	// ABI 1-3 only know the first field; 4 adds net, 6 adds scoped. Passing a
	// larger struct than the kernel knows is E2BIG, so the size is the ABI's,
	// not unsafe.Sizeof(attr).
	var size uintptr
	switch {
	case abi <= 3:
		size = 8
	case abi <= 5:
		size = 16
	default:
		size = 24
	}

	fd, _, errno := syscall.Syscall(sysLandlockCreateRuleset,
		uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return fmt.Errorf("create ruleset: %w", errno)
	}
	rulesetFD := int(fd)
	defer syscall.Close(rulesetFD) //nolint:errcheck // best effort on a fd we are done with

	if err := addPathRule(rulesetFD, workspace, handled); err != nil {
		return fmt.Errorf("workspace rule: %w", err)
	}
	for _, p := range readWrite {
		if err := addPathRule(rulesetFD, p, handled); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read-write rule %q: %w", p, err)
		}
	}
	roAccess := readOnlyAccess(abi) & handled
	for _, p := range readOnly {
		if err := addPathRule(rulesetFD, p, roAccess); err != nil {
			// A path that vanished between DefaultReadOnlyPaths and here is
			// not worth failing a run over; a real error is.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read-only rule %q: %w", p, err)
		}
	}

	if _, _, errno := syscall.Syscall(sysLandlockRestrictSelf, uintptr(rulesetFD), 0, 0); errno != 0 {
		return fmt.Errorf("restrict self: %w", errno)
	}
	return nil
}

// fileOnlyAccess is the subset of rights that mean anything for a path that
// is not a directory. Handing the kernel a directory right (READ_DIR, MAKE_*,
// REMOVE_*, REFER) on a regular file is EINVAL, and several entries on the
// read-only list — /etc/resolv.conf, /dev/urandom — are files.
const fileOnlyAccess = accessFSExecute | accessFSWriteFile | accessFSReadFile |
	accessFSTruncate | accessFSIoctlDev

func addPathRule(rulesetFD int, path string, access uint64) error {
	if access == 0 {
		return nil
	}
	// G304: the path is variable by design — it is the allowlist entry being
	// granted. O_PATH opens no file description that can read or write; it
	// exists only to name the path to the kernel.
	f, err := os.OpenFile(path, oPath|syscall.O_CLOEXEC, 0) //nolint:gosec // see comment
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // O_PATH handle, nothing buffered

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		access &= fileOnlyAccess
		if access == 0 {
			return nil
		}
	}

	// struct landlock_path_beneath_attr is
	//
	//     struct { __u64 allowed_access; __s32 parent_fd; } __attribute__((packed));
	//
	// A Go struct would be padded to 16 bytes and the kernel would reject the
	// size, so the 12 packed bytes are laid out by hand.
	var buf [12]byte

	fd := f.Fd()
	if fd > math.MaxInt32 {
		// parent_fd is a __s32. A descriptor this high cannot occur under the
		// RLIMIT_NOFILE the sandbox sets, but truncating it would silently
		// grant the rule to whatever file the truncated number named.
		return fmt.Errorf("file descriptor %d does not fit parent_fd", fd)
	}
	*(*uint64)(unsafe.Pointer(&buf[0])) = access
	*(*int32)(unsafe.Pointer(&buf[8])) = int32(fd)

	if _, _, errno := syscall.Syscall6(sysLandlockAddRule,
		uintptr(rulesetFD), landlockRulePathBeneath,
		uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
