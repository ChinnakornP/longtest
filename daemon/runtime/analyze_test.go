package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/agent"
	"github.com/ChinnakornP/longtest/daemon/agent/prompts"
	"github.com/ChinnakornP/longtest/daemon/analysis"
	"github.com/ChinnakornP/longtest/daemon/executor"
	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
	"github.com/ChinnakornP/longtest/daemon/workspace"
)

// The analysis phase end to end, through the real run controller: which
// failures reach a model, which never do, and what the daemon guarantees about
// the findings whatever the model does.

// fullRunAgent stubs discovery and planning so a test can be about analysis.
func fullRunAgent(t *testing.T, cases []any, analyse func(AgentTask) ([]byte, error)) *fakeAgent {
	t.Helper()

	agent := &fakeAgent{byPhase: map[workspace.Phase]func(AgentTask) ([]byte, error){}}
	agent.byPhase[workspace.PhaseDiscovery] = func(task AgentTask) ([]byte, error) {
		return mustJSON(t, testAppMap(task.BaseURL)), nil
	}
	agent.byPhase[workspace.PhasePlanning] = func(AgentTask) ([]byte, error) {
		return mustJSON(t, map[string]any{
			"version": 1, "testCases": cases,
			"rationale": "r", "coverageNotes": "c",
		}), nil
	}
	if analyse != nil {
		agent.byPhase[workspace.PhaseAnalysis] = analyse
	}
	return agent
}

// failedWithNetworkLog is an execution that failed with a network log on disk,
// the way the executor leaves one.
func failedWithNetworkLog(t *testing.T, params executor.TestcaseRunParams, entries []map[string]any) qaschema.ExecutionResult {
	t.Helper()

	log, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode network log: %v", err)
	}
	result := failWith(t, params, "the page never loaded", map[string]string{"screenshot-1.png": "png"})
	if err := os.WriteFile(filepath.Join(params.ArtifactDir, "network-0.json"), log, 0o600); err != nil {
		t.Fatalf("write network log: %v", err)
	}
	result.Artifacts = append(result.Artifacts, qaschema.Artifact{
		ID:   "network-0",
		Kind: qaschema.ArtifactKindNetwork,
		Key:  params.StorageKeyPrefix + params.TestCase.ID + "/network-0.json",
	})
	return result
}

// The acceptance criterion for the rule pass: a failure the rules can classify
// never starts an AI CLI. The provider is not stubbed to fail here — it has no
// analysis handler at all, so a call to it would be a hard error rather than a
// quietly different answer.
func TestRuleClassifiedFailuresNeverReachTheModel(t *testing.T) {
	storage := newFakeStorage(t)
	agent := fullRunAgent(t, []any{testCase("TC-001", "Create employee")}, nil)

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		// A request that never produced a response: NETWORK_ERROR by rule.
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "GET", "url": "http://app.internal:3000/employees", "startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	result := h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult)
	payload := decodeAs[qaschema.RunResultPayload](t, result.Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", payload.Status, payload.Error)
	}
	if len(payload.Findings) != 1 {
		t.Fatalf("findings = %+v, want one for the failed execution", payload.Findings)
	}
	if payload.Findings[0].FailureClass != qaschema.FailureClassNETWORKERROR {
		t.Fatalf("class = %s, want NETWORK_ERROR from the rule pass", payload.Findings[0].FailureClass)
	}
	if payload.Findings[0].AnalyzedBy != nil {
		t.Fatalf("a rule verdict claimed a provider: %+v", payload.Findings[0].AnalyzedBy)
	}

	for _, task := range agent.Tasks() {
		if task.Phase == workspace.PhaseAnalysis {
			t.Fatal("the analysis phase called the AI CLI for a failure the rules had already classified")
		}
	}
}

// A 401 is an authentication error, and it too is decided without a model.
func TestUnauthorizedIsClassifiedByRule(t *testing.T) {
	storage := newFakeStorage(t)
	agent := fullRunAgent(t, []any{testCase("TC-001", "Create employee")}, nil)

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "GET", "url": "http://app.internal:3000/api/me", "status": 401,
				"startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult).Payload)
	if len(payload.Findings) != 1 ||
		payload.Findings[0].FailureClass != qaschema.FailureClassAUTHENTICATIONERROR {
		t.Fatalf("findings = %+v, want one AUTHENTICATION_ERROR", payload.Findings)
	}
}

// A 500 is the judgement call, so it does reach the model — and the model's
// answer is what gets stored.
func TestAmbiguousFailuresReachTheModel(t *testing.T) {
	storage := newFakeStorage(t)
	var analysed bool
	agent := fullRunAgent(t, []any{testCase("TC-001", "Create employee")},
		func(task AgentTask) ([]byte, error) {
			analysed = true
			return mustJSON(t, []any{map[string]any{
				"version": 1, "testCaseId": "TC-001", "stepIndex": 0,
				"failureClass": "PRODUCT_BUG",
				"rootCause":    "POST /api/employees returned 500 and the row was never created",
				"confidence":   0.92,
				"evidence":     []any{firstArtifactID(t, task)},
			}}), nil
		})

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "POST", "url": "http://app.internal:3000/api/employees", "status": 500,
				"startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult).Payload)
	if !analysed {
		t.Fatal("a 500 was decided without the model; that is the judgement call the model is for")
	}
	if len(payload.Findings) != 1 || payload.Findings[0].FailureClass != qaschema.FailureClassPRODUCTBUG {
		t.Fatalf("findings = %+v, want the analyst's PRODUCT_BUG", payload.Findings)
	}
}

// The gate, end to end and through the real retry loop: a finding citing an
// artifact the execution never produced is refused, re-asked with the reason
// attached, and accepted once the citation is real.
//
// Driven by MockProvider rather than the fake AgentRunner, because the retry
// loop and Task.Review both live in agent.Runner — a fake that stands in for
// the whole runner would be asserting against a reimplementation of the thing
// under test.
func TestFabricatedEvidenceIsRejectedAndRetried(t *testing.T) {
	storage := newFakeStorage(t)
	// The id writeEvidence mints for screenshot-1.png in the run's first
	// execution, once namespaced. Hard-coded because MockProvider answers from
	// canned bytes: a canned analysis has to name evidence the run really
	// produced, which is the property the gate exists to enforce.
	const realCitation = "e0-art-screenshot-1-png"

	mock := scriptedMock(t, map[prompts.Phase][]agent.MockAnswer{
		prompts.PhaseAnalysis: {
			{Output: analysisAnswer(t, findingFor("TC-001", "screenshot-does-not-exist"))},
			{Output: analysisAnswer(t, findingFor("TC-001", realCitation))},
		},
	}, testCase("TC-001", "Create employee"))

	h := newHarness(t, harnessOptions{agent: mockAgentRunner(t, mock)})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "POST", "url": "http://app.internal:3000/api/employees", "status": 500,
				"startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult).Payload)

	if attempts := analysisAttempts(mock); attempts != 2 {
		t.Fatalf("the analyst was invoked %d time(s); the gate should have re-asked exactly once", attempts)
	}
	if payload.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", payload.Status, payload.Error)
	}
	if len(payload.Findings) != 1 || payload.Findings[0].FailureClass != qaschema.FailureClassPRODUCTBUG {
		t.Fatalf("findings = %+v", payload.Findings)
	}
	if len(payload.Findings[0].Evidence) != 1 || payload.Findings[0].Evidence[0] != realCitation {
		t.Fatalf("evidence = %v, want the real citation", payload.Findings[0].Evidence)
	}
}

// The rejected attempt's reasons reach the next prompt, so the model is told
// what was wrong rather than asked the same question again.
func TestTheGatesReasonsReachTheRetryPrompt(t *testing.T) {
	storage := newFakeStorage(t)
	mock := scriptedMock(t, map[prompts.Phase][]agent.MockAnswer{
		prompts.PhaseAnalysis: {
			{Output: analysisAnswer(t, findingFor("TC-001", "screenshot-does-not-exist"))},
			{Output: analysisAnswer(t, findingFor("TC-001", "e0-art-screenshot-1-png"))},
		},
	}, testCase("TC-001", "Create employee"))

	h := newHarness(t, harnessOptions{agent: mockAgentRunner(t, mock)})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "POST", "url": "http://app.internal:3000/api/employees", "status": 500,
				"startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))
	h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult)

	var retry string
	for _, call := range mock.Calls() {
		if call.Phase == prompts.PhaseAnalysis && call.Attempt == 2 {
			retry = call.Prompt
		}
	}
	if retry == "" {
		t.Fatal("there was no second analysis attempt to inspect")
	}
	if !strings.Contains(retry, analysis.RuleUnknownEvidence) {
		t.Fatalf("the retry prompt does not carry the rule that fired:\n%s", retry)
	}
	if !strings.Contains(retry, "screenshot-does-not-exist") {
		t.Fatalf("the retry prompt does not say which citation was refused:\n%s", retry)
	}
	// The feedback quotes the model's own previous answer, so it travels as
	// untrusted content rather than as instructions: on a hijacked first
	// attempt it is page content wearing the model's voice.
	if !strings.Contains(retry, security.MarkerStart) {
		t.Fatalf("the validator report was not framed as untrusted content:\n%s", retry)
	}
}

// The whole answer is refused, not the finding that was wrong. A run that kept
// the good half would produce a report whose gaps look exactly like failures
// nobody had anything to say about.
func TestOneFabricatedCitationRefusesTheWholeAnswer(t *testing.T) {
	storage := newFakeStorage(t)
	good := findingFor("TC-001", "e0-art-screenshot-1-png")
	bad := findingFor("TC-002", "invented-artifact")

	mock := scriptedMock(t, map[prompts.Phase][]agent.MockAnswer{
		// One answer, reused for every attempt: the model keeps making the
		// same mistake, the retries run out.
		prompts.PhaseAnalysis: {{Output: analysisAnswer(t, good, bad)}},
	}, testCase("TC-001", "Create employee"), testCase("TC-002", "Edit employee"))

	h := newHarness(t, harnessOptions{agent: mockAgentRunner(t, mock)})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "POST", "url": "http://app.internal:3000/api/employees", "status": 500,
				"startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult).Payload)

	if attempts := analysisAttempts(mock); attempts != agent.DefaultMaxAttempts {
		t.Fatalf("the analyst was invoked %d time(s), want one try and two retries", attempts)
	}
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q, want the analysis to fail after its retries", payload.Status)
	}

	// The good finding went with the bad one rather than being stored half —
	// and both failures still carry a finding that says plainly that nothing
	// was concluded.
	if len(payload.Findings) != 2 {
		t.Fatalf("findings = %+v, want one per failed execution even after a failed analysis", payload.Findings)
	}
	for _, finding := range payload.Findings {
		if finding.FailureClass != qaschema.FailureClassUNKNOWN {
			t.Fatalf("a refused answer was stored anyway: %+v", finding)
		}
		if !strings.Contains(finding.RootCause, "did not produce a usable verdict") {
			t.Fatalf("a synthesised finding reads like a real verdict: %q", finding.RootCause)
		}
	}
}

// A verdict the model was barely sure of is recorded as UNKNOWN. Its reasoning
// and its confidence survive: PRODUCT_BUG is a routing decision, not a number
// on a screen, and a 0.2-confidence one sends an engineer to read code that was
// never broken.
func TestALowConfidenceVerdictIsRecordedAsUnknown(t *testing.T) {
	storage := newFakeStorage(t)
	agent := fullRunAgent(t, []any{testCase("TC-001", "Create employee")},
		func(task AgentTask) ([]byte, error) {
			return mustJSON(t, []any{map[string]any{
				"version": 1, "testCaseId": "TC-001", "stepIndex": 0,
				"failureClass": "PRODUCT_BUG", "rootCause": "it might be the API, hard to say",
				"confidence": 0.15, "evidence": []any{firstArtifactID(t, task)},
			}}), nil
		})

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "POST", "url": "http://app.internal:3000/api/employees", "status": 500,
				"startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult).Payload)
	if len(payload.Findings) != 1 {
		t.Fatalf("findings = %+v", payload.Findings)
	}
	finding := payload.Findings[0]
	if finding.FailureClass != qaschema.FailureClassUNKNOWN {
		t.Fatalf("class = %s, want the low-confidence PRODUCT_BUG downgraded", finding.FailureClass)
	}
	if finding.Confidence != 0.15 || finding.RootCause != "it might be the API, hard to say" {
		t.Fatalf("the downgrade rewrote the analyst's own words: %+v", finding)
	}
}

// A run with nothing to explain does not start the analyst at all.
func TestAPassingRunSkipsAnalysisEntirely(t *testing.T) {
	storage := newFakeStorage(t)
	agent := fullRunAgent(t, []any{testCase("TC-001", "Create employee")}, nil)

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return writeEvidence(t, params, map[string]string{"screenshot-1.png": "png"})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult).Payload)
	if payload.Status != qaschema.RunResultPayloadStatusCompleted {
		t.Fatalf("status = %q, error = %+v", payload.Status, payload.Error)
	}
	if len(payload.Findings) != 0 {
		t.Fatalf("findings = %+v; a passing run has nothing to explain", payload.Findings)
	}
	for _, task := range agent.Tasks() {
		if task.Phase == workspace.PhaseAnalysis {
			t.Fatal("the analyst was asked to explain a run in which nothing failed")
		}
	}
}

// Every failed execution carries a finding — the criterion the whole phase
// exists to satisfy — including the ones the analyst forgot.
func TestEveryFailedExecutionGetsAFinding(t *testing.T) {
	storage := newFakeStorage(t)
	agent := fullRunAgent(t,
		[]any{testCase("TC-001", "Create employee"), testCase("TC-002", "Edit employee")},
		func(task AgentTask) ([]byte, error) {
			// Answers about one of the two, every time. The gate re-asks, the
			// retries run out, and the daemon covers the gap itself.
			return mustJSON(t, []any{map[string]any{
				"version": 1, "testCaseId": "TC-001", "stepIndex": 0,
				"failureClass": "PRODUCT_BUG", "rootCause": "500 from the API",
				"confidence": 0.9, "evidence": []any{firstArtifactID(t, task)},
			}}), nil
		})

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "POST", "url": "http://app.internal:3000/api/employees", "status": 500,
				"startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult).Payload)
	if len(payload.Executions) != 2 {
		t.Fatalf("executions = %d", len(payload.Executions))
	}
	if len(payload.Findings) != 2 {
		t.Fatalf("findings = %+v, want one per failed execution", payload.Findings)
	}
	covered := map[string]bool{}
	for _, finding := range payload.Findings {
		covered[finding.TestCaseID] = true
		if len(finding.Evidence) == 0 {
			t.Fatalf("a finding cites nothing: %+v", finding)
		}
	}
	if !covered["TC-001"] || !covered["TC-002"] {
		t.Fatalf("a failed execution was left silent: %v", covered)
	}
}

// Artifact ids are unique across a run, so a finding's evidence link opens the
// evidence from its own execution.
//
// The executor mints them from a counter that restarts at zero per test case,
// and ingest keys one run-wide map on them: without this, a finding about case
// one that cites `screenshot-0` resolves to case two's screenshot.
func TestArtifactIDsAreUniqueAcrossTheRun(t *testing.T) {
	storage := newFakeStorage(t)
	agent := fullRunAgent(t,
		[]any{testCase("TC-001", "Create employee"), testCase("TC-002", "Edit employee")}, nil)

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		// The same file name in both cases, which is exactly what the
		// executor produces: its collector is per case.
		result := failedWithNetworkLog(t, params, []map[string]any{
			{"method": "GET", "url": "http://app.internal:3000/", "startedAt": "2026-09-05T10:00:00Z"},
		})
		result.Steps[0].ArtifactIDs = []qaschema.ArtifactID{result.Artifacts[0].ID}
		return result
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(20*time.Second, qaschema.EnvelopeTypeRunResult).Payload)

	seen := map[string]string{}
	for _, artifact := range payload.Artifacts {
		if previous, clash := seen[artifact.ID]; clash {
			t.Fatalf("artifact id %q names two objects: %s and %s", artifact.ID, previous, artifact.Key)
		}
		seen[artifact.ID] = artifact.Key
	}
	if len(seen) != len(payload.Artifacts) || len(payload.Artifacts) < 4 {
		t.Fatalf("artifacts = %d, unique ids = %d", len(payload.Artifacts), len(seen))
	}

	// And the references inside each execution moved with them.
	for _, execution := range payload.Executions {
		for _, step := range execution.Steps {
			for _, id := range step.ArtifactIDs {
				if _, ok := seen[id]; !ok {
					t.Fatalf("%s step %d points at %q, which names no artifact in this run",
						execution.TestCaseID, step.Index, id)
				}
			}
		}
	}
}

// scriptedMock builds a MockProvider that answers discovery and planning the
// same way every time and analysis from a script.
func scriptedMock(t *testing.T, answers map[prompts.Phase][]agent.MockAnswer, cases ...any) *agent.MockProvider {
	t.Helper()

	full := map[prompts.Phase][]agent.MockAnswer{
		prompts.PhaseDiscovery: {{Output: mustJSON(t, testAppMap("http://app.internal:3000"))}},
		prompts.PhasePlanning: {{Output: mustJSON(t, map[string]any{
			"version": 1, "testCases": cases, "rationale": "r", "coverageNotes": "c",
		})}},
	}
	for phase, scripted := range answers {
		full[phase] = scripted
	}
	return agent.NewMockProvider(agent.MockOptions{Answers: full})
}

// findingFor is one analysis answer element.
func findingFor(ref, evidence string) map[string]any {
	return map[string]any{
		"version": 1, "testCaseId": ref, "stepIndex": 0,
		"failureClass": "PRODUCT_BUG",
		"rootCause":    "POST /api/employees returned 500 and the row was never created",
		"confidence":   0.92,
		"evidence":     []any{evidence},
	}
}

func analysisAnswer(t *testing.T, findings ...map[string]any) []byte {
	t.Helper()
	return mustJSON(t, findings)
}

func analysisAttempts(mock *agent.MockProvider) int {
	count := 0
	for _, call := range mock.Calls() {
		if call.Phase == prompts.PhaseAnalysis {
			count++
		}
	}
	return count
}

// An execution the phase cannot write a finding for does not take the rest of
// the report with it.
//
// The path is real rather than theoretical. uploadEvidenceBundles warns and
// carries on when an upload fails, which is deliberate — losing a whole report
// because one JSON file did not reach S3 is the wrong trade. But an execution
// that failed at the transport captured no evidence files of its own, so the
// bundle was its only citable artifact, and without it no finding@1 can be
// written for it at all. Before this was fixed, that one execution turned the
// entire findings list into nil on the way out.
func TestAnUncoverableFailureDoesNotEmptyTheReport(t *testing.T) {
	storage := newFakeStorage(t)
	// TC-002's evidence bundle is refused; everything else uploads.
	storage.RejectKeys(func(key string) bool {
		return strings.Contains(key, "TC-002/") && strings.Contains(key, evidenceFileName)
	})

	agent := fullRunAgent(t,
		[]any{testCase("TC-001", "Create employee"), testCase("TC-002", "Edit employee")}, nil)

	h := newHarness(t, harnessOptions{agent: agent})
	h.executor.onRun = func(params executor.TestcaseRunParams) qaschema.ExecutionResult {
		if params.TestCase.ID == "TC-002" {
			// A transport failure: the executor never got far enough to
			// capture anything, which is what synthesiseFailure produces.
			now := time.Now().UTC().Format(time.RFC3339)
			return qaschema.ExecutionResult{
				Version: 1, TestCaseID: params.TestCase.ID, Result: qaschema.OutcomeError,
				Message:   ptr("the executor could not run this test case"),
				Steps:     []qaschema.StepResult{},
				Artifacts: []qaschema.Artifact{},
				StartedAt: now, EndedAt: now,
			}
		}
		return failedWithNetworkLog(t, params, []map[string]any{
			{"method": "GET", "url": "http://app.internal:3000/", "startedAt": "2026-09-05T10:00:00Z"},
		})
	}

	h.backend.ExpectType(5*time.Second, qaschema.EnvelopeTypeHello)
	h.backend.Send(assignFrame(t, assignOptions{
		mode: qaschema.RunAssignPayloadModeFull, putBase: storage.PutBase(),
	}))

	payload := decodeAs[qaschema.RunResultPayload](t,
		h.backend.ExpectType(30*time.Second, qaschema.EnvelopeTypeRunResult).Payload)

	// The phase failed, and says so — an execution with no finding is a real
	// problem and must not be reported as a clean run.
	if payload.Status != qaschema.RunResultPayloadStatusFailed {
		t.Fatalf("status = %q, want the uncoverable execution to fail the phase", payload.Status)
	}
	// But TC-001 was classified by rule before any of that, and its finding
	// reaches the backend.
	if len(payload.Findings) != 1 {
		t.Fatalf("findings = %+v, want the one execution that could be classified", payload.Findings)
	}
	if payload.Findings[0].TestCaseID != "TC-001" ||
		payload.Findings[0].FailureClass != qaschema.FailureClassNETWORKERROR {
		t.Fatalf("finding = %+v", payload.Findings[0])
	}
	if len(payload.Executions) != 2 {
		t.Fatalf("executions = %d; a failed analysis threw away the run", len(payload.Executions))
	}
}
