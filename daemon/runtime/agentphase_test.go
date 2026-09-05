package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// fakeAgent stands in for the AI CLI provider T10 delivers. It records the
// task it was handed, which is how these tests check the file-exchange
// contract (ADR-003) without an actual CLI.
type fakeAgent struct {
	mu    sync.Mutex
	tasks []AgentTask

	byPhase map[workspace.Phase]func(AgentTask) ([]byte, error)
}

func (a *fakeAgent) Run(_ context.Context, task AgentTask) ([]byte, error) {
	a.mu.Lock()
	a.tasks = append(a.tasks, task)
	handler := a.byPhase[task.Phase]
	a.mu.Unlock()

	if handler == nil {
		return nil, errors.New("fakeAgent: no handler for " + string(task.Phase))
	}
	return handler(task)
}

func (a *fakeAgent) Tasks() []AgentTask {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AgentTask(nil), a.tasks...)
}

// firstArtifactID reads an id out of an evidence bundle the analysis phase
// placed in the workspace.
//
// A canned analysis has to cite evidence the run really produced — that is what
// analysis.Context checks — and the ids are namespaced per execution, so a test
// cannot hard-code one without encoding the namespacing scheme into every
// fixture that touches it.
func firstArtifactID(t *testing.T, task AgentTask) string {
	t.Helper()

	for name, data := range task.Inputs {
		if !strings.HasPrefix(name, "evidence-") {
			continue
		}
		var bundle struct {
			Artifacts []struct {
				ID string `json:"id"`
			} `json:"artifacts"`
		}
		if err := json.Unmarshal(data, &bundle); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if len(bundle.Artifacts) > 0 {
			return bundle.Artifacts[0].ID
		}
	}
	t.Fatalf("no evidence bundle with an artifact in the analysis inputs: %v", task.Inputs)
	return ""
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

// A full run walks discover → plan → execute → analyze and reports each
// phase's output in one result.
func TestFullModeWalksEveryPhase(t *testing.T) {
	storage := newFakeStorage(t)
	agent := &fakeAgent{byPhase: map[workspace.Phase]func(AgentTask) ([]byte, error){}}

	agent.byPhase[workspace.PhaseDiscovery] = func(task AgentTask) ([]byte, error) {
		return mustJSON(t, testAppMap(task.BaseURL)), nil
	}
	agent.byPhase[workspace.PhasePlanning] = func(AgentTask) ([]byte, error) {
		return mustJSON(t, map[string]any{
			"version":       1,
			"testCases":     []any{testCase("TC-001", "Create employee")},
			"rationale":     "the employees page is the only place data is written",
			"coverageNotes": "settings is not covered",
		}), nil
	}
	agent.byPhase[workspace.PhaseAnalysis] = func(task AgentTask) ([]byte, error) {
		return mustJSON(t, []any{map[string]any{
			"version":      1,
			"testCaseId":   "TC-001",
			"stepIndex":    nil,
			"failureClass": "PRODUCT_BUG",
			"rootCause":    "POST /api/employees returned 500",
			"confidence":   0.94,
			// Read back from the evidence bundle rather than hard-coded: the
			// ids are namespaced per execution before the analyst sees them,
			// and the review gate rejects a citation that names nothing.
			"evidence": []any{firstArtifactID(t, task)},
		}}), nil
	}

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		// Failing, because only a failure is analysed.
		return failWith(t, params, "the save button did nothing", map[string]string{"screenshot-1.png": "png"})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode:    qaschema.RunAssignPayloadModeFull,
		putBase: storage.PutBase(),
	}))

	result := h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", payload.Status, payload.Error)
	}
	if payload.AppMap == nil {
		t.Fatal("run.result carries no application map")
	}
	if payload.TestPlan == nil || len(payload.TestPlan.TestCases) != 1 {
		t.Fatalf("test plan = %+v", payload.TestPlan)
	}
	if len(payload.Executions) != 1 {
		t.Fatalf("executions = %d; the plan's cases should have been executed", len(payload.Executions))
	}
	if len(payload.Findings) != 1 || payload.Findings[0].FailureClass != qaschema.FailureClassPRODUCTBUG {
		t.Fatalf("findings = %+v", payload.Findings)
	}
	// The analyst's document reaches the backend exactly as it was validated:
	// a null stepIndex is required by finding@1 and must survive the trip.
	rawResult := decodeAs[struct {
		Findings []map[string]any `json:"findings"`
	}](t, result.Payload)
	if _, ok := rawResult.Findings[0]["stepIndex"]; !ok {
		t.Fatalf("stepIndex was dropped in transit: %+v", rawResult.Findings[0])
	}
	// The execution's screenshot, plus the evidence bundle the analysis phase
	// uploaded so the finding cites something a person can open.
	if len(payload.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want the screenshot and the analysis evidence bundle", len(payload.Artifacts))
	}

	// Each phase gets its own directory, and the planner is handed the map as
	// a file rather than as prompt text.
	tasks := agent.Tasks()
	if len(tasks) != 3 {
		t.Fatalf("agent ran %d times, want discovery, planning and analysis", len(tasks))
	}
	dirs := map[string]bool{}
	for _, task := range tasks {
		if !strings.HasSuffix(task.Dir, string(task.Phase)) {
			t.Fatalf("%s phase ran in %s", task.Phase, task.Dir)
		}
		if _, err := os.Stat(task.Dir); err != nil {
			t.Fatalf("phase directory missing: %v", err)
		}
		dirs[task.Dir] = true
	}
	if len(dirs) != 3 {
		t.Fatalf("phases shared a directory: %v", dirs)
	}
	if _, ok := tasks[1].Inputs["application-map.json"]; !ok {
		t.Fatalf("the planner was not handed the application map as a file: %v", tasks[1].Inputs)
	}
	if tasks[0].SchemaID != "application-map@1" || tasks[1].SchemaID != "test-plan@1" || tasks[2].SchemaID != "finding@1" {
		t.Fatalf("phases asked for the wrong contracts: %q %q %q",
			tasks[0].SchemaID, tasks[1].SchemaID, tasks[2].SchemaID)
	}
}

// Output that does not match its contract fails the run with the contract's
// own code, rather than being forwarded and breaking something downstream.
func TestInvalidAgentOutputIsRejected(t *testing.T) {
	agent := &fakeAgent{byPhase: map[workspace.Phase]func(AgentTask) ([]byte, error){
		workspace.PhaseDiscovery: func(AgentTask) ([]byte, error) {
			// Plausible, and missing the required pages/workflows arrays.
			return []byte(`{"version":1,"baseUrl":"http://app.internal:3000"}`), nil
		},
	}}

	h := newHarness(t, harnessOptions{agent: agent})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{mode: qaschema.RunAssignPayloadModeDiscover}))

	result := h.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q", payload.Status)
	}
	if payload.Error == nil || payload.Error.Code != qaschema.RunErrorCodeAgentOutputInvalid {
		t.Fatalf("error = %+v, want agent_output_invalid", payload.Error)
	}
	if !strings.Contains(payload.Error.Message, "application-map@1") {
		t.Fatalf("the error should name the contract it failed: %q", payload.Error.Message)
	}
}

func TestAnalystArrayIsValidatedElementByElement(t *testing.T) {
	storage := newFakeStorage(t)
	agent := &fakeAgent{byPhase: map[workspace.Phase]func(AgentTask) ([]byte, error){
		workspace.PhaseAnalysis: func(AgentTask) ([]byte, error) {
			// The second finding has no evidence, which finding@1 forbids: a
			// finding with no evidence is a guess.
			return []byte(`[
			  {"version":1,"testCaseId":"TC-001","stepIndex":0,"failureClass":"PRODUCT_BUG",
			   "rootCause":"500 from the API","confidence":0.9,"evidence":["art-1"]},
			  {"version":1,"testCaseId":"TC-002","stepIndex":0,"failureClass":"UNKNOWN",
			   "rootCause":"not sure","confidence":0.2,"evidence":[]}
			]`), nil
		},
	}}

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failWith(t, params, "the save button did nothing", map[string]string{"screenshot-1.png": "png"})
	}
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	// analyze only runs in full mode, so the earlier phases are stubbed out.
	agent.byPhase[workspace.PhaseDiscovery] = func(task AgentTask) ([]byte, error) {
		return mustJSON(t, testAppMap(task.BaseURL)), nil
	}
	agent.byPhase[workspace.PhasePlanning] = func(AgentTask) ([]byte, error) {
		return mustJSON(t, map[string]any{
			"version": 1, "testCases": []any{testCase("TC-001", "Create employee")},
			"rationale": "r", "coverageNotes": "c",
		}), nil
	}

	h.backend.Send(assignFrame(t, assignOptions{
		mode:    qaschema.RunAssignPayloadModeFull,
		putBase: storage.PutBase(),
	}))

	result := h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q, want the invalid finding to fail the phase", payload.Status)
	}
	if payload.Error == nil || payload.Error.Code != qaschema.RunErrorCodeAgentOutputInvalid {
		t.Fatalf("error = %+v", payload.Error)
	}
	if !strings.Contains(payload.Error.Message, "item 1") {
		t.Fatalf("the error should say which element failed: %q", payload.Error.Message)
	}
	// The work done before the analyst still reaches the backend.
	if len(payload.Executions) != 1 {
		t.Fatalf("a failed analysis threw away the executions: %+v", payload.Executions)
	}
	// And so does a finding for the failure the analyst could not classify. A
	// failed execution with nothing attached reads like a failure nobody
	// thought worth explaining, which is the outcome this phase exists to
	// prevent — a failed analysis is not a licence to produce it.
	if len(payload.Findings) != 1 || payload.Findings[0].FailureClass != qaschema.FailureClassUNKNOWN {
		t.Fatalf("findings = %+v, want one UNKNOWN for the unclassified failure", payload.Findings)
	}
}

func TestAgentInputsLandInTheWorkspace(t *testing.T) {
	var seen string
	agent := &fakeAgent{byPhase: map[workspace.Phase]func(AgentTask) ([]byte, error){
		workspace.PhaseDiscovery: func(task AgentTask) ([]byte, error) {
			seen = task.Dir
			return mustJSON(t, testAppMap(task.BaseURL)), nil
		},
	}}

	root := t.TempDir()
	h := newHarness(t, harnessOptions{agent: agent, workspaceRoot: root})
	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)

	assign := assignFrame(t, assignOptions{mode: qaschema.RunAssignPayloadModeDiscover})
	h.backend.Send(assign)
	h.backend.ExpectType(15*time.Second, qaschema.EnvelopeTypeRunResult)

	payload := decodeAs[qaschema.RunAssignPayload](t, assign.Payload)
	want := filepath.Join(root, payload.ProjectID, payload.RunID, string(workspace.PhaseDiscovery))
	if seen != want {
		t.Fatalf("agent ran in %q, want the run's own discovery directory %q", seen, want)
	}
}
