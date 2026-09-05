package analysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The fixtures every test in this package builds on. They are helpers rather
// than shared package-level values because several tests mutate what they are
// given, and a shared execution result would make the order of the tests
// matter.

func ptr[T any](v T) *T { return &v }

// failedExecution is a case that failed on its second step, with a screenshot.
func failedExecution(ref string, opts ...func(*qaschema.ExecutionResult)) qaschema.ExecutionResult {
	execution := qaschema.ExecutionResult{
		Version:    1,
		TestCaseID: ref,
		Result:     qaschema.OutcomeFail,
		Message:    ptr("the employee did not appear in the list"),
		Steps: []qaschema.StepResult{
			{Index: 0, Action: qaschema.StepActionNavigate, Status: qaschema.OutcomePass},
			{Index: 1, Action: qaschema.StepActionClick, Status: qaschema.OutcomeFail,
				Message: ptr("clicked, and the list did not change")},
		},
		Artifacts: []qaschema.Artifact{
			{ID: "e0-screenshot-0", Kind: qaschema.ArtifactKindScreenshot, Key: keyFor(ref, "screenshot-0.png")},
		},
		StartedAt: "2026-09-05T10:00:00Z",
		EndedAt:   "2026-09-05T10:00:04Z",
	}
	for _, opt := range opts {
		opt(&execution)
	}
	return execution
}

func withLogs(ref string) func(*qaschema.ExecutionResult) {
	return func(e *qaschema.ExecutionResult) {
		e.Artifacts = append(e.Artifacts,
			qaschema.Artifact{ID: "e0-network-1", Kind: qaschema.ArtifactKindNetwork, Key: keyFor(ref, "network-1.json")},
			qaschema.Artifact{ID: "e0-console-2", Kind: qaschema.ArtifactKindConsole, Key: keyFor(ref, "console-2.json")},
		)
	}
}

func keyFor(ref, name string) string {
	return "orgs/org1/runs/2026-09-05/run1/" + ref + "/" + name
}

func testCase(ref string) qaschema.TestCase {
	return qaschema.TestCase{
		Version: 1, ID: ref, Name: "Create employee",
		Priority: qaschema.TestCasePriorityHigh,
		Category: qaschema.TestCaseCategoryFunctional,
		Steps: []qaschema.Step{
			{Action: "navigate", URL: ptr("/employees")},
			{Action: "click", Target: &qaschema.Target{Ref: ptr("emp.btn.add")}},
		},
		Assertions: []qaschema.Assertion{{Type: qaschema.AssertionTypeVisible,
			Target: &qaschema.Target{Ref: ptr("emp.row.first")}}},
	}
}

// writeLogs puts the network and console artifacts on disk where the collector
// looks for them, and returns the directory.
func writeLogs(t *testing.T, ref string, network []NetworkEntry, console []ConsoleEntry) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(name string, value any) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("network-1.json", network)
	write("console-2.json", console)
	return dir
}

// bundleFor is a ready-made bundle with one citable artifact, for the tests
// that are about rules or the gate rather than about collection.
func bundleFor(ref string, opts ...func(*Bundle)) Bundle {
	b := Bundle{
		TestCaseRef: ref,
		Execution:   failedExecution(ref),
		TestCase:    ptr(testCase(ref)),
		Artifacts: []qaschema.Artifact{
			{ID: "e0-screenshot-0", Kind: qaschema.ArtifactKindScreenshot, Key: keyFor(ref, "screenshot-0.png")},
		},
	}
	b.FailedStep = &b.Execution.Steps[1]
	b.PrecedingStep = &b.Execution.Steps[0]
	for _, opt := range opts {
		opt(&b)
	}
	return b
}

func status(code int) *int { return &code }

// decodeFinding reads a finding document back for assertions.
func decodeFinding(t *testing.T, document json.RawMessage) qaschema.Finding {
	t.Helper()
	var finding qaschema.Finding
	if err := json.Unmarshal(document, &finding); err != nil {
		t.Fatalf("decode finding: %v", err)
	}
	return finding
}

// rules renders the rules that fired, for a table-driven assertion.
func rulesOf(rejections []Rejection) []string {
	out := make([]string, len(rejections))
	for i, rejection := range rejections {
		out[i] = rejection.Rule
	}
	return out
}
