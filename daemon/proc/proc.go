// Package proc starts child processes in their own process group so the daemon
// can kill a whole tree, not just the process it spawned.
//
// This matters because everything the daemon runs spawns children of its own:
// the executor is a Node process that forks Chromium, and an AI CLI is a
// wrapper script around a long-lived agent. Signalling only the direct child
// leaves Chromium holding a profile directory and an AI CLI holding a network
// connection, which is exactly the "process ค้าง" a run cancel must not leave
// behind (contract: cancel completes within 5 seconds).
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// DefaultGrace is how long a process tree is given to exit after the polite
// signal before it is killed outright. It is deliberately well inside the
// 5-second cancel budget so the kill, the wait and the result frame all fit.
const DefaultGrace = 2 * time.Second

// Options describes a child process. Stdin and Stdout are wired to OS pipes
// when Pipe is set; stderr always goes to Stderr (nil discards it).
type Options struct {
	Name string
	Args []string
	Dir  string
	// Env replaces the environment entirely when non-nil. A nil Env inherits
	// the daemon's, which is what lets an AI CLI find its own credentials.
	Env []string
	// Stdout and Stderr collect the child's output when Pipe is not set.
	// A nil writer discards that stream.
	Stdout io.Writer
	Stderr io.Writer
	// Pipe requests stdin/stdout pipes for a JSON-RPC conversation, and takes
	// precedence over Stdout.
	Pipe bool
}

// Cmd is a running child process and its process group.
type Cmd struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File

	// childEnds are this process's copies of the descriptors handed to the
	// child. They are closed as soon as it has started.
	childEnds []*os.File

	done    chan struct{}
	waitErr error
}

// Start launches the process. The returned Cmd is running; the caller owns
// Terminate.
//
// The process is placed in a new process group before exec, which is what
// makes killTree possible later. Doing it after start would race a fast child
// that has already forked.
func Start(opts Options) (*Cmd, error) {
	if opts.Name == "" {
		return nil, errors.New("proc: no command")
	}

	// exec.Command rather than CommandContext on purpose: CommandContext
	// signals only the direct child, which is exactly the behaviour this
	// package exists to replace. Terminate is the cancellation path.
	//nolint:gosec,noctx // G204: the command comes from daemon config, not from a page under test.
	cmd := exec.Command(opts.Name, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	setGroup(cmd)

	c := &Cmd{cmd: cmd, done: make(chan struct{})}

	if opts.Pipe {
		// Real OS pipes rather than exec's io.Reader/io.Writer copying: with
		// *os.File on both sides, os/exec spawns no copy goroutine, so Wait
		// returns as soon as the process does and never blocks on a reader
		// that is still draining. The parent ends are ours to close.
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("proc: stdin pipe: %w", err)
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			_ = inR.Close()
			_ = inW.Close()
			return nil, fmt.Errorf("proc: stdout pipe: %w", err)
		}
		cmd.Stdin, cmd.Stdout = inR, outW
		c.stdin, c.stdout = inW, outR
		c.childEnds = []*os.File{inR, outW}
	}

	if err := cmd.Start(); err != nil {
		c.closePipes()
		c.closeChildEnds()
		return nil, fmt.Errorf("proc: start %s: %w", opts.Name, err)
	}

	// os/exec closes only the descriptors it created itself; a *os.File the
	// caller supplied stays open in this process. Holding the child's write
	// end would mean stdout never reports EOF when the child dies, and a
	// reader waiting on it would hang instead of noticing the exit.
	c.closeChildEnds()

	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
	}()

	return c, nil
}

// Adopt starts a command somebody else built and gives it this package's
// process-group handling.
//
// It exists for the sandbox: security.Spec.Command returns an *exec.Cmd whose
// SysProcAttr already carries the clone flags and the parent-death signal that
// have to be requested at fork time, and rebuilding it from Options would drop
// them. Everything else — the new process group, Terminate killing the whole
// tree — is identical to Start.
//
// The caller owns cmd.Stdin, cmd.Stdout and cmd.Stderr; Options.Pipe has no
// equivalent here.
func Adopt(cmd *exec.Cmd) (*Cmd, error) {
	if cmd == nil {
		return nil, errors.New("proc: no command")
	}
	setGroup(cmd)

	c := &Cmd{cmd: cmd, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("proc: start %s: %w", cmd.Path, err)
	}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// Stdin is the write end of the child's stdin, or nil when Options.Pipe was
// not set.
func (c *Cmd) Stdin() io.WriteCloser {
	if c.stdin == nil {
		return nil
	}
	return c.stdin
}

// Stdout is the read end of the child's stdout, or nil when Options.Pipe was
// not set.
func (c *Cmd) Stdout() io.ReadCloser {
	if c.stdout == nil {
		return nil
	}
	return c.stdout
}

// PID is the direct child's process id. The group id is the same number.
func (c *Cmd) PID() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Done is closed once the direct child has been reaped.
func (c *Cmd) Done() <-chan struct{} { return c.done }

// Wait blocks until the child exits and returns its exit error, if any.
func (c *Cmd) Wait() error {
	<-c.done
	return c.waitErr
}

// Exited reports whether the direct child has been reaped.
func (c *Cmd) Exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// Terminate ends the whole process tree and waits for the direct child to be
// reaped. It signals the group politely, waits grace, then kills the group,
// and returns only once the child is gone or ctx expires.
//
// It is safe to call on an already-exited process, and safe to call twice.
func (c *Cmd) Terminate(ctx context.Context, grace time.Duration) error {
	if c.cmd.Process == nil {
		return nil
	}
	if c.Exited() {
		c.closePipes()
		return nil
	}
	if grace <= 0 {
		grace = DefaultGrace
	}

	// Closing stdin first is what lets a well-behaved sidecar shut down on its
	// own: the executor's stdio loop ends when its input does.
	if c.stdin != nil {
		_ = c.stdin.Close()
	}

	_ = c.signalGroup(termSignal)

	select {
	case <-c.done:
		c.closePipes()
		return nil
	case <-time.After(grace):
	case <-ctx.Done():
	}

	if err := c.signalGroup(killSignal); err != nil && !c.Exited() {
		return fmt.Errorf("proc: kill process group %d: %w", c.PID(), err)
	}

	// A killed process is reaped promptly; the only way this waits long is an
	// uninterruptible child, and then the caller's context is the backstop.
	select {
	case <-c.done:
		c.closePipes()
		return nil
	case <-ctx.Done():
		c.closePipes()
		return fmt.Errorf("proc: process group %d still alive: %w", c.PID(), ctx.Err())
	case <-time.After(grace):
		c.closePipes()
		return fmt.Errorf("proc: process group %d did not exit after kill", c.PID())
	}
}

func (c *Cmd) closeChildEnds() {
	for _, f := range c.childEnds {
		_ = f.Close()
	}
	c.childEnds = nil
}

func (c *Cmd) closePipes() {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
}
