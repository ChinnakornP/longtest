package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// Registry is the set of AI CLIs this daemon was built with.
//
// It is a registry rather than a switch on a name so that a build can omit a
// provider entirely — an operator who never installs Antigravity should not
// have a code path for it — and so tests can register a MockProvider under a
// real CLI's name without touching any production wiring.
type Registry struct {
	mu        sync.RWMutex
	providers map[qaschema.AgentCapabilityName]Provider
	order     []qaschema.AgentCapabilityName
}

// NewRegistry builds a registry. Registration order is preserved, and is what
// decides the fallback when a run names no agent.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{providers: map[qaschema.AgentCapabilityName]Provider{}}
	for _, p := range providers {
		r.Register(p)
	}
	return r
}

// Register adds a provider, replacing any earlier one with the same name.
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name := p.Name()
	if _, exists := r.providers[name]; !exists {
		r.order = append(r.order, name)
	}
	r.providers[name] = p
}

// Get returns one provider by name.
func (r *Registry) Get(name qaschema.AgentCapabilityName) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Names lists the registered providers in registration order.
func (r *Registry) Names() []qaschema.AgentCapabilityName {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]qaschema.AgentCapabilityName(nil), r.order...)
}

// Detect asks every registered provider about this machine, concurrently.
//
// This is the daemon's single source of truth for the hello frame: a capability
// list built any other way could claim a CLI the provider would then refuse to
// launch.
func (r *Registry) Detect(ctx context.Context) []Capability {
	r.mu.RLock()
	providers := make([]Provider, 0, len(r.order))
	for _, name := range r.order {
		providers = append(providers, r.providers[name])
	}
	r.mu.RUnlock()

	out := make([]Capability, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			capability, err := p.Detect(ctx)
			if err != nil {
				out[i] = Capability{
					Name:      p.Name(),
					Readiness: ReadinessMissing,
					Detail:    fmt.Sprintf("could not be detected: %v", err),
				}
				return
			}
			capability.Name = p.Name()
			out[i] = capability
		}()
	}
	wg.Wait()
	return out
}

// Schema renders a detection result as the hello frame's agents array.
func Schema(caps []Capability) []qaschema.AgentCapability {
	out := make([]qaschema.AgentCapability, len(caps))
	for i, c := range caps {
		out[i] = c.Schema()
	}
	return out
}

// Select picks the provider for a run.
//
// A run that named an agent gets that one or nothing: silently substituting a
// different model would make two runs of the same suite incomparable, which is
// the entire point of recording which agent produced a plan. A run that named
// none gets the first usable provider in registration order, with preferred
// tried first.
func (r *Registry) Select(ctx context.Context, requested, preferred qaschema.AgentCapabilityName) (Provider, Capability, error) {
	if requested != "" {
		p, ok := r.Get(requested)
		if !ok {
			return nil, Capability{}, errorf(StatusUnavailable, nil,
				"this runtime has no provider for %q; it has %s", requested, r.list())
		}
		capability, err := p.Detect(ctx)
		if err != nil {
			return nil, Capability{}, errorf(StatusUnavailable, err, "could not detect %s", requested)
		}
		capability.Name = requested
		if !capability.Usable() {
			return nil, capability, errorf(StatusUnavailable, nil, "%s is not usable on this runtime: %s", requested, capability.Detail)
		}
		return p, capability, nil
	}

	candidates := r.Names()
	if preferred != "" {
		candidates = append([]qaschema.AgentCapabilityName{preferred}, candidates...)
	}

	var reasons []string
	seen := map[qaschema.AgentCapabilityName]bool{}
	for _, name := range candidates {
		if seen[name] {
			continue
		}
		seen[name] = true
		p, ok := r.Get(name)
		if !ok {
			continue
		}
		capability, err := p.Detect(ctx)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		capability.Name = name
		if capability.Usable() {
			return p, capability, nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", name, capability.Detail))
	}

	sort.Strings(reasons)
	return nil, Capability{}, errorf(StatusUnavailable, nil,
		"no AI CLI on this runtime is usable (%s)", strings.Join(reasons, "; "))
}

func (r *Registry) list() string {
	names := r.Names()
	if len(names) == 0 {
		return "none"
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = string(n)
	}
	return strings.Join(parts, ", ")
}
