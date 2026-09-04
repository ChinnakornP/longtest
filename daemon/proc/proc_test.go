package proc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTerminateKillsGrandchildren is the acceptance test behind "cancel leaves
// no process behind": the daemon spawns a shell, the shell spawns a sleeper,
// and terminating the shell must take the sleeper with it.
func TestTerminateKillsGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture is POSIX-only")
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The grandchild deliberately ignores SIGTERM, so a test that passes here
	// proves the group was killed rather than the child politely relaying.
	script := "trap '' TERM\nsleep 300 &\necho $! > " + pidFile + "\nwait\n"
	scriptPath := filepath.Join(dir, "spawn.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	var stderr bytes.Buffer
	cmd, err := Start(Options{Name: "/bin/sh", Args: []string{scriptPath}, Stderr: &stderr})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	grandchild := waitForPIDFile(t, pidFile)
	if !processAlive(grandchild) {
		t.Fatalf("grandchild %d never started", grandchild)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := cmd.Terminate(ctx, 200*time.Millisecond); err != nil {
		t.Fatalf("terminate: %v (stderr: %s)", err, stderr.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("terminate took %s, budget is 5s", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for processAlive(grandchild) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the group kill", grandchild)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminateIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	cmd, err := Start(Options{Name: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	ctx := t.Context()
	for i := range 2 {
		if err := cmd.Terminate(ctx, 50*time.Millisecond); err != nil {
			t.Fatalf("terminate #%d: %v", i, err)
		}
	}
}

func TestPipeRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	cmd, err := Start(Options{Name: "/bin/cat", Pipe: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Terminate(context.Background(), 50*time.Millisecond) })

	if _, err := cmd.Stdin().Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := cmd.Stdout().Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf); got != "ping\n" {
		t.Fatalf("read %q, want %q", got, "ping\n")
	}
}

func TestStartRejectsEmptyCommand(t *testing.T) {
	if _, err := Start(Options{}); err == nil {
		t.Fatal("expected an error for an empty command")
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path) //nolint:gosec // test-owned path
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processAlive(pid int) bool {
	// `kill -0` rather than os.FindProcess+Signal(0): the latter reports a
	// zombie as alive, and the shell's own child is reaped by its parent.
	return exec.CommandContext(context.Background(), "kill", "-0", strconv.Itoa(pid)).Run() == nil
}
