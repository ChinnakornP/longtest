// Package antigravity will run the Antigravity CLI as an [agent.Provider].
//
// Detection is real: a runtime reports honestly in its hello frame whether
// this machine has the agy CLI and whether the operator has logged it in, so the
// platform can show it as an option and say why it is not offered. Launching
// is not implemented — see [Provider.Run].
package antigravity

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// Name is the CLI this provider will drive.
const Name = qaschema.AgentCapabilityNameOpencode

// Options configure the provider.
type Options struct {
	// Binary overrides PATH lookup. Empty means detection resolves `agy`.
	Binary string
	// Host is the machine detection reads; the zero value reads this one.
	Host *agent.Host
	// DetectTTL caches the detection result. Zero means one minute.
	DetectTTL time.Duration
	// DetectOptions is passed through to detection, for tests.
	DetectOptions agent.DetectOptions
}

// Provider detects Antigravity and refuses to run it.
type Provider struct {
	opts Options
	host agent.Host

	mu       sync.Mutex
	cached   agent.Capability
	cachedAt time.Time
	once     bool
}

// New builds the provider.
func New(opts Options) *Provider {
	host := agent.RealHost()
	if opts.Host != nil {
		host = *opts.Host
	}
	return &Provider{opts: opts, host: host}
}

// Name identifies the CLI.
func (p *Provider) Name() qaschema.AgentCapabilityName { return Name }

// Detect reports whether this machine has Antigravity and whether it is logged in.
func (p *Provider) Detect(ctx context.Context) (agent.Capability, error) {
	ttl := p.opts.DetectTTL
	if ttl == 0 {
		ttl = time.Minute
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.once && time.Since(p.cachedAt) < ttl {
		return p.cached, nil
	}

	cli, ok := agent.KnownCLI(Name)
	if !ok {
		return agent.Capability{}, fmt.Errorf("antigravity: %q is not a known CLI", Name)
	}
	if p.opts.Binary != "" {
		cli.Binary = p.opts.Binary
	}
	opts := p.opts.DetectOptions
	if opts.Host == nil {
		opts.Host = &p.host
	}
	opts.Only = []qaschema.AgentCapabilityName{Name}

	p.cached, p.cachedAt, p.once = agent.DetectOne(ctx, cli, opts), time.Now(), true
	return p.cached, nil
}

// Run reports that this provider cannot execute a phase yet.
//
// TODO(T10-followup): implement the file exchange. What is missing is only the
// invocation: the CLI ships as `agy`, and it needs its own headless flag set,
// its credential directory (~/.antigravity) mounted read-only into the
// sandbox, and its own entry in a credentialEnv list, exactly as
// claude.Provider does. Everything
// else — prompt rendering, validation, retries, the attempt record — is the
// runner's and needs no change here. Until then a run that asks for antigravity
// fails at assignment with agent_not_available rather than part-way through a
// phase, which is the difference between "this runtime cannot do it" and "your
// test run broke".
func (p *Provider) Run(_ context.Context, _ agent.Task) (agent.Result, error) {
	const detail = "the Antigravity provider is not implemented yet; this runtime can detect the CLI but not drive it"
	return agent.Result{
			Status: agent.StatusUnavailable, Attempts: 0, Provider: Name, Detail: detail,
		},
		fmt.Errorf("antigravity: %w: %s", agent.ErrNotDetected, detail)
}
