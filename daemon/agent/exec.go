package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/ChinnakornP/longtest/daemon/proc"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// Launch is one child process a provider wants run.
//
// Providers do not build their own exec.Cmd. Going through here is what
// guarantees three things every CLI needs and none of them should have to
// remember: the process is confined to the run workspace, it dies at the
// deadline, and it takes its children with it when it does.
type Launch struct {
	// Binary is the resolved executable, and Args its arguments.
	Binary string
	Args   []string

	// Stdin is written to the child. It is used for the prompt, which keeps
	// it off the process table where `ps` would show it to every account on
	// the machine.
	Stdin []byte

	// Stdout and Stderr collect the child's output for the attempt log. They
	// are a debugging record, never a protocol: the answer is the file the
	// CLI writes (ADR-003).
	Stdout io.Writer
	Stderr io.Writer

	// Sandbox confines the child. Its WorkspaceDir is also the working
	// directory and $HOME.
	Sandbox security.Spec

	// Timeout kills the process tree. Zero means DefaultTimeout.
	Timeout time.Duration

	// Grace is how long the tree gets to exit after the polite signal before
	// it is killed outright. Zero means proc.DefaultGrace.
	Grace time.Duration
}

// LaunchResult is how the child ended.
type LaunchResult struct {
	ExitCode int
	TimedOut bool
	Duration time.Duration
}

// Run starts the child and blocks until it exits, the timeout fires, or ctx is
// cancelled. It returns only once the process tree is gone in all three cases.
//
// A non-zero exit is not an error here: several of these CLIs exit non-zero
// after writing a perfectly good answer, and the caller decides what the exit
// code means once it has looked at the output file.
func Run(ctx context.Context, l Launch) (LaunchResult, error) {
	if l.Binary == "" {
		return LaunchResult{}, errors.New("agent: no binary to launch")
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// The wall-clock limit is enforced twice on purpose: here, where we can
	// report it as agent_timeout, and inside the sandbox as an rlimit, which
	// survives a daemon that was itself killed mid-run.
	spec := l.Sandbox
	if spec.Limits.Wall <= 0 || spec.Limits.Wall > timeout {
		spec.Limits.Wall = timeout
	}

	// Not CommandContext's context: it signals only the direct child, and the
	// thing we started is a sandbox stub that execs the real CLI, which in
	// turn spawns its own tree. Terminate below is the cancellation path.
	cmd, err := spec.Command(context.WithoutCancel(ctx), l.Binary, l.Args...)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent: build sandboxed command: %w", err)
	}
	if l.Stdin != nil {
		cmd.Stdin = bytes.NewReader(l.Stdin)
	}
	cmd.Stdout, cmd.Stderr = l.Stdout, l.Stderr

	started := time.Now()
	child, err := proc.Adopt(cmd)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent: start %s: %w", l.Binary, err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-child.Done():
		return LaunchResult{
			ExitCode: exitCode(child.Wait()),
			Duration: time.Since(started),
		}, nil

	case <-timer.C:
		// context.WithoutCancel: the run may already be cancelled, and the
		// kill still has to be given its grace period rather than being
		// abandoned halfway with the tree half-signalled.
		if err := child.Terminate(context.WithoutCancel(ctx), l.Grace); err != nil {
			return LaunchResult{TimedOut: true, Duration: time.Since(started)},
				fmt.Errorf("agent: %s did not die at the deadline: %w", l.Binary, err)
		}
		return LaunchResult{TimedOut: true, ExitCode: -1, Duration: time.Since(started)}, nil

	case <-ctx.Done():
		if err := child.Terminate(context.WithoutCancel(ctx), l.Grace); err != nil {
			return LaunchResult{Duration: time.Since(started)},
				fmt.Errorf("agent: %s did not die on cancel: %w", l.Binary, err)
		}
		return LaunchResult{ExitCode: -1, Duration: time.Since(started)}, ctx.Err()
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
