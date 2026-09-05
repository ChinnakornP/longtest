package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// The report as a client has to read it.
//
// TestRunResultBuildsTheReport covers one execution end to end. This is about
// the part a UI cannot work around if it is wrong: which finding belongs to
// which execution, and whether each one's evidence link opens that execution's
// own evidence rather than another's.

// reportBody is the response decoded as docs/api/openapi.yaml describes it. It
// is written out by hand rather than reusing the service's own view structs so
// that a rename there fails this test instead of silently changing the wire.
type reportBody struct {
	Executions []struct {
		ID           uuid.UUID `json:"id"`
		TestCaseRef  string    `json:"testCaseRef"`
		Result       string    `json:"result"`
		FailureClass string    `json:"failureClass"`
		Artifacts    []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
			Kind string    `json:"kind"`
			URL  string    `json:"url"`
		} `json:"artifacts"`
	} `json:"executions"`
	Findings []struct {
		ID           uuid.UUID  `json:"id"`
		ExecutionID  *uuid.UUID `json:"executionId"`
		TestCaseID   *uuid.UUID `json:"testCaseId"`
		StepIndex    *int       `json:"stepIndex"`
		FailureClass string     `json:"failureClass"`
		RootCause    string     `json:"rootCause"`
		Confidence   float64    `json:"confidence"`
		CreatedAt    time.Time  `json:"createdAt"`
		Evidence     []struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
			Kind string    `json:"kind"`
			URL  string    `json:"url"`
		} `json:"evidence"`
	} `json:"findings"`
}

// A finding names the execution it is about, and its evidence is that
// execution's own.
//
// The join is the whole contract for a report UI: findings and executions are
// separate arrays, and `findings[].executionId = executions[].id` is the only
// thing pairing them. Two failing cases, because one case cannot show a
// mis-join — every candidate is the right answer when there is only one.
func TestReportPairsEachFindingWithItsOwnExecution(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://report.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	for _, ref := range []string{"TC-001", "TC-002"} {
		caseID := seedTestCase(t, env, owner, projectID, ref)
		owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
			map[string]string{"status": "approved"}).ExpectStatus(t, http.StatusOK)
	}

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "execute")
	daemon.receive(t, time.Second)

	// Distinct handles per execution, which is what the daemon guarantees: the
	// backend keys one run-wide map on them, so two executions offering the
	// same handle would resolve to whichever landed last.
	executions := make([]qaschema.ExecutionResult, 0, 2)
	findings := make([]qaschema.Finding, 0, 2)
	for i, ref := range []string{"TC-001", "TC-002"} {
		name := ref + "-shot.png"
		key, err := artifact.ObjectKey(owner.OrgID, created.ID, created.CreatedAt, ref, name)
		if err != nil {
			t.Fatalf("build artifact key: %v", err)
		}
		handle := fmt.Sprintf("e%d-screenshot-0", i)

		executions = append(executions, qaschema.ExecutionResult{
			Version: 1, TestCaseID: ref, Result: qaschema.OutcomeFail,
			Message: strp("the row never appeared"),
			Steps: []qaschema.StepResult{
				{Index: 0, Action: qaschema.StepActionNavigate, Status: qaschema.OutcomePass},
				{Index: 1, Action: qaschema.StepActionClick, Status: qaschema.OutcomeFail},
			},
			Artifacts: []qaschema.Artifact{{ID: handle, Kind: qaschema.ArtifactKindScreenshot, Key: key}},
			StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			EndedAt:   time.Now().UTC().Format(time.RFC3339),
		})
		findings = append(findings, qaschema.Finding{
			Version: 1, TestCaseID: ref, StepIndex: intp(1),
			FailureClass: qaschema.FailureClassPRODUCTBUG,
			RootCause:    "POST /api/employees returned 500 for " + ref,
			Confidence:   0.9,
			Evidence:     []qaschema.ArtifactID{handle},
		})
	}

	daemon.send(t, qaschema.EnvelopeTypeRunResult, &created.ID, 1, qaschema.RunResultPayload{
		Status:     qaschema.RunResultPayloadStatusCompleted,
		Executions: executions,
		Findings:   findings,
	})
	waitFor(t, 5*time.Second, func() bool { return getRun(t, owner, created.ID).Status == "failed" })

	resp := owner.Get(t, "/api/v1/runs/"+created.ID.String()+"/report")
	resp.ExpectStatus(t, http.StatusOK)
	var report reportBody
	resp.JSON(t, &report)

	if len(report.Executions) != 2 || len(report.Findings) != 2 {
		t.Fatalf("report has %d executions and %d findings, want 2 and 2",
			len(report.Executions), len(report.Findings))
	}

	byExecution := map[uuid.UUID]string{}
	for _, execution := range report.Executions {
		byExecution[execution.ID] = execution.TestCaseRef
	}

	for _, finding := range report.Findings {
		if finding.ExecutionID == nil {
			t.Fatalf("a finding names no execution, so nothing can pair it: %+v", finding)
		}
		ref, ok := byExecution[*finding.ExecutionID]
		if !ok {
			t.Fatalf("finding %s points at execution %s, which is not in this report",
				finding.ID, *finding.ExecutionID)
		}
		// The root cause names the case it was written about, so a finding
		// attached to the wrong execution is visible rather than plausible.
		if finding.RootCause != "POST /api/employees returned 500 for "+ref {
			t.Fatalf("the finding for %s carries %q", ref, finding.RootCause)
		}
		if len(finding.Evidence) != 1 {
			t.Fatalf("finding for %s cites %d artifacts, want 1", ref, len(finding.Evidence))
		}
		// The link a person clicks: it must open this execution's screenshot,
		// not the other one's. A run-local handle reused across executions is
		// exactly how that goes wrong, and it goes wrong silently — the file
		// opens, it is the same kind, and it is of a different test.
		if finding.Evidence[0].Name != ref+"-shot.png" {
			t.Fatalf("the finding for %s cites %q", ref, finding.Evidence[0].Name)
		}
		if finding.Evidence[0].URL == "" {
			t.Fatalf("the finding for %s has no openable evidence link", ref)
		}
		if finding.Evidence[0].Kind != "screenshot" {
			t.Fatalf("evidence kind = %q", finding.Evidence[0].Kind)
		}
	}
}

// Everything docs/api/openapi.yaml marks required on a Finding is present, and
// present as the documented type. T16 generates its UI straight from that
// contract, so a field the spec promises and the service omits is a blank
// panel rather than a caught error.
func TestReportFindingCarriesEveryRequiredField(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://report.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	caseID := seedTestCase(t, env, owner, projectID, "TC-001")
	owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+caseID.String(),
		map[string]string{"status": "approved"}).ExpectStatus(t, http.StatusOK)

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "execute")
	daemon.receive(t, time.Second)

	key, err := artifact.ObjectKey(owner.OrgID, created.ID, created.CreatedAt, "TC-001", "shot.png")
	if err != nil {
		t.Fatalf("build artifact key: %v", err)
	}
	daemon.send(t, qaschema.EnvelopeTypeRunResult, &created.ID, 1, struct {
		Status     qaschema.RunResultPayloadStatus `json:"status"`
		Executions []qaschema.ExecutionResult      `json:"executions"`
		Findings   []json.RawMessage               `json:"findings"`
	}{
		Status: qaschema.RunResultPayloadStatusCompleted,
		Executions: []qaschema.ExecutionResult{{
			Version: 1, TestCaseID: "TC-001", Result: qaschema.OutcomeFail,
			Steps:     []qaschema.StepResult{{Index: 0, Action: qaschema.StepActionClick, Status: qaschema.OutcomeFail}},
			Artifacts: []qaschema.Artifact{{ID: "e0-screenshot-0", Kind: qaschema.ArtifactKindScreenshot, Key: key}},
			StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			EndedAt:   time.Now().UTC().Format(time.RFC3339),
		}},
		// A whole-case finding: finding@1 makes stepIndex required AND
		// nullable, so it travels as an explicit null and a client has to be
		// able to tell "no single step" from "field missing". Written as a raw
		// map because the generated Finding type tags stepIndex omitempty and
		// would drop the property — the same reason daemon/runtime forwards
		// findings as bytes rather than as structs.
		Findings: []json.RawMessage{json.RawMessage(`{
			"version": 1,
			"testCaseId": "TC-001",
			"stepIndex": null,
			"failureClass": "UNKNOWN",
			"rootCause": "the evidence does not say which step is at fault",
			"confidence": 0,
			"evidence": ["e0-screenshot-0"],
			"analyzedBy": {"provider": "claude", "version": "1.2.3"}
		}`)},
	})
	waitFor(t, 5*time.Second, func() bool { return getRun(t, owner, created.ID).Status == "failed" })

	resp := owner.Get(t, "/api/v1/runs/"+created.ID.String()+"/report")
	resp.ExpectStatus(t, http.StatusOK)

	var raw struct {
		Findings []map[string]json.RawMessage `json:"findings"`
	}
	resp.JSON(t, &raw)
	if len(raw.Findings) != 1 {
		t.Fatalf("findings = %d", len(raw.Findings))
	}
	for _, field := range []string{"id", "failureClass", "rootCause", "confidence", "evidence", "createdAt"} {
		if _, ok := raw.Findings[0][field]; !ok {
			t.Errorf("the spec marks %q required and the report omits it", field)
		}
	}

	var report reportBody
	resp.JSON(t, &report)
	finding := report.Findings[0]
	if finding.FailureClass != "UNKNOWN" {
		t.Fatalf("failureClass = %q", finding.FailureClass)
	}
	// A confidence of 0 is a real value, not an absent one: it is what an
	// unclassifiable failure carries, and omitempty here would render it as a
	// missing field on every honest UNKNOWN.
	if finding.Confidence != 0 {
		t.Fatalf("confidence = %v", finding.Confidence)
	}
	if finding.StepIndex != nil {
		t.Fatalf("stepIndex = %v, want absent for a whole-case finding", *finding.StepIndex)
	}
	if finding.CreatedAt.IsZero() {
		t.Fatal("createdAt is zero")
	}
	if finding.TestCaseID == nil {
		t.Fatal("the finding does not name its test case")
	}
}
