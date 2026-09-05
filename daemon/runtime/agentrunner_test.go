package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// mockAgentRunner is the whole AI layer as a daemon test gets to use it: the
// real runner, the real prompt rendering, the real validation and retry loop,
// with canned answers where the model would be.
func mockAgentRunner(t *testing.T, mock *agent.MockProvider) AgentRunner {
	t.Helper()
	runner, err := agent.NewRunner(agent.RunnerOptions{
		Registry: agent.NewRegistry(mock),
		Sandbox:  security.Spec{AllowUnsandboxed: true},
	})
	if err != nil {
		t.Fatalf("new agent runner: %v", err)
	}
	return NewAgentRunner(runner)
}

func fixtureMock(t *testing.T) *agent.MockProvider {
	t.Helper()
	return agent.NewMockProvider(agent.MockOptions{
		Dir: filepath.Join("..", "agent", "testdata", "mock"),
	})
}

// The point of MockProvider: a full run — discover, plan, execute, analyse —
// with no language model anywhere in it, and no part of the AI layer stubbed
// out except the answers themselves.
func TestAFullRunWorksWithTheMockProvider(t *testing.T) {
	storage := newFakeStorage(t)
	mock := fixtureMock(t)

	h := newHarness(t, harnessOptions{agent: mockAgentRunner(t, mock)})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return writeEvidence(t, params, map[string]string{"screenshot-1.png": "png"})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode:    qaschema.RunAssignPayloadModeFull,
		putBase: storage.PutBase(),
	}))

	result := h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", payload.Status, payload.Error)
	}
	if payload.AppMap == nil || len(payload.AppMap.Pages) == 0 {
		t.Fatalf("app map = %+v", payload.AppMap)
	}
	if payload.TestPlan == nil || len(payload.TestPlan.TestCases) != 1 {
		t.Fatalf("test plan = %+v", payload.TestPlan)
	}
	if len(payload.Findings) != 1 {
		t.Fatalf("findings = %+v", payload.Findings)
	}

	// Each phase that needs a model got one, in its own directory, with the
	// prompt on disk where a human debugging the run can read it.
	calls := mock.Calls()
	if len(calls) != 3 {
		t.Fatalf("the CLI ran %d times, want discovery, planning and analysis", len(calls))
	}
	for _, call := range calls {
		if !strings.HasSuffix(call.Dir, string(call.Phase)) {
			t.Fatalf("the %s phase ran in %s", call.Phase, call.Dir)
		}
		if !strings.Contains(call.Prompt, "UNTRUSTED_PAGE_CONTENT") &&
			!strings.Contains(call.Prompt, "no page content was captured") {
			t.Fatalf("the %s prompt has no untrusted-content section:\n%s", call.Phase, call.Prompt)
		}
	}
}

// A model that will not produce a valid document fails the run with the
// contract's own error code, not with a generic one — that code is what the UI
// switches on to tell an operator this was the agent and not their application.
func TestAnInvalidPlanFailsTheRunWithTheContractCode(t *testing.T) {
	mock := agent.NewMockProvider(agent.MockOptions{
		Answers: map[prompts.Phase][]agent.MockAnswer{
			prompts.PhaseDiscovery: {{Output: mustFixture(t, "discovery.json")}},
			prompts.PhasePlanning:  {{Output: []byte(`{"version": 1}`)}},
		},
	})

	h := newHarness(t, harnessOptions{agent: mockAgentRunner(t, mock)})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{mode: qaschema.RunAssignPayloadModeFull}))

	result := h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q", payload.Status)
	}
	if payload.Error == nil || payload.Error.Code != qaschema.RunErrorCodeAgentOutputInvalid {
		t.Fatalf("error = %+v", payload.Error)
	}
	if calls := mock.Calls(); len(calls) != 1+agent.DefaultMaxAttempts {
		t.Fatalf("the planner was invoked %d times, want one discovery plus %d planning attempts",
			len(calls), agent.DefaultMaxAttempts)
	}
}

// A runtime with no usable CLI is a different failure from a bad answer, and
// an operator needs to be told which one they have.
func TestNoUsableAgentFailsWithAgentNotAvailable(t *testing.T) {
	mock := agent.NewMockProvider(agent.MockOptions{
		Capability: &agent.Capability{
			Readiness: agent.ReadinessUnauthenticated,
			Detail:    "claude is installed but no credential was found",
		},
	})

	h := newHarness(t, harnessOptions{agent: mockAgentRunner(t, mock)})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{mode: qaschema.RunAssignPayloadModeDiscover}))

	result := h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Error == nil || payload.Error.Code != qaschema.RunErrorCodeAgentNotAvailable {
		t.Fatalf("error = %+v", payload.Error)
	}
	if !strings.Contains(payload.Error.Message, "no credential") {
		t.Fatalf("the operator is not told what to fix: %q", payload.Error.Message)
	}
}

// The retry loop is narrated to the UI as it happens, rather than only showing
// up as a slow phase followed by a failure.
func TestAgentAttemptsAreNarratedAsRunEvents(t *testing.T) {
	mock := agent.NewMockProvider(agent.MockOptions{
		Answers: map[prompts.Phase][]agent.MockAnswer{
			prompts.PhaseDiscovery: {
				{Output: []byte(`{"version": 1}`)},
				{Output: mustFixture(t, "discovery.json")},
			},
		},
	})

	h := newHarness(t, harnessOptions{agent: mockAgentRunner(t, mock)})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{mode: qaschema.RunAssignPayloadModeDiscover}))

	seen := map[string]bool{}
	for {
		env := h.backend.Expect(30*time.Second, func(e qaschema.Envelope) bool {
			return e.Type == qaschema.EnvelopeTypeRunEvent || e.Type == qaschema.EnvelopeTypeRunResult
		})
		if env.Type == qaschema.EnvelopeTypeRunResult {
			break
		}
		payload := decodeAs[qaschema.RunEventPayload](t, env.Payload)
		seen[payload.Code] = true
	}

	for _, code := range []string{
		string(agent.EventAttemptStarted),
		string(agent.EventOutputInvalid),
		string(agent.EventFinished),
	} {
		if !seen[code] {
			t.Fatalf("the UI was never told about %q; it saw %v", code, seen)
		}
	}
}

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "mock", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}
