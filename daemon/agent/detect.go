package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// Host is the machine state an auth probe reads. It is injected rather than
// read directly so a test can describe a laptop it is not running on.
type Host struct {
	Getenv func(string) string
	// Exists reports whether a path is there. Directories count.
	Exists func(string) bool
	// HomeDir is the operator's real home — not the sandbox's, which is the
	// run workspace and never holds a credential.
	HomeDir string
}

// RealHost reads this machine.
func RealHost() Host {
	home, _ := os.UserHomeDir()
	return Host{
		Getenv:  os.Getenv,
		Exists:  func(p string) bool { _, err := os.Stat(p); return err == nil },
		HomeDir: home,
	}
}

func (h Host) getenv(k string) string {
	if h.Getenv == nil {
		return ""
	}
	return h.Getenv(k)
}

func (h Host) exists(p string) bool {
	if h.Exists == nil || p == "" {
		return false
	}
	return h.Exists(p)
}

// anySet reports whether any of the named variables has a non-empty value.
func (h Host) anySet(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(h.getenv(k)); v != "" {
			return k
		}
	}
	return ""
}

// AuthProbe answers whether the operator has logged this CLI in on this
// machine.
//
// It is a question about local configuration and never a request to the
// vendor: detection runs on every connect, and a probe that spent an API call
// per CLI would bill the operator for a heartbeat. That makes it a heuristic,
// and it is deliberately biased towards "ready" — a false "unauthenticated"
// hides a working runtime, while a false "ready" costs one run that fails with
// the CLI's own login message, which is the clearer error anyway.
type AuthProbe func(cli CLI, path string, host Host) (Readiness, string)

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
	// Login is what a human runs to authenticate it, printed by doctor when
	// the binary is there and the credential is not.
	Login string
	// Auth reports whether that login has happened.
	Auth AuthProbe
}

// Known is every CLI a runtime can report in its hello frame. The binary names
// are the ones each vendor ships: Antigravity's CLI is `agy`.
var Known = []CLI{
	{
		Name: qaschema.AgentCapabilityNameClaude, Binary: "claude", Args: []string{"--version"},
		Install: "npm i -g @anthropic-ai/claude-code",
		Login:   "claude setup-token, or run `claude` once and log in",
		Auth:    claudeAuth,
	},
	{
		Name: qaschema.AgentCapabilityNameOpencode, Binary: "opencode", Args: []string{"--version"},
		Install: "npm i -g opencode-ai",
		Login:   "opencode auth login",
		Auth:    opencodeAuth,
	},
	{
		Name: qaschema.AgentCapabilityNameAntigravity, Binary: "agy", Args: []string{"--version"},
		Install: "see the Antigravity CLI docs",
		Login:   "agy login",
		Auth:    antigravityAuth,
	},
}

// KnownCLI returns the recipe for one name.
func KnownCLI(name qaschema.AgentCapabilityName) (CLI, bool) {
	for _, cli := range Known {
		if cli.Name == name {
			return cli, true
		}
	}
	return CLI{}, false
}

// ClaudeConfigDir is where the Claude Code CLI keeps its credentials on this
// host. A provider mounts it read-only into the sandbox; without it the CLI
// starts in a workspace whose $HOME has never been logged in.
func ClaudeConfigDir(host Host) string {
	if dir := strings.TrimSpace(host.getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	if host.HomeDir == "" {
		return ""
	}
	return filepath.Join(host.HomeDir, ".claude")
}

func claudeAuth(_ CLI, _ string, host Host) (Readiness, string) {
	if key := host.anySet(
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	); key != "" {
		return ReadinessReady, "authenticated through " + key
	}
	dir := ClaudeConfigDir(host)
	// On macOS the OAuth token is in the keychain rather than a file, so the
	// config directory existing at all is taken as evidence of a login. See
	// AuthProbe on why this leans towards "ready".
	for _, marker := range []string{".credentials.json", ".claude.json", "settings.json"} {
		if host.exists(filepath.Join(dir, marker)) {
			return ReadinessReady, ""
		}
	}
	return ReadinessUnauthenticated, fmt.Sprintf("claude is installed but no credential was found in %s", dir)
}

func opencodeAuth(_ CLI, _ string, host Host) (Readiness, string) {
	if key := host.anySet("OPENCODE_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"); key != "" {
		return ReadinessReady, "authenticated through " + key
	}
	dirs := []string{}
	if data := strings.TrimSpace(host.getenv("XDG_DATA_HOME")); data != "" {
		dirs = append(dirs, filepath.Join(data, "opencode"))
	}
	if host.HomeDir != "" {
		dirs = append(dirs, filepath.Join(host.HomeDir, ".local", "share", "opencode"))
	}
	for _, dir := range dirs {
		if host.exists(filepath.Join(dir, "auth.json")) {
			return ReadinessReady, ""
		}
	}
	return ReadinessUnauthenticated, "opencode is installed but no credential was found"
}

func antigravityAuth(_ CLI, _ string, host Host) (Readiness, string) {
	if key := host.anySet("AGY_API_KEY", "ANTIGRAVITY_API_KEY"); key != "" {
		return ReadinessReady, "authenticated through " + key
	}
	if host.HomeDir != "" && host.exists(filepath.Join(host.HomeDir, ".antigravity")) {
		return ReadinessReady, ""
	}
	return ReadinessUnauthenticated, "agy is installed but no credential was found"
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
	// Host is the machine the auth probes read; the zero value reads this one.
	Host *Host
	// Auth overrides every CLI's own probe. Tests use it to describe a
	// machine where a CLI is, or is not, logged in.
	Auth AuthProbe
}

func (o DetectOptions) host() Host {
	if o.Host != nil {
		return *o.Host
	}
	return RealHost()
}

// Detect reports every known CLI in the wire form the hello frame carries.
func Detect(ctx context.Context, opts DetectOptions) []qaschema.AgentCapability {
	found := DetectAll(ctx, opts)
	out := make([]qaschema.AgentCapability, len(found))
	for i, c := range found {
		out[i] = c.Schema()
	}
	return out
}

// DetectAll reports every known CLI and, for each, the three-way readiness.
//
// A CLI that is missing or broken is reported unusable with a reason, never
// omitted: the operator picking an agent in the UI needs to see that
// `opencode` exists as an option and why this runtime cannot offer it.
// Detection runs the CLIs concurrently because three sequential Node startups
// is most of a second on a cold cache, and this runs on every connect.
func DetectAll(ctx context.Context, opts DetectOptions) []Capability {
	clis := make([]CLI, 0, len(Known))
	for _, cli := range Known {
		if len(opts.Only) == 0 || containsName(opts.Only, cli.Name) {
			clis = append(clis, cli)
		}
	}

	out := make([]Capability, len(clis))
	var wg sync.WaitGroup
	for i, cli := range clis {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = DetectOne(ctx, cli, opts)
		}()
	}
	wg.Wait()
	return out
}

// DetectOne runs the version probe and then the auth probe for a single CLI.
// The order matters: "not installed" and "not logged in" are different
// answers, and asking about a credential for a binary that is not there would
// report the wrong one.
func DetectOne(ctx context.Context, cli CLI, opts DetectOptions) Capability {
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	capability := Capability{Name: cli.Name, Readiness: ReadinessMissing}

	path, err := lookPath(cli.Binary)
	if err != nil {
		capability.Detail = fmt.Sprintf("%s is not on PATH (install: %s)", cli.Binary, cli.Install)
		return capability
	}
	capability.Path = path

	// The version probe runs in its own process group: several of these CLIs
	// are wrapper scripts, and killing only the wrapper leaves the real
	// process holding the output pipe, which is indistinguishable from a hang.
	var output syncBuffer
	cmd, err := proc.Start(proc.Options{Name: path, Args: cli.Args, Stdout: &output, Stderr: &output})
	if err != nil {
		capability.Detail = fmt.Sprintf("%s could not be started: %v", cli.Binary, err)
		return capability
	}

	select {
	case <-cmd.Done():
		err = cmd.Wait()
	case <-time.After(timeout):
		_ = cmd.Terminate(ctx, time.Second)
		capability.Detail = fmt.Sprintf("%s %s did not answer within %s", cli.Binary, strings.Join(cli.Args, " "), timeout)
		return capability
	case <-ctx.Done():
		_ = cmd.Terminate(context.WithoutCancel(ctx), time.Second)
		capability.Detail = fmt.Sprintf("%s %s was interrupted: %v", cli.Binary, strings.Join(cli.Args, " "), ctx.Err())
		return capability
	}

	if err != nil {
		capability.Detail = fmt.Sprintf("%s %s failed: %s", cli.Binary, strings.Join(cli.Args, " "), firstLine(output.String(), err))
		return capability
	}

	capability.Version = truncate(firstLine(output.String(), nil), 64)

	probe := opts.Auth
	if probe == nil {
		probe = cli.Auth
	}
	if probe == nil {
		// A CLI with no probe is taken at its word rather than reported
		// broken: a vendor we cannot inspect is not the operator's fault.
		capability.Readiness = ReadinessReady
		return capability
	}

	readiness, detail := probe(cli, path, opts.host())
	capability.Readiness = readiness
	switch readiness {
	case ReadinessReady:
		capability.Detail = ""
	case ReadinessUnauthenticated:
		capability.Detail = detail
		if cli.Login != "" {
			capability.Detail += " (log in: " + cli.Login + ")"
		}
	default:
		capability.Detail = detail
	}
	return capability
}

// ErrNotDetected is returned by a provider whose CLI this machine cannot use.
var ErrNotDetected = errors.New("agent: CLI is not usable on this machine")

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
