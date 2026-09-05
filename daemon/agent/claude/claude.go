// Package claude runs Claude Code headless as an [agent.Provider].
//
// The exchange is files, not stdout (ADR-003): the prompt is written to the
// phase directory, the CLI is launched with that prompt on stdin and told to
// write out.json next to it, and the daemon reads that file back. Claude
// Code's own output format is a debugging convenience here and nothing more —
// it has changed shape several times, and a parser built on it would break on
// an `npm update` the operator ran for unrelated reasons.
//
// The CLI is launched through a security.Spec, so it runs with the run's
// workspace as its working directory and its $HOME, and can write nowhere
// else. Its credentials stay where the operator authenticated them: the
// config directory is mounted read-only and pointed at with CLAUDE_CONFIG_DIR,
// which lets the CLI read the token it was logged in with and not rewrite the
// operator's configuration.
package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// Name is the CLI this provider drives.
const Name = qaschema.AgentCapabilityNameClaude

// credentialEnv is every variable Claude Code authenticates with, in the four
// ways it can be configured: a direct API key, an OAuth token, Bedrock, or
// Vertex.
//
// It is a list and not a prefix match because the daemon's own environment
// holds the runtime pairing token and the artifact-store credentials, and an
// AI CLI that could read those would be one prompt injection away from
// registering itself as another runtime. Adding a variable here is a decision;
// a wildcard would be an accident waiting to be made.
var credentialEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_MODEL",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"AWS_REGION",
	"AWS_DEFAULT_REGION",
	"AWS_PROFILE",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"CLOUD_ML_REGION",
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"NODE_EXTRA_CA_CERTS",
}

// baseArgs is the headless invocation.
//
// Every flag is there to remove a degree of freedom rather than to add a
// feature:
//
//	-p                        answer and exit; no interactive session
//	--output-format text      the answer is out.json, so this is only a log
//	--restricted              no Bash, no code execution — the model plans,
//	                          the executor acts (the split ADR-003 is built on)
//	--allowedTools Read,Write,Edit,Glob,Grep
//	                          enough to read the inputs and write the answer
//	--permission-mode acceptEdits
//	                          nobody is at the terminal to approve a write
//	--disable-slash-commands  a skill in the workspace is model-reachable
//	                          configuration we did not put there
//	--strict-mcp-config       no MCP server the operator configured for their
//	                          own work joins a customer's test run
//	--no-session-persistence  the transcript would outlive the workspace, and
//	                          it holds the page content the run was about
var baseArgs = []string{
	"-p",
	"--output-format", "text",
	"--restricted",
	"--allowedTools", "Read,Write,Edit,Glob,Grep",
	"--permission-mode", "acceptEdits",
	"--disable-slash-commands",
	"--strict-mcp-config",
	"--no-session-persistence",
}

// Options configure the provider.
type Options struct {
	// Binary overrides PATH lookup. Empty means detection resolves `claude`.
	Binary string
	// Model is passed to --model. Empty leaves the CLI's own default, which
	// is what the operator selected when they logged in.
	Model string
	// ConfigDir overrides where the CLI's credentials are read from. Empty
	// means CLAUDE_CONFIG_DIR, then ~/.claude.
	ConfigDir string
	// ExtraArgs are appended to the invocation. They are for an operator
	// working around a CLI regression, not for per-run configuration: nothing
	// derived from a page or a model may reach them.
	ExtraArgs []string
	// Host is the machine detection reads; the zero value reads this one.
	Host *agent.Host
	// DetectTTL caches the detection result. Zero means DefaultDetectTTL.
	DetectTTL time.Duration
	// DetectOptions is passed through to detection, for tests.
	DetectOptions agent.DetectOptions
}

// DefaultDetectTTL is how long a detection result is reused. Detection spawns
// the CLI, and a full run would otherwise pay for three of those on top of
// the one the daemon already did for its hello frame.
const DefaultDetectTTL = time.Minute

// Provider is the Claude Code implementation of [agent.Provider].
type Provider struct {
	opts Options
	host agent.Host

	mu         sync.Mutex
	cached     agent.Capability
	cachedAt   time.Time
	cachedOnce bool
}

// New builds the provider. It touches nothing until Detect or Run is called,
// so constructing one on a machine without the CLI is not an error.
func New(opts Options) *Provider {
	host := agent.RealHost()
	if opts.Host != nil {
		host = *opts.Host
	}
	return &Provider{opts: opts, host: host}
}

// Name identifies the CLI.
func (p *Provider) Name() qaschema.AgentCapabilityName { return Name }

// Detect reports whether this machine has Claude Code and whether the operator
// has logged it in.
func (p *Provider) Detect(ctx context.Context) (agent.Capability, error) {
	ttl := p.opts.DetectTTL
	if ttl == 0 {
		ttl = DefaultDetectTTL
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedOnce && ttl > 0 && time.Since(p.cachedAt) < ttl {
		return p.cached, nil
	}

	cli, ok := agent.KnownCLI(Name)
	if !ok {
		return agent.Capability{}, fmt.Errorf("claude: %q is not a known CLI", Name)
	}
	if p.opts.Binary != "" {
		cli.Binary = p.opts.Binary
	}

	opts := p.opts.DetectOptions
	if opts.Host == nil {
		opts.Host = &p.host
	}
	opts.Only = []qaschema.AgentCapabilityName{Name}

	capability := agent.DetectOne(ctx, cli, opts)
	p.cached, p.cachedAt, p.cachedOnce = capability, time.Now(), true
	return capability, nil
}

// Run performs one invocation of the CLI against the prompt already written in
// the task's workspace.
func (p *Provider) Run(ctx context.Context, t agent.Task) (agent.Result, error) {
	capability, err := p.Detect(ctx)
	if err != nil {
		return agent.Result{Status: agent.StatusUnavailable, Attempts: 1, Detail: err.Error()},
			fmt.Errorf("claude: detect: %w", err)
	}
	if !capability.Usable() {
		return agent.Result{
				Status: agent.StatusUnavailable, Attempts: 1, Provider: Name, Detail: capability.Detail,
			},
			fmt.Errorf("claude: %w: %s", agent.ErrNotDetected, capability.Detail)
	}

	ws, err := security.OpenWorkspace(t.WorkspaceDir)
	if err != nil {
		return agent.Result{Status: agent.StatusError, Attempts: 1, Provider: Name, Detail: err.Error()},
			fmt.Errorf("claude: open workspace: %w", err)
	}
	defer func() { _ = ws.Close() }()

	prompt, err := ws.ReadFile(promptName(t))
	if err != nil {
		return agent.Result{Status: agent.StatusError, Attempts: 1, Provider: Name, Detail: err.Error()},
			fmt.Errorf("claude: read %s: %w", promptName(t), err)
	}

	args := p.args()
	launch := agent.Launch{
		Binary:  capability.Path,
		Args:    args,
		Stdin:   prompt,
		Stdout:  t.Stdout,
		Stderr:  t.Stderr,
		Sandbox: p.sandbox(t.Sandbox, capability.Path),
		Timeout: t.Timeout,
	}

	outcome, runErr := agent.Run(ctx, launch)
	result := agent.Result{
		Attempts: 1,
		Provider: Name,
		ExitCode: outcome.ExitCode,
		Duration: outcome.Duration,
		Command:  recordedCommand(args),
	}

	switch {
	case outcome.TimedOut:
		result.Status = agent.StatusTimeout
		result.Detail = fmt.Sprintf("claude did not finish within %s and its process tree was killed", t.Timeout)
		return result, nil
	case runErr != nil:
		result.Status = agent.StatusError
		result.Detail = runErr.Error()
		return result, fmt.Errorf("claude: run: %w", runErr)
	}

	output, err := ws.ReadFile(outputName(t))
	if err != nil || len(strings.TrimSpace(string(output))) == 0 {
		// The CLI ran and wrote nothing usable. That is a bad answer rather
		// than a broken machine, so the runner is allowed to try again with
		// the reason attached.
		result.Status = agent.StatusOutputInvalid
		result.Detail = fmt.Sprintf("claude exited %d without writing %s", outcome.ExitCode, outputName(t))
		return result, nil
	}

	result.Output = output
	result.Status = agent.StatusOK
	if outcome.ExitCode != 0 {
		// Some versions exit non-zero after a perfectly good answer. The file
		// is what decides; the exit code is recorded so a human can see it.
		result.Detail = fmt.Sprintf("claude exited %d but wrote %s", outcome.ExitCode, outputName(t))
	}
	return result, nil
}

// recordedCommand renders argv for the attempt record. The system prompt is a
// couple of kilobytes of standing rules that are identical on every
// invocation; printing it in full would bury the flags that actually differ.
func recordedCommand(args []string) string {
	out := make([]string, 0, len(args)+1)
	out = append(out, "claude")
	for i := 0; i < len(args); i++ {
		out = append(out, args[i])
		if args[i] == "--append-system-prompt" && i+1 < len(args) {
			out = append(out, fmt.Sprintf("<system prompt, %d bytes>", len(args[i+1])))
			i++
		}
	}
	return strings.Join(out, " ")
}

func (p *Provider) args() []string {
	args := append([]string(nil), baseArgs...)
	if p.opts.Model != "" {
		args = append(args, "--model", p.opts.Model)
	}
	// The standing rules go in as a system prompt rather than at the top of
	// the task: a CLI that compacts a long conversation drops the oldest user
	// turn first, and the untrusted-content rule is the last thing that may
	// fall out of context.
	args = append(args, "--append-system-prompt", prompts.System())
	return append(args, p.opts.ExtraArgs...)
}

// sandbox extends the runner's spec with what this CLI needs and nothing else.
func (p *Provider) sandbox(base security.Spec, binary string) security.Spec {
	spec := base

	readOnly := spec.ReadOnlyPaths
	if readOnly == nil {
		readOnly = security.DefaultReadOnlyPaths()
	}
	readOnly = append(readOnly, existing(
		p.configDir(),
		p.host.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
	)...)
	readOnly = append(readOnly, binaryPaths(binary)...)
	spec.ReadOnlyPaths = readOnly

	envAllow := spec.EnvAllow
	if len(envAllow) == 0 {
		envAllow = security.BaseEnvAllow()
	}
	spec.EnvAllow = append(append([]string(nil), envAllow...), credentialEnv...)

	envSet := map[string]string{}
	for k, v := range spec.EnvSet {
		envSet[k] = v
	}
	// $HOME is the workspace, so the CLI would otherwise look for its
	// credentials in a directory created seconds ago and conclude it has
	// never been logged in.
	if dir := p.configDir(); dir != "" {
		envSet["CLAUDE_CONFIG_DIR"] = dir
	}
	spec.EnvSet = envSet
	return spec
}

// configDir is where the operator's credentials live on the host.
func (p *Provider) configDir() string {
	if p.opts.ConfigDir != "" {
		return p.opts.ConfigDir
	}
	return agent.ClaudeConfigDir(p.host)
}

// binaryPaths is every directory the sandbox has to make executable for this
// CLI to start.
//
// The resolved target matters as much as the name on PATH: Landlock rules
// follow the real path, and every current installer puts a symlink in
// ~/.local/bin or /usr/local/bin pointing at a versioned directory somewhere
// else. Granting only the link's directory produces `permission denied` at
// exec with nothing to suggest which path was missing.
func binaryPaths(binary string) []string {
	paths := []string{filepath.Dir(binary)}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil && resolved != binary {
		paths = append(paths, resolved, filepath.Dir(resolved))
	}
	return existing(paths...)
}

// existing filters out empty and absent paths: naming a path the sandbox
// cannot find is an error on some hosts, and an empty one always is.
func existing(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || p == "." {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

func promptName(t agent.Task) string {
	if t.PromptFile != "" {
		return t.PromptFile
	}
	return agent.DefaultPromptFile
}

func outputName(t agent.Task) string {
	if t.OutputFile != "" {
		return t.OutputFile
	}
	return agent.DefaultOutputFile
}
