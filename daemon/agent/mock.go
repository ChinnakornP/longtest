package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// MockProvider answers from files instead of from a model.
//
// It exists so the rest of the daemon is testable. Every integration test
// downstream of this package — the run controller, the executor handoff, the
// cancel path — needs an agent that produces a plan, and none of them needs a
// language model to do it: an LLM in a test suite is slow, costs money per
// run, and is the one component that can answer differently to the same input,
// which is the opposite of what a regression test is for.
//
// The answers are files rather than Go literals on purpose. A fixture that
// lives on disk can be shared with the TypeScript side, checked against the
// contracts by the same fixture suite that checks every other document, and
// edited by someone debugging a failure without recompiling anything.
type MockProvider struct {
	opts MockOptions

	mu    sync.Mutex
	calls []MockCall
}

// MockOptions configure the provider.
type MockOptions struct {
	// As is the CLI this mock impersonates. Empty means claude, so a run that
	// asks for the default agent finds it.
	As qaschema.AgentCapabilityName

	// Dir holds the canned answers. For a phase p and attempt n the provider
	// reads the first of these that exists:
	//
	//	{Dir}/{p}.attempt-{n}.json    a different answer per attempt
	//	{Dir}/{p}.json                the same answer every time
	//
	// A sibling file {p}.attempt-{n}.status, or {p}.status, holds a [Status]
	// word — that is how a test asks for a timeout or an unavailable CLI
	// without one actually happening.
	Dir string

	// Answers overrides Dir for a test that would rather not touch the disk.
	// The slice is indexed by attempt; the last entry is reused once the
	// attempts run past it.
	Answers map[prompts.Phase][]MockAnswer

	// Capability is what Detect reports. The zero value is ready.
	Capability *Capability

	// Delay is slept before each answer, for a test that needs a phase to
	// still be running when it cancels the run.
	Delay time.Duration
}

// MockAnswer is one scripted reply.
type MockAnswer struct {
	// Output is written to the task's output file and returned.
	Output []byte
	// Status is the outcome. Empty means StatusOK.
	Status Status
	// Detail explains a non-ok status.
	Detail string
}

// MockCall records one invocation, for assertions.
type MockCall struct {
	Phase   prompts.Phase
	Attempt int
	// Prompt is exactly what was on disk when the CLI would have read it.
	// Tests assert against this: it is the last point at which a credential
	// could still leak into a third party's context window.
	Prompt string
	Dir    string
}

// NewMockProvider builds a mock.
func NewMockProvider(opts MockOptions) *MockProvider {
	if opts.As == "" {
		opts.As = qaschema.AgentCapabilityNameClaude
	}
	return &MockProvider{opts: opts}
}

// Name is the CLI this mock stands in for.
func (m *MockProvider) Name() qaschema.AgentCapabilityName { return m.opts.As }

// Detect reports the configured capability, ready by default.
func (m *MockProvider) Detect(context.Context) (Capability, error) {
	if m.opts.Capability != nil {
		return *m.opts.Capability, nil
	}
	return Capability{
		Name: m.opts.As, Readiness: ReadinessReady,
		Version: "mock", Path: "/nonexistent/" + string(m.opts.As),
	}, nil
}

// Calls returns every invocation so far.
func (m *MockProvider) Calls() []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MockCall(nil), m.calls...)
}

// Run writes the canned answer into the workspace and returns it.
//
// It goes through the file exchange rather than short-circuiting it: a mock
// that returned bytes directly would let a bug in "read the file the CLI
// wrote" survive every test in the repository.
func (m *MockProvider) Run(ctx context.Context, t Task) (Result, error) {
	m.mu.Lock()
	attempt := 0
	for _, call := range m.calls {
		if call.Phase == t.Phase {
			attempt++
		}
	}
	attempt++
	m.mu.Unlock()

	ws, err := security.OpenWorkspace(t.WorkspaceDir)
	if err != nil {
		return Result{Status: StatusError, Attempts: 1, Provider: m.opts.As, Detail: err.Error()},
			fmt.Errorf("mock: open workspace: %w", err)
	}
	defer func() { _ = ws.Close() }()

	prompt, err := ws.ReadFile(t.promptName())
	if err != nil {
		return Result{Status: StatusError, Attempts: 1, Provider: m.opts.As, Detail: err.Error()},
			fmt.Errorf("mock: the runner wrote no %s: %w", t.promptName(), err)
	}

	m.mu.Lock()
	m.calls = append(m.calls, MockCall{
		Phase: t.Phase, Attempt: attempt, Prompt: string(prompt), Dir: t.WorkspaceDir,
	})
	m.mu.Unlock()

	if m.opts.Delay > 0 {
		select {
		case <-time.After(m.opts.Delay):
		case <-ctx.Done():
			return Result{Status: StatusError, Attempts: 1, Provider: m.opts.As, Detail: ctx.Err().Error()}, ctx.Err()
		}
	}

	answer, err := m.answer(t.Phase, attempt)
	if err != nil {
		return Result{Status: StatusError, Attempts: 1, Provider: m.opts.As, Detail: err.Error()}, err
	}

	result := Result{
		Attempts: 1, Provider: m.opts.As, Detail: answer.Detail,
		Command: fmt.Sprintf("mock(%s) %s attempt %d", m.opts.As, t.Phase, attempt),
	}
	result.Status = answer.Status
	if result.Status == "" {
		result.Status = StatusOK
	}
	if result.Status != StatusOK && result.Status != StatusOutputInvalid {
		return result, nil
	}

	if err := ws.WriteFile(t.outputName(), answer.Output); err != nil {
		return Result{Status: StatusError, Attempts: 1, Provider: m.opts.As, Detail: err.Error()},
			fmt.Errorf("mock: write %s: %w", t.outputName(), err)
	}
	written, err := ws.ReadFile(t.outputName())
	if err != nil {
		return Result{Status: StatusError, Attempts: 1, Provider: m.opts.As, Detail: err.Error()},
			fmt.Errorf("mock: read back %s: %w", t.outputName(), err)
	}
	result.Output = written
	return result, nil
}

func (m *MockProvider) answer(phase prompts.Phase, attempt int) (MockAnswer, error) {
	if scripted, ok := m.opts.Answers[phase]; ok && len(scripted) > 0 {
		if attempt > len(scripted) {
			attempt = len(scripted)
		}
		return scripted[attempt-1], nil
	}
	if m.opts.Dir == "" {
		return MockAnswer{}, fmt.Errorf("mock: no answer configured for the %s phase", phase)
	}

	candidates := []string{
		fmt.Sprintf("%s.attempt-%d", phase, attempt),
		string(phase),
	}
	for _, base := range candidates {
		//nolint:gosec // G304: Dir is a fixture directory a test names, not a
		// path from a page or a model.
		data, err := os.ReadFile(filepath.Join(m.opts.Dir, base+".json"))
		switch {
		case err == nil:
			return MockAnswer{Output: data, Status: m.status(base)}, nil
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return MockAnswer{}, fmt.Errorf("mock: read %s: %w", base+".json", err)
		}
	}
	// A status file with no answer file is how a test scripts a timeout: there
	// is no document to write, because the CLI never got that far.
	for _, base := range candidates {
		if status := m.status(base); status != "" {
			return MockAnswer{Status: status, Detail: "scripted by " + base + ".status"}, nil
		}
	}
	return MockAnswer{}, fmt.Errorf("mock: %s holds no answer for the %s phase, attempt %d", m.opts.Dir, phase, attempt)
}

func (m *MockProvider) status(base string) Status {
	//nolint:gosec // G304: see answer.
	data, err := os.ReadFile(filepath.Join(m.opts.Dir, base+".status"))
	if err != nil {
		return ""
	}
	return Status(strings.TrimSpace(string(data)))
}
