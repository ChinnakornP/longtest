package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/security"
)

// TestMain lets the test binary stand in for the daemon binary. Spec.Command
// re-execs the daemon with a marker argv and applies the sandbox in the child;
// pointing SelfExe at this binary and dispatching here exercises that exact
// path rather than a simulation of it.
func TestMain(m *testing.M) {
	if security.IsSandboxStub() {
		security.RunSandboxStub()
	}
	os.Exit(m.Run())
}

func sandboxFor(t *testing.T, dir string) security.Spec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return security.Spec{
		WorkspaceDir: dir,
		Limits:       security.DefaultAgentLimits(),
		Network:      security.NetworkHost,
		EnvAllow:     security.BaseEnvAllow(),
		SelfExe:      self,
		// A developer on macOS still gets the timeout and process-group
		// behaviour these tests are about; only the confinement is absent.
		AllowUnsandboxed: true,
	}
}

func requireLaunchable(t *testing.T, dir string) {
	t.Helper()
	result, err := Run(t.Context(), Launch{
		Binary: "/bin/sh", Args: []string{"-c", "exit 0"},
		Sandbox: sandboxFor(t, dir), Timeout: 30 * time.Second,
	})
	if err != nil || result.ExitCode != 0 {
		t.Skipf("this host cannot launch a sandboxed child: %v (exit %d)", err, result.ExitCode)
	}
}

func TestLaunchReportsTheExitCode(t *testing.T) {
	dir := t.TempDir()
	requireLaunchable(t, dir)

	var stderr strings.Builder
	result, err := Run(t.Context(), Launch{
		Binary: "/bin/sh", Args: []string{"-c", "exit 3"},
		Stderr: &stderr, Sandbox: sandboxFor(t, dir), Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("exit code = %d, stderr = %q", result.ExitCode, stderr.String())
	}
	if result.TimedOut {
		t.Fatal("a child that exited on its own was reported as timed out")
	}
}

func TestLaunchWritesStdinAndCollectsOutput(t *testing.T) {
	dir := t.TempDir()
	requireLaunchable(t, dir)

	var stdout, stderr strings.Builder
	_, err := Run(t.Context(), Launch{
		Binary: "/bin/sh", Args: []string{"-c", "cat; echo problem >&2"},
		Stdin: []byte("the prompt\n"), Stdout: &stdout, Stderr: &stderr,
		Sandbox: sandboxFor(t, dir), Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != "the prompt" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.TrimSpace(stderr.String()) != "problem" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// A CLI that hangs must be killed with everything it started. An AI CLI is a
// wrapper around a long-lived agent that spawns its own children, and
// signalling only the process we forked leaves them holding a connection.
func TestLaunchKillsTheWholeTreeAtTheDeadline(t *testing.T) {
	dir := t.TempDir()
	requireLaunchable(t, dir)
	marker := filepath.Join(dir, "grandchild-survived")

	started := time.Now()
	result, err := Run(t.Context(), Launch{
		Binary: "/bin/sh",
		// A background grandchild that would leave a marker if it outlived
		// the kill, and a parent that never returns on its own.
		Args:    []string{"-c", "(sleep 3; touch " + marker + ") & sleep 60"},
		Sandbox: sandboxFor(t, dir),
		Timeout: 300 * time.Millisecond,
		Grace:   200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.TimedOut {
		t.Fatal("a child that outran its deadline was not reported as timed out")
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("the kill took %s", elapsed)
	}

	// Past the grandchild's own sleep: if it were still alive, the marker
	// would be here by now.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a grandchild outlived the process tree it was killed with")
	}
}

// Cancelling a run is the same kill on a different trigger, and it has the
// same five-second budget.
func TestLaunchStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	requireLaunchable(t, dir)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	_, err := Run(ctx, Launch{
		Binary: "/bin/sh", Args: []string{"-c", "sleep 60"},
		Sandbox: sandboxFor(t, dir), Timeout: time.Minute, Grace: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a cancelled launch reported success")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancel took %s, past the run cancel budget", elapsed)
	}
}

func TestLaunchNeedsABinary(t *testing.T) {
	if _, err := Run(t.Context(), Launch{Sandbox: sandboxFor(t, t.TempDir())}); err == nil {
		t.Fatal("an empty launch was accepted")
	}
}
