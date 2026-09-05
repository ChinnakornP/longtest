//go:build linux

package security

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func sandboxSupported() bool { return true }

// applyProcAttr sets the process attributes that must be requested at clone
// time rather than after the fact.
func applyProcAttr(cmd *exec.Cmd, s Spec) {
	attr := &syscall.SysProcAttr{
		// Its own process group, so a wall-clock kill takes the whole tree
		// and not just the stub. A CLI that spawned a language server must
		// not outlive the run that started it.
		Setpgid: true,
		// If the daemon dies, the kernel signals the child. Otherwise an
		// orphaned Chromium keeps a customer's laptop warm until reboot.
		Pdeathsig: syscall.SIGKILL,
	}
	if s.Network == NetworkNone {
		// A user namespace is what lets an unprivileged daemon create a
		// network namespace at all. The child sees an interface-less netns:
		// no loopback, no DNS, no route.
		attr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	cmd.SysProcAttr = attr
}

// applyStub restricts the current process. Order matters: no_new_privs must
// be set before Landlock will accept a ruleset, and every restriction must be
// in place before the exec.
func applyStub(spec stubSpec) error {
	if err := setNoNewPrivs(); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := applyRlimits(spec.Limits); err != nil {
		return fmt.Errorf("rlimits: %w", err)
	}
	if err := applyLandlock(spec.WorkspaceDir, spec.ReadOnlyPaths, spec.ReadWritePaths); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	return nil
}

// setNoNewPrivs makes it impossible for the child, or anything it execs, to
// gain privileges through a setuid binary. Without it a confined agent could
// still run `sudo` or a setuid helper and step outside every limit below.
func setNoNewPrivs() error {
	const prSetNoNewPrivs = 38
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

func applyRlimits(l Limits) error {
	set := func(res int, v uint64, name string) error {
		if v == 0 {
			return nil
		}
		var cur syscall.Rlimit
		if err := syscall.Getrlimit(res, &cur); err == nil {
			// Never raise a limit: if the operator already runs the daemon
			// under a tighter one, that is the effective policy.
			if cur.Max != ^uint64(0) && v > cur.Max {
				v = cur.Max
			}
		}
		lim := syscall.Rlimit{Cur: v, Max: v}
		if err := syscall.Setrlimit(res, &lim); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	// No core dumps, ever, and not as a resource limit: a core of the AI CLI
	// contains the model credentials it authenticated with and whatever page
	// content was in memory, and core_pattern routes it somewhere outside the
	// workspace that the run cannot scrub.
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return fmt.Errorf("RLIMIT_CORE: %w", err)
	}

	if err := set(syscall.RLIMIT_CPU, l.CPUSeconds, "RLIMIT_CPU"); err != nil {
		return err
	}
	if err := set(syscall.RLIMIT_AS, l.AddressSpaceBytes, "RLIMIT_AS"); err != nil {
		return err
	}
	if err := set(syscall.RLIMIT_FSIZE, l.MaxFileBytes, "RLIMIT_FSIZE"); err != nil {
		return err
	}
	if err := set(syscall.RLIMIT_NOFILE, l.MaxOpenFiles, "RLIMIT_NOFILE"); err != nil {
		return err
	}
	// RLIMIT_NPROC is not a per-process limit: the kernel counts every
	// process owned by this uid, the daemon's own and every unrelated program
	// the operator happens to be running. Setting it to an absolute MaxProcs
	// makes fork() fail immediately on any machine that is already past that
	// count — a workstation trivially is — so the ceiling is expressed as
	// headroom above current usage instead.
	//
	// That makes it a bound on how many processes this run may *add*, which is
	// the fork-bomb property we wanted, at the cost of being approximate: a
	// second run starting concurrently raises the baseline. A precise limit
	// needs a cgroup with pids.max, which needs cgroup delegation the daemon
	// cannot assume on a customer laptop. See docs/SECURITY.md.
	if l.MaxProcs > 0 {
		const rlimitNproc = 6
		if err := set(rlimitNproc, countUserTasks()+l.MaxProcs, "RLIMIT_NPROC"); err != nil &&
			!errors.Is(err, syscall.EPERM) {
			return err
		}
	}
	return nil
}

// countUserTasks returns how many tasks this uid currently owns.
//
// Tasks, not processes: the kernel charges RLIMIT_NPROC per clone, so every
// thread counts. A desktop with 400 processes routinely runs several thousand
// threads, and a baseline that counted only thread-group leaders would set the
// limit an order of magnitude below actual usage — every fork the run makes
// would then fail with EAGAIN, which looks exactly like a mysterious hang.
//
// A read failure returns a deliberately high baseline: over-counting weakens
// the limit, under-counting breaks the run.
func countUserTasks() uint64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 65536
	}
	self := os.Getuid()
	if self < 0 {
		// Only reachable on a platform without uids, which this file is not
		// built for. Fail towards a limit that does not break forking.
		return 65536
	}
	uid := uint64(self)
	var n uint64
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name[0] < '0' || name[0] > '9' {
			continue
		}
		info, err := os.Stat("/proc/" + name)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint64(st.Uid) != uid {
			continue
		}
		tasks, err := os.ReadDir("/proc/" + name + "/task")
		if err != nil {
			n++ // process exited under us; count it as one
			continue
		}
		n += uint64(len(tasks))
	}
	if n == 0 {
		return 65536
	}
	return n
}

// execve replaces the stub with the target program.
//
// G204 flags this as a subprocess launched from a variable, which is exactly
// what a sandbox stub is: it exists to become the program the caller named,
// after applying restrictions that survive the exec. The argv is built by
// Spec.Command in this repository, never taken from a page or a model.
func execve(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env) //nolint:gosec // see comment
}
