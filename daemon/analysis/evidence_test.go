package analysis

import (
	"path/filepath"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// Only failures are collected. An analyst asked to explain a pass will find
// something to say about it, and that something ends up on the report.
func TestCollectIgnoresEverythingThatDidNotFail(t *testing.T) {
	passing := failedExecution("TC-001")
	passing.Result = qaschema.OutcomePass
	skipped := failedExecution("TC-002")
	skipped.Result = qaschema.OutcomeSkipped
	errored := failedExecution("TC-003")
	errored.Result = qaschema.OutcomeError
	failed := failedExecution("TC-004")

	bundles := Collector{}.Collect(
		[]qaschema.ExecutionResult{passing, skipped, errored, failed},
		[]qaschema.TestCase{testCase("TC-004")})

	var refs []string
	for _, b := range bundles {
		refs = append(refs, b.TestCaseRef)
	}
	if len(refs) != 2 || refs[0] != "TC-003" || refs[1] != "TC-004" {
		t.Fatalf("collected %v, want the errored and the failed case only", refs)
	}
}

// The failing step and the one before it, because the cause of a failure is
// usually the step before the one that reported it.
func TestCollectPairsTheFailingStepWithItsPredecessor(t *testing.T) {
	bundles := Collector{}.Collect(
		[]qaschema.ExecutionResult{failedExecution("TC-001")},
		[]qaschema.TestCase{testCase("TC-001")})

	b := bundles[0]
	if b.FailedStep == nil || b.FailedStep.Index != 1 {
		t.Fatalf("failed step = %+v, want index 1", b.FailedStep)
	}
	if b.PrecedingStep == nil || b.PrecedingStep.Index != 0 {
		t.Fatalf("preceding step = %+v, want index 0", b.PrecedingStep)
	}
	if b.TestCase == nil || len(b.TestCase.Steps) != 2 {
		t.Fatalf("the test case as written is missing from the bundle: %+v", b.TestCase)
	}
}

// A case that failed on its very first step has no predecessor, and asking for
// one must not walk off the front of the slice.
func TestCollectHandlesAFailureOnTheFirstStep(t *testing.T) {
	execution := failedExecution("TC-001")
	execution.Steps[0].Status = qaschema.OutcomeFail

	b := Collector{}.Collect([]qaschema.ExecutionResult{execution}, nil)[0]
	if b.FailedStep == nil || b.FailedStep.Index != 0 {
		t.Fatalf("failed step = %+v", b.FailedStep)
	}
	if b.PrecedingStep != nil {
		t.Fatalf("preceding step = %+v, want none", b.PrecedingStep)
	}
}

// The logs are read off disk and filtered: failed requests and error-level
// console lines, not the whole log. A page that logs in a loop would otherwise
// put tens of thousands of lines in front of a model asked about one click.
func TestCollectKeepsOnlyTheInterestingLogLines(t *testing.T) {
	dir := writeLogs(t, "TC-001",
		[]NetworkEntry{
			{Method: "GET", URL: "http://app/employees", Status: status(200)},
			{Method: "POST", URL: "http://app/api/employees", Status: status(500)},
			{Method: "GET", URL: "http://app/api/me", Status: nil},
		},
		[]ConsoleEntry{
			{Level: "log", Text: "rendered"},
			{Level: "error", Text: "Uncaught TypeError: cannot read properties of null"},
			{Level: "warn", Text: "deprecated"},
		})

	collector := Collector{ArtifactDir: func(string) (string, error) { return dir, nil }}
	b := collector.Collect(
		[]qaschema.ExecutionResult{failedExecution("TC-001", withLogs("TC-001"))},
		nil)[0]

	if len(b.Network) != 2 {
		t.Fatalf("network = %+v, want the 500 and the request with no response", b.Network)
	}
	if len(b.Console) != 2 {
		t.Fatalf("console = %+v, want the error and the warning", b.Console)
	}
	for _, entry := range b.Console {
		if entry.Level == "log" {
			t.Fatalf("an info line reached the bundle: %+v", entry)
		}
	}
}

// A log that cannot be read is a warning, not a failure: the analyst still has
// the step results and the screenshots, and losing the whole report because
// one file went missing is the wrong trade.
func TestCollectSurvivesAMissingLogFile(t *testing.T) {
	collector := Collector{ArtifactDir: func(string) (string, error) {
		return filepath.Join(t.TempDir(), "nothing-here"), nil
	}}
	b := collector.Collect(
		[]qaschema.ExecutionResult{failedExecution("TC-001", withLogs("TC-001"))},
		nil)[0]

	if b.Network != nil || b.Console != nil {
		t.Fatalf("unreadable logs produced content: %+v %+v", b.Network, b.Console)
	}
	if len(b.Artifacts) != 3 {
		t.Fatalf("the artifacts are still citable: %+v", b.Artifacts)
	}
}

// Only the elements the failing case targets, not the whole application map.
func TestCollectAttachesOnlyTheTargetedElements(t *testing.T) {
	appMap := &qaschema.ApplicationMap{
		Version: 1, BaseURL: "http://app",
		Pages: []qaschema.Page{{
			ID: "page.employees", Path: "/employees", Title: "Employees",
			Elements: []qaschema.Element{
				{Ref: "emp.btn.add", Type: "button", Label: ptr("Add employee"),
					Locators: []qaschema.Locator{{Kind: qaschema.LocatorKindTestID, Value: "add-employee"}}},
				{Ref: "emp.row.first", Type: "other"},
				{Ref: "emp.btn.export", Type: "button"},
			},
		}},
	}

	collector := Collector{AppMap: appMap}
	b := collector.Collect(
		[]qaschema.ExecutionResult{failedExecution("TC-001")},
		[]qaschema.TestCase{testCase("TC-001")})[0]

	if len(b.Elements) != 2 {
		t.Fatalf("elements = %+v, want the two the case targets", b.Elements)
	}
	if b.Elements[0].Ref != "emp.btn.add" || b.Elements[0].PagePath != "/employees" {
		t.Fatalf("element = %+v, want the ref flattened with its page", b.Elements[0])
	}
	if len(b.Elements[0].Locators) != 1 {
		t.Fatalf("the locator chain is what a TEST_BUG verdict rests on: %+v", b.Elements[0])
	}
}

// A previous run's outcome is the difference between "broken since Tuesday"
// and "broken by what you just deployed".
func TestCollectCarriesThePreviousOutcome(t *testing.T) {
	collector := Collector{Previous: map[string]PriorOutcome{
		"TC-001": {RunID: "run-0", Result: qaschema.OutcomePass},
	}}
	b := collector.Collect([]qaschema.ExecutionResult{failedExecution("TC-001")}, nil)[0]

	if b.Previous == nil || b.Previous.Result != qaschema.OutcomePass {
		t.Fatalf("previous = %+v, want the passing earlier run", b.Previous)
	}
}

// The bundle encodes to JSON a model can read and a person can open.
func TestBundleEncodes(t *testing.T) {
	data, err := bundleFor("TC-001").Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("the encoded bundle is empty")
	}
}
