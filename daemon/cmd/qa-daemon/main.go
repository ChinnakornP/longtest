// Command qa-daemon is the QA runtime agent. It runs on the operator's own
// machine, dials the backend over a single outbound WebSocket, and drives
// Chromium through the Node executor sidecar.
//
// It opens no inbound port (ADR-002), which is what lets it test an
// application that is only reachable from inside a corporate network:
// localhost:3000, 192.168.1.20, staging.internal.
//
// Usage:
//
//	qa-daemon pair --code <pairing-code> --server <url> [--name <runtime>]
//	qa-daemon start [--config <path>] [--verbose]
//	qa-daemon status [--output json]
//	qa-daemon doctor [--output json]
//	qa-daemon version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/ChinnakornP/longtest/daemon/runtime"
	"github.com/ChinnakornP/longtest/daemon/security"
)

func main() {
	// First, before flags, signals or anything else is set up: a re-exec of
	// this binary as a sandbox stub has to apply its restrictions and hand off
	// to the target program. security.Spec launches every sandboxed child by
	// re-execing us with a marker argv, so that the rlimits and the Landlock
	// ruleset are applied in the child, after fork and before exec — there is
	// then no window in which the process exists unrestricted. Without this
	// dispatch the re-exec would fall through to the CLI parser below, print
	// "unknown command", and the sandbox would silently do nothing.
	if security.IsSandboxStub() {
		security.RunSandboxStub()
	}

	// Signals are handled here rather than in the runtime package so that a
	// second Ctrl+C is fatal: the first asks for a graceful shutdown, and an
	// operator who presses it again wants out now, not another wait.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintf(os.Stderr, "qa-daemon: %v\n", err)
		}
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errUsage
	}

	switch args[0] {
	case "pair":
		return runPair(ctx, args[1:], stdout, stderr)
	case "start":
		return runStart(ctx, args[1:], stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-V":
		printer{stdout}.printf("qa-daemon %s\n", runtime.Version)
		return nil
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		printer{stderr}.printf("qa-daemon: unknown command %q\n\n", args[0])
		usage(stderr)
		return errUsage
	}
}

func usage(w io.Writer) {
	printer{w}.print(`qa-daemon - the AI QA runtime agent

Commands:
  pair     Exchange a one-time pairing code for this machine's runtime token
  start    Connect to the backend and execute assigned runs
  status   Print what the running daemon is doing
  doctor   Diagnose why this machine cannot run tests
  version  Print the daemon version

Run "qa-daemon <command> -h" for the flags of one command.
`)
}

// printer writes command output. Its Fprintf return value is dropped in one
// place rather than at every call site: a CLI that cannot write to its own
// stdout has nothing useful left to do, and threading that error through every
// line of a status report buries the report in error handling.
type printer struct{ w io.Writer }

func (p printer) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.w, format, args...)
}

func (p printer) print(text string) {
	_, _ = fmt.Fprint(p.w, text)
}

// newFlagSet builds a flag set that reports errors through the returned error
// rather than exiting the process, so main owns every exit path.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}
