package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The acceptance criterion in full: cancel a run that is driving a real,
// unresponsive sidecar with a child of its own, and both processes must be
// gone inside the five-second budget.
//
// The other cancel tests use a fake executor, which proves the daemon's
// bookkeeping. This one proves the part that actually leaks in production: a
// browser the sidecar forked.
func TestCancelKillsARealProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell fixture is POSIX-only")
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	// A sidecar that ignores SIGTERM and never answers a request: the worst
	// case cancel has to handle. Killing only the shell would leave `sleep`
	// running, which is what a stranded Chromium looks like.
	script := "trap '' TERM\nsleep 300 &\necho $! > " + pidFile + "\nwait\n"
	scriptPath := filepath.Join(dir, "sidecar.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var started *executor.Client
	h := newHarness(t, harnessOptions{
		newExec: func(opts executor.Options) (ExecutorClient, error) {
			client, err := executor.Start(executor.Options{
				Command:     []string{"/bin/sh", scriptPath},
				Dir:         opts.Dir,
				Logger:      opts.Logger,
				CallTimeout: 30 * time.Second,
			})
			started = client
			return client, err
		},
	})

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	assign := assignFrame(t, assignOptions{
		withMap:   true,
		testCases: []any{testCase("TC-001", "Create employee")},
	})
	h.backend.Send(assign)

	grandchild := waitForPID(t, pidFile)
	if !alive(grandchild) {
		t.Fatalf("the fixture's child %d never started", grandchild)
	}
	sidecar := started.PID()

	runID := decodeAs[qaschema.RunAssignPayload](t, assign.Payload).RunID
	start := time.Now()
	h.backend.Send(cancelFrame(t, runID, qaschema.RunCancelPayloadReasonUserRequested, "stop"))

	result := h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeRunResult)
	if payload := decodeAs[qaschema.RunResultPayload](t, result.Payload); payload.Status != qaschema.RunResultPayloadStatusCancelled {
		t.Fatalf("status = %q", payload.Status)
	}

	for _, pid := range []int{sidecar, grandchild} {
		deadline := start.Add(5 * time.Second)
		for alive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d was still alive %s after the cancel", pid, time.Since(start))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Logf("sidecar %d and its child %d were gone %s after the cancel", sidecar, grandchild, time.Since(start))
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path) //nolint:gosec // test-owned path
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fixture never wrote %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func alive(pid int) bool {
	return exec.CommandContext(context.Background(), "kill", "-0", strconv.Itoa(pid)).Run() == nil
}
