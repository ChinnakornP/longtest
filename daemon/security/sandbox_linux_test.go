//go:build linux

package security_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/security"
)

// specFor builds a sandbox spec whose stub is this test binary.
func specFor(t *testing.T, workspace string) security.Spec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return security.Spec{
		WorkspaceDir: workspace,
		Limits:       security.DefaultAgentLimits(),
		Network:      security.NetworkHost,
		EnvAllow:     security.BaseEnvAllow(),
		SelfExe:      self,
	}
}

func requireLandlock(t *testing.T) {
	t.Helper()
	// The confinement tests assert that a read outside the workspace fails.
	// On a kernel without Landlock the child would succeed, and a test that
	// silently passes because the check did not run is worse than no test:
	// skip loudly instead.
	dir := t.TempDir()
	spec := specFor(t, dir)
	cmd, err := spec.Command(context.Background(), "/bin/sh", "-c", "exit 0")
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "landlock") {
			t.Skipf("kernel has no usable landlock support: %s", stderr.String())
		}
		t.Fatalf("probe run failed: %v (stderr=%s)", err, stderr.String())
	}
}

func run(t *testing.T, spec security.Spec, name string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, err := spec.Command(ctx, name, args...)
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestSandboxConfinesReadsToTheWorkspace(t *testing.T) {
	requireLandlock(t)

	// A file outside the workspace that is not on the read-only allowlist:
	// this stands in for ~/.ssh/id_ed25519, another tenant's run directory,
	// or the daemon's own config.
	outside := filepath.Join(t.TempDir(), "operator-secret.txt")
	if err := os.WriteFile(outside, []byte("SSH PRIVATE KEY"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	ws := t.TempDir()
	spec := specFor(t, ws)

	stdout, _, err := run(t, spec, "/bin/sh", "-c", "cat "+outside)
	if err == nil {
		t.Fatalf("read outside the workspace succeeded, got %q", stdout)
	}
	if strings.Contains(stdout, "SSH PRIVATE KEY") {
		t.Fatalf("file contents leaked out of the sandbox: %q", stdout)
	}
}

func TestSandboxConfinesWritesToTheWorkspace(t *testing.T) {
	requireLandlock(t)

	outsideDir := t.TempDir()
	ws := t.TempDir()
	spec := specFor(t, ws)

	target := filepath.Join(outsideDir, "planted.txt")
	if _, _, err := run(t, spec, "/bin/sh", "-c", "echo pwned > "+target); err == nil {
		t.Fatal("write outside the workspace succeeded")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was created outside the workspace: %v", err)
	}
}

func TestSandboxAllowsTheWorkspaceItself(t *testing.T) {
	requireLandlock(t)

	ws := t.TempDir()
	spec := specFor(t, ws)

	if _, stderr, err := run(t, spec, "/bin/sh", "-c", "echo out.json > out.json && cat out.json"); err != nil {
		t.Fatalf("write inside the workspace failed: %v (stderr=%s)", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(ws, "out.json")); err != nil {
		t.Fatalf("expected the file to exist inside the workspace: %v", err)
	}
}

// A symlink is the classic way out of a directory-shaped jail. Landlock
// resolves it and refuses, so the escape hatch a compromised agent would
// reach for first is closed.
func TestSandboxRefusesToFollowASymlinkOut(t *testing.T) {
	requireLandlock(t)

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ws := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	stdout, _, err := run(t, specFor(t, ws), "/bin/sh", "-c", "cat link")
	if err == nil || strings.Contains(stdout, "OUTSIDE") {
		t.Fatalf("symlink escape succeeded: err=%v stdout=%q", err, stdout)
	}
}

func TestSandboxDoesNotInheritTheDaemonEnvironment(t *testing.T) {
	// The daemon's environment holds the runtime pairing token and the
	// artifact-store credentials. Only the allowlist crosses into a child.
	t.Setenv("DAEMON_PAIRING_TOKEN", "pairing-token-must-not-cross")
	t.Setenv("S3_SECRET_ACCESS_KEY", "artifact-secret-must-not-cross")

	ws := t.TempDir()
	stdout, _, err := run(t, specFor(t, ws), "/usr/bin/env")
	if err != nil {
		t.Fatalf("env failed: %v", err)
	}
	for _, forbidden := range []string{"pairing-token-must-not-cross", "artifact-secret-must-not-cross"} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("daemon environment leaked into the sandbox: %q", stdout)
		}
	}
	if !strings.Contains(stdout, "HOME="+ws) {
		t.Fatalf("expected HOME to be the workspace, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "PATH=") {
		t.Fatalf("expected the allowlisted PATH to be present, got:\n%s", stdout)
	}
}

func TestSandboxDoesNotLeakItsOwnSpec(t *testing.T) {
	ws := t.TempDir()
	stdout, _, err := run(t, specFor(t, ws), "/usr/bin/env")
	if err != nil {
		t.Fatalf("env failed: %v", err)
	}
	if strings.Contains(stdout, "QA_SANDBOX_SPEC") {
		t.Fatalf("the sandbox spec reached the sandboxed program:\n%s", stdout)
	}
}

func TestSandboxCapsFileSize(t *testing.T) {
	requireLandlock(t)

	ws := t.TempDir()
	spec := specFor(t, ws)
	spec.Limits.MaxFileBytes = 64 << 10

	// dd writes past the limit; RLIMIT_FSIZE turns that into SIGXFSZ.
	_, _, err := run(t, spec, "/bin/sh", "-c",
		"dd if=/dev/zero of=big bs=1024 count=512 2>/dev/null")
	if err == nil {
		t.Fatal("expected the oversized write to be refused")
	}
	if info, statErr := os.Stat(filepath.Join(ws, "big")); statErr == nil && info.Size() > int64(spec.Limits.MaxFileBytes) {
		t.Fatalf("file grew past RLIMIT_FSIZE: %d bytes", info.Size())
	}
}

func TestSandboxWithNetworkNoneHasNoRoute(t *testing.T) {
	ws := t.TempDir()
	spec := specFor(t, ws)
	spec.Network = security.NetworkNone

	// An empty network namespace has no interfaces at all, so even loopback
	// is gone. Reading /proc/net/dev is enough to see it without depending on
	// a network tool being installed.
	stdout, stderr, err := run(t, spec, "/bin/sh", "-c", "ip -o link show 2>/dev/null | wc -l")
	if err != nil {
		if strings.Contains(stderr, "operation not permitted") || strings.Contains(stderr, "EPERM") {
			t.Skip("unprivileged user namespaces are disabled on this host")
		}
		t.Skipf("could not create a network namespace here: %v (stderr=%s)", err, stderr)
	}
	if n := strings.TrimSpace(stdout); n != "1" && n != "0" {
		// A fresh netns has only a downed `lo`. Anything more means the child
		// kept the host's interfaces.
		t.Fatalf("expected an empty network namespace, got %s interfaces", n)
	}
}

// Regression: RLIMIT_NPROC is charged per task, not per process, and is
// shared with every other program this uid runs. An absolute ceiling made
// fork() fail on any real workstation, which the shell reports as a retry
// loop rather than an error — a hang with no obvious cause.
func TestSandboxLeavesRoomToFork(t *testing.T) {
	requireLandlock(t)

	spec := specFor(t, t.TempDir())
	stdout, stderr, err := run(t, spec, "/bin/sh", "-c", "for i in 1 2 3 4 5; do /bin/echo ok; done")
	if err != nil {
		t.Fatalf("forking a handful of children failed: %v (stderr=%s)", err, stderr)
	}
	if strings.Count(stdout, "ok") != 5 {
		t.Fatalf("expected 5 forked children to run, got %q (stderr=%s)", stdout, stderr)
	}
}

// /dev/null has to stay writable: `2>/dev/null` appears in more or less every
// command line an agent will produce, and denying it fails the command with a
// permission error that looks like a bug in the command.
func TestSandboxAllowsWritingToDevNull(t *testing.T) {
	requireLandlock(t)

	spec := specFor(t, t.TempDir())
	if _, stderr, err := run(t, spec, "/bin/sh", "-c", "echo noise > /dev/null"); err != nil {
		t.Fatalf("writing to /dev/null failed: %v (stderr=%s)", err, stderr)
	}
}

func TestSandboxRequiresAWorkspace(t *testing.T) {
	var spec security.Spec
	if _, err := spec.Command(context.Background(), "/bin/true"); err == nil {
		t.Fatal("expected a spec without a workspace to be refused")
	}
}

func TestSandboxProxyPolicyNeedsAProxy(t *testing.T) {
	spec := specFor(t, t.TempDir())
	spec.Network = security.NetworkProxy
	if _, err := spec.Command(context.Background(), "/bin/true"); err == nil {
		t.Fatal("expected NetworkProxy without a ProxyURL to be refused")
	}
}
