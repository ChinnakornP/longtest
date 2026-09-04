package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/proc"
)

// syncBuffer collects a child's stdout and stderr. os/exec writes both from
// its own goroutines, so the buffer needs a lock even though only one
// goroutine reads it afterwards.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// CLI is one AI coding CLI the daemon knows how to launch.
//
// The product deliberately does not hold LLM API keys: it drives whichever CLI
// the operator already installed and authenticated (ADR-003). Detection is
// therefore a question about this machine, not about an account.
type CLI struct {
	Name   qaschema.AgentCapabilityName
	Binary string
	Args   []string
	// Install is what a human runs to get it, printed by doctor.
	Install string
}

// Known is every CLI a runtime can report in its hello frame. The binary names
// are the ones each vendor ships: Antigravity's CLI is `agy`.
var Known = []CLI{
	{Name: qaschema.AgentCapabilityNameClaude, Binary: "claude", Args: []string{"--version"},
		Install: "npm i -g @anthropic-ai/claude-code"},
	{Name: qaschema.AgentCapabilityNameOpencode, Binary: "opencode", Args: []string{"--version"},
		Install: "npm i -g opencode-ai"},
	{Name: qaschema.AgentCapabilityNameAntigravity, Binary: "agy", Args: []string{"--version"},
		Install: "see the Antigravity CLI docs"},
}

// DetectOptions customise detection, mainly for tests.
type DetectOptions struct {
	// LookPath resolves a binary name; nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Timeout bounds each `--version` call. A CLI that hangs on startup is
	// reported unusable rather than allowed to stall the hello frame.
	Timeout time.Duration
	// Only restricts detection to these CLIs; empty means all of Known.
	Only []qaschema.AgentCapabilityName
}

// Detect reports every known CLI and, for each, whether it can be used.
//
// A CLI that is missing or broken is reported with ok:false and a reason,
// never omitted: the operator picking an agent in the UI needs to see that
// `opencode` exists as an option and why this runtime cannot offer it.
// Detection runs the CLIs concurrently because three sequential Node startups
// is most of a second on a cold cache, and this runs on every connect.
func Detect(ctx context.Context, opts DetectOptions) []qaschema.AgentCapability {
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	clis := make([]CLI, 0, len(Known))
	for _, cli := range Known {
		if len(opts.Only) == 0 || containsName(opts.Only, cli.Name) {
			clis = append(clis, cli)
		}
	}

	out := make([]qaschema.AgentCapability, len(clis))
	var wg sync.WaitGroup
	for i, cli := range clis {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = detectOne(ctx, cli, lookPath, timeout)
		}()
	}
	wg.Wait()
	return out
}

func detectOne(ctx context.Context, cli CLI, lookPath func(string) (string, error), timeout time.Duration) qaschema.AgentCapability {
	capability := qaschema.AgentCapability{Name: cli.Name}

	path, err := lookPath(cli.Binary)
	if err != nil {
		capability.Error = ptr(fmt.Sprintf("%s is not on PATH (install: %s)", cli.Binary, cli.Install))
		return capability
	}

	// The version probe runs in its own process group: several of these CLIs
	// are wrapper scripts, and killing only the wrapper leaves the real
	// process holding the output pipe, which is indistinguishable from a hang.
	var output syncBuffer
	cmd, err := proc.Start(proc.Options{Name: path, Args: cli.Args, Stdout: &output, Stderr: &output})
	if err != nil {
		capability.Error = ptr(fmt.Sprintf("%s could not be started: %v", cli.Binary, err))
		return capability
	}

	select {
	case <-cmd.Done():
		err = cmd.Wait()
	case <-time.After(timeout):
		_ = cmd.Terminate(ctx, time.Second)
		capability.Error = ptr(fmt.Sprintf("%s %s did not answer within %s", cli.Binary, strings.Join(cli.Args, " "), timeout))
		return capability
	case <-ctx.Done():
		_ = cmd.Terminate(context.WithoutCancel(ctx), time.Second)
		capability.Error = ptr(fmt.Sprintf("%s %s was interrupted: %v", cli.Binary, strings.Join(cli.Args, " "), ctx.Err()))
		return capability
	}

	switch {
	case err != nil:
		capability.Error = ptr(fmt.Sprintf("%s %s failed: %s", cli.Binary, strings.Join(cli.Args, " "), firstLine(output.String(), err)))
		return capability
	}

	capability.Ok = true
	if version := firstLine(output.String(), nil); version != "" {
		capability.Version = ptr(truncate(version, 64))
	}
	return capability
}

// firstLine keeps a CLI banner from turning into a multi-line error message on
// a dashboard. Output is the CLI's, not a page's, but it is still not ours.
func firstLine(output string, fallback error) string {
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	if fallback != nil {
		return fallback.Error()
	}
	return ""
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func containsName(names []qaschema.AgentCapabilityName, want qaschema.AgentCapabilityName) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
