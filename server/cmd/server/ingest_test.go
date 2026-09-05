package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	runpkg "github.com/ChinnakornP/longtest/server/internal/run"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// What ingest refuses to store.
//
// The producer half of this landed first (LONG-17): the daemon namespaces
// every executor-minted artifact handle so it is unique across a run. These
// are the consumer half — the frames a daemon that did NOT do that sends, and
// what the backend does with them now that it can tell.

// rejectionEventView is a run event with its `data` decoded as the structured
// rule/detail pairs. It is written out by hand rather than reusing the
// service's own types so that a rename there fails this test instead of
// silently changing what an alert matches on.
type rejectionEventView struct {
	Seq     int64  `json:"seq"`
	Phase   string `json:"phase"`
	Level   string `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Stored     int `json:"stored"`
		Rejections []struct {
			TestCaseID string `json:"testCaseId"`
			StepIndex  int    `json:"stepIndex"`
			Rule       string `json:"rule"`
			Detail     string `json:"detail"`
		} `json:"rejections"`
	} `json:"data"`
}

func listRejectionEvents(t *testing.T, client *authtest.Client, runID uuid.UUID) []rejectionEventView {
	t.Helper()

	resp := client.Get(t, "/api/v1/runs/"+runID.String()+"/events")
	resp.ExpectStatus(t, http.StatusOK)

	var body struct {
		Events []rejectionEventView `json:"events"`
	}
	resp.JSON(t, &body)
	return body.Events
}

func rejectionEvent(t *testing.T, client *authtest.Client, runID uuid.UUID) rejectionEventView {
	t.Helper()

	for _, event := range listRejectionEvents(t, client, runID) {
		if event.Code == "result_rejected" {
			return event
		}
	}
	t.Fatal("the run stream does not say the result was rejected")
	return rejectionEventView{}
}

// ingestEnv is one run, assigned to a daemon, with two approved cases waiting
// for it. Every test below differs only in the result frame it then sends.
type ingestEnv struct {
	*qaEnv
	owner  *authtest.Client
	daemon *daemonClient
	run    runView
	refs   []string
}

func newIngestEnv(t *testing.T) *ingestEnv {
	t.Helper()

	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://ingest.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	refs := []string{"TC-001", "TC-002"}
	for _, ref := range refs {
		caseID := seedTestCase(t, env, owner, projectID, ref)
		owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "approved"}).ExpectStatus(t, http.StatusOK)
	}

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "execute")
	daemon.receive(t, time.Second)

	return &ingestEnv{qaEnv: env, owner: owner, daemon: daemon, run: created, refs: refs}
}

func (e *ingestEnv) key(t *testing.T, ref, name string) string {
	t.Helper()

	key, err := artifact.ObjectKey(e.owner.OrgID, e.run.ID, e.run.CreatedAt, ref, name)
	if err != nil {
		t.Fatalf("build artifact key: %v", err)
	}
	return key
}

// execution is one failed case with one screenshot, under the handle the
// caller chooses — which is the whole variable in these tests.
func (e *ingestEnv) execution(t *testing.T, ref, handle string) qaschema.ExecutionResult {
	t.Helper()

	return qaschema.ExecutionResult{
		Version: 1, TestCaseID: ref, Result: qaschema.OutcomeFail,
		Message: strp("the row never appeared"),
		Steps: []qaschema.StepResult{
			{Index: 0, Action: qaschema.StepActionNavigate, Status: qaschema.OutcomePass},
			{Index: 1, Action: qaschema.StepActionClick, Status: qaschema.OutcomeFail},
		},
		Artifacts: []qaschema.Artifact{{
			ID: handle, Kind: qaschema.ArtifactKindScreenshot, Key: e.key(t, ref, "shot.png"),
		}},
		StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		EndedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

func (e *ingestEnv) finding(ref, handle string) qaschema.Finding {
	return qaschema.Finding{
		Version: 1, TestCaseID: ref, StepIndex: intp(1),
		FailureClass: qaschema.FailureClassPRODUCTBUG,
		RootCause:    "POST /api/employees returned 500 for " + ref,
		Confidence:   0.9,
		Evidence:     []qaschema.ArtifactID{handle},
	}
}

// sendResult delivers the frame and waits for the run to become terminal.
func (e *ingestEnv) sendResult(t *testing.T, payload qaschema.RunResultPayload) runView {
	t.Helper()

	e.daemon.send(t, qaschema.EnvelopeTypeRunResult, &e.run.ID, 1, payload)
	waitFor(t, 5*time.Second, func() bool {
		return isTerminalStatus(getRun(t, e.owner, e.run.ID).Status)
	})
	return getRun(t, e.owner, e.run.ID)
}

func isTerminalStatus(status string) bool {
	switch status {
	case "passed", "failed", "error", "cancelled":
		return true
	}
	return false
}

// report reads what a client would see for this run.
func (e *ingestEnv) report(t *testing.T) reportBody {
	t.Helper()

	resp := e.owner.Get(t, "/api/v1/runs/"+e.run.ID.String()+"/report")
	resp.ExpectStatus(t, http.StatusOK)
	var report reportBody
	resp.JSON(t, &report)
	return report
}

// storedArtifacts is what actually reached the artifacts table. The report
// only shows artifacts through an execution or a finding, so a row written and
// then orphaned would not appear there — and "no row was written" is the claim
// these tests are making.
func (e *ingestEnv) storedArtifacts(t *testing.T) []dbgen.Artifact {
	t.Helper()

	rows, err := e.Store.ListArtifactsForRun(t.Context(), dbgen.ListArtifactsForRunParams{
		OrgID: e.owner.OrgID, RunID: e.run.ID,
	})
	if err != nil {
		t.Fatalf("list the run's artifacts: %v", err)
	}
	return rows
}

// Acceptance: a result frame that uses one artifact handle for two different
// objects is refused whole, and the event says which handle collided.
//
// This is the frame a daemon older than LONG-17 sends. The executor mints
// artifact ids from a counter that restarts at zero for every test case, so
// forty cases produce forty artifacts called `screenshot-0`; ingest keys one
// run-wide map on them. Before this change the last one won, and a finding
// about TC-001 rendered TC-002's screenshot — a link that opens, and is a
// picture of a different test.
func TestADuplicateArtifactHandleRejectsTheWholeFrame(t *testing.T) {
	env := newIngestEnv(t)

	finished := env.sendResult(t, qaschema.RunResultPayload{
		Status: qaschema.RunResultPayloadStatusCompleted,
		Executions: []qaschema.ExecutionResult{
			env.execution(t, "TC-001", "screenshot-0"),
			env.execution(t, "TC-002", "screenshot-0"),
		},
		Findings: []qaschema.Finding{env.finding("TC-001", "screenshot-0")},
	})

	if finished.Status != "error" {
		t.Fatalf("a frame with a colliding artifact handle finished the run as %q", finished.Status)
	}
	if finished.Error == nil || finished.Error.Code != string(qaschema.RunErrorCodeAgentOutputInvalid) {
		t.Fatalf("got error %+v, want %s", finished.Error, qaschema.RunErrorCodeAgentOutputInvalid)
	}

	// Not one row. The transaction rollback is the guarantee, and this is what
	// makes it a guarantee rather than a claim: a frame accepted halfway is a
	// report nobody can read the gaps out of.
	if stored := env.storedArtifacts(t); len(stored) != 0 {
		t.Errorf("a refused frame left %d artifacts behind", len(stored))
	}
	report := env.report(t)
	if len(report.Findings) != 0 {
		t.Errorf("a refused frame left %d findings behind", len(report.Findings))
	}
	for _, execution := range report.Executions {
		if execution.Result == "failed" || execution.Result == "passed" {
			t.Errorf("execution %s was closed out as %q by a refused frame",
				execution.TestCaseRef, execution.Result)
		}
	}

	event := rejectionEvent(t, env.owner, env.run.ID)
	if event.Level != "error" {
		t.Errorf("the rejection is recorded at level %q", event.Level)
	}
	if event.Message == "" {
		t.Error("the result_rejected event carries no reason")
	}
	if len(event.Data.Rejections) != 1 {
		t.Fatalf("the event carries %d rejections, want 1", len(event.Data.Rejections))
	}
	rejection := event.Data.Rejections[0]
	if rejection.Rule != runpkg.RuleDuplicateArtifactHandle {
		t.Fatalf("rule = %q, want %q", rejection.Rule, runpkg.RuleDuplicateArtifactHandle)
	}
	// The handle and both sides of the collision, which is what tells a daemon
	// author which two cases produced the same id.
	for _, want := range []string{`"screenshot-0"`, "TC-001/shot.png", "TC-002/shot.png"} {
		if !strings.Contains(rejection.Detail, want) {
			t.Errorf("the rejection detail does not name %s: %s", want, rejection.Detail)
		}
	}
	// The rule string, not the model's words: an alert matches on the rule.
	if !strings.Contains(event.Message, runpkg.RuleDuplicateArtifactHandle) {
		t.Errorf("message = %q, want the rule in it", event.Message)
	}
}

// Acceptance: a finding citing a handle the frame does not carry is refused,
// not stored with the citation quietly dropped.
//
// This used to link whatever resolved and store the finding regardless, so a
// verdict reached the report with the evidence behind it removed — and
// finding@1 declares `evidence` with minItems 1 precisely because a finding
// nothing supports is a guess.
func TestAFindingCitingAnUnknownHandleRejectsTheWholeFrame(t *testing.T) {
	env := newIngestEnv(t)

	finished := env.sendResult(t, qaschema.RunResultPayload{
		Status: qaschema.RunResultPayloadStatusCompleted,
		Executions: []qaschema.ExecutionResult{
			env.execution(t, "TC-001", "e0-screenshot-0"),
			env.execution(t, "TC-002", "e1-screenshot-0"),
		},
		Findings: []qaschema.Finding{
			env.finding("TC-001", "e0-screenshot-0"),
			// A handle no artifact in this frame carries — what an analyst
			// that invented a citation produces.
			env.finding("TC-002", "e7-screenshot-0"),
		},
	})

	if finished.Status != "error" {
		t.Fatalf("a frame citing an unknown handle finished the run as %q", finished.Status)
	}

	report := env.report(t)
	if len(report.Findings) != 0 {
		// Including the one finding that WAS well-formed: half a result is a
		// report whose gaps nobody downstream can see.
		t.Errorf("a refused frame left %d findings behind", len(report.Findings))
	}
	if stored := env.storedArtifacts(t); len(stored) != 0 {
		t.Errorf("a refused frame left %d artifacts behind", len(stored))
	}

	event := rejectionEvent(t, env.owner, env.run.ID)
	if len(event.Data.Rejections) != 1 {
		t.Fatalf("the event carries %d rejections, want 1", len(event.Data.Rejections))
	}
	rejection := event.Data.Rejections[0]
	if rejection.Rule != runpkg.RuleUnknownEvidenceHandle {
		t.Fatalf("rule = %q, want %q", rejection.Rule, runpkg.RuleUnknownEvidenceHandle)
	}
	if rejection.TestCaseID != "TC-002" {
		t.Errorf("the rejection blames %q, want the case whose finding cited nothing", rejection.TestCaseID)
	}
	if !strings.Contains(rejection.Detail, `"e7-screenshot-0"`) {
		t.Errorf("the rejection detail does not name the handle: %s", rejection.Detail)
	}
}

// Acceptance: the frame a current daemon actually sends still lands.
//
// It is not the shape the other report tests use. A real run.result lists each
// execution's evidence TWICE — once in the run-level `artifacts[]` that the
// daemon builds from its uploads, and once inside the execution the executor
// reported — and both entries carry the same handle. That is one artifact
// described twice, not a collision, and a duplicate check that could not tell
// the difference would fail every real run.
func TestTheFrameACurrentDaemonSendsIsStillAccepted(t *testing.T) {
	env := newIngestEnv(t)

	executions := make([]qaschema.ExecutionResult, 0, len(env.refs))
	runLevel := make([]qaschema.Artifact, 0, len(env.refs)*2)
	findings := make([]qaschema.Finding, 0, len(env.refs))
	for i, ref := range env.refs {
		handle := fmt.Sprintf("e%d-screenshot-0", i)
		execution := env.execution(t, ref, handle)
		executions = append(executions, execution)
		// What the daemon uploaded, echoed at run level exactly as
		// runTestCases returns it.
		runLevel = append(runLevel, execution.Artifacts...)
		// And the analysis phase's own evidence bundle for the same case,
		// which is run-level only and carries its own handle.
		bundle := fmt.Sprintf("analysis-%d", i)
		runLevel = append(runLevel, qaschema.Artifact{
			ID: bundle, Kind: qaschema.ArtifactKindReport, Key: env.key(t, ref, "evidence.json"),
		})
		cited := env.finding(ref, handle)
		cited.Evidence = append(cited.Evidence, bundle)
		findings = append(findings, cited)
	}

	finished := env.sendResult(t, qaschema.RunResultPayload{
		Status:     qaschema.RunResultPayloadStatusCompleted,
		Executions: executions,
		Artifacts:  runLevel,
		Findings:   findings,
	})
	if finished.Status != "failed" {
		t.Fatalf("the run finished as %q (%+v), want failed — two executions failed", finished.Status, finished.Error)
	}

	report := env.report(t)
	if len(report.Findings) != 2 {
		t.Fatalf("the report carries %d findings, want 2", len(report.Findings))
	}
	byExecution := map[uuid.UUID]string{}
	for _, execution := range report.Executions {
		byExecution[execution.ID] = execution.TestCaseRef
	}
	for _, finding := range report.Findings {
		if finding.ExecutionID == nil {
			t.Fatalf("a finding names no execution: %+v", finding)
		}
		ref := byExecution[*finding.ExecutionID]
		if finding.RootCause != "POST /api/employees returned 500 for "+ref {
			t.Fatalf("the finding for %s carries %q", ref, finding.RootCause)
		}
		// Both citations, resolved, and the screenshot is this case's own.
		if len(finding.Evidence) != 2 {
			t.Fatalf("the finding for %s cites %d artifacts, want 2", ref, len(finding.Evidence))
		}
		for _, evidence := range finding.Evidence {
			if evidence.URL == "" {
				t.Errorf("the finding for %s has an evidence entry with no openable link", ref)
			}
		}
	}

	// Not asserted here, and worth knowing: on this frame shape
	// report.executions[].artifacts comes back empty. Ingest writes the
	// run-level copy first with no execution, and UpsertArtifact's
	// ON CONFLICT (storage_key) DO UPDATE does not re-attribute the row when
	// the execution's own copy arrives. That is a separate bug from the one
	// this file is about — the evidence a finding cites still resolves,
	// because finding_evidence is its own table — and asserting the current
	// behaviour here would nail it down rather than report it.

	// Listing an artifact at both levels is one row, not two: the upsert is on
	// the storage key, which is what makes the two entries the same artifact.
	if stored := env.storedArtifacts(t); len(stored) != 4 {
		keys := make([]string, len(stored))
		for i, row := range stored {
			keys[i] = row.StorageKey
		}
		t.Fatalf("stored %d artifacts, want 4 (a screenshot and a bundle per case): %v", len(stored), keys)
	}

	for _, event := range listRejectionEvents(t, env.owner, env.run.ID) {
		if event.Code == "result_rejected" {
			t.Fatalf("a good frame was refused: %s", event.Message)
		}
	}
}
