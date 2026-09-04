package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	runpkg "github.com/ChinnakornP/longtest/server/internal/run"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// The daemon control plane, checked against the acceptance criteria of LONG-10.
// Every test here drives a real WebSocket against the real router, because the
// parts that break — the assignment race, the dedup gate, the tenancy check on
// a frame — only exist once there is a socket.

// startRun creates a run pinned to a runtime and returns it.
func startRun(t *testing.T, client *authtest.Client, projectID, runtimeID uuid.UUID, mode string) runView {
	t.Helper()

	body := map[string]any{"projectId": projectID, "mode": mode}
	if runtimeID != uuid.Nil {
		body["runtimeId"] = runtimeID
	}
	resp := client.Post(t, "/api/v1/runs", body)
	resp.ExpectStatus(t, http.StatusCreated)

	var view runView
	resp.JSON(t, &view)
	return view
}

func getRun(t *testing.T, client *authtest.Client, runID uuid.UUID) runView {
	t.Helper()

	resp := client.Get(t, "/api/v1/runs/"+runID.String())
	resp.ExpectStatus(t, http.StatusOK)

	var view runView
	resp.JSON(t, &view)
	return view
}

func listEvents(t *testing.T, client *authtest.Client, runID uuid.UUID, since string) []eventView {
	t.Helper()

	path := "/api/v1/runs/" + runID.String() + "/events"
	if since != "" {
		path += "?since=" + since
	}
	resp := client.Get(t, path)
	resp.ExpectStatus(t, http.StatusOK)

	var body struct {
		Events []eventView `json:"events"`
	}
	resp.JSON(t, &body)
	return body.Events
}

type eventView struct {
	Seq     int64  `json:"seq"`
	Phase   string `json:"phase"`
	Level   string `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func eventPayload(code, message string) qaschema.RunEventPayload {
	return qaschema.RunEventPayload{
		Phase: qaschema.RunEventPayloadPhaseDiscover, Level: qaschema.RunEventPayloadLevelInfo,
		Code: code, Message: message,
	}
}

// Acceptance: creating a run reaches an online daemon in the same organization
// within one second. Assignment latency is a product characteristic — the UI
// should show a run starting within a second of the click (ADR-005).
func TestAnOnlineDaemonIsAssignedARunWithinOneSecond(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://assign.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)

	started := time.Now()
	created := startRun(t, owner, projectID, runtimeID, "discover")

	frame := daemon.receive(t, time.Second)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("run.assign took %s, the contract allows one second", elapsed)
	}
	if frame.Type != qaschema.EnvelopeTypeRunAssign {
		t.Fatalf("got a %s frame, want run.assign", frame.Type)
	}
	if frame.RunID == nil || *frame.RunID != created.ID.String() {
		t.Fatalf("run.assign is for %v, want %s", frame.RunID, created.ID)
	}

	var payload struct {
		RunID          string                  `json:"runId"`
		Mode           string                  `json:"mode"`
		ProjectID      string                  `json:"projectId"`
		BaseURL        string                  `json:"baseUrl"`
		ArtifactUpload qaschema.ArtifactUpload `json:"artifactUpload"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("decode run.assign payload: %v", err)
	}
	if payload.BaseURL != "https://assign.example.com" {
		t.Fatalf("got baseUrl %q, want the project's", payload.BaseURL)
	}

	// The upload grant is bounded to this run's own prefix and does not
	// outlive the contract's six-hour ceiling.
	wantPrefix := artifact.KeyPrefix(owner.OrgID, created.ID, created.CreatedAt)
	if payload.ArtifactUpload.KeyPrefix != wantPrefix {
		t.Fatalf("got key prefix %q, want %q", payload.ArtifactUpload.KeyPrefix, wantPrefix)
	}
	expiresAt, err := time.Parse(time.RFC3339, payload.ArtifactUpload.ExpiresAt)
	if err != nil {
		t.Fatalf("the upload grant has an unparsable expiry %q: %v", payload.ArtifactUpload.ExpiresAt, err)
	}
	if until := time.Until(expiresAt); until <= 0 || until > artifact.MaxUploadWindow {
		t.Fatalf("the upload grant expires in %s, want between now and %s", until, artifact.MaxUploadWindow)
	}

	// The run row follows the frame.
	waitFor(t, time.Second, func() bool { return getRun(t, owner, created.ID).Status == "assigned" })
}

// Acceptance: the same (runId, seq) delivered a hundred times leaves one row
// and produces one browser event. Delivery from a daemon is at-least-once, so
// this is the property the whole event pipeline rests on.
func TestARedeliveredEventIsStoredOnceAndSeenOnce(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://dedup.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "discover")
	daemon.receive(t, time.Second) // run.assign

	browser := env.dialBrowser(t, owner, created.ID, "")
	if snapshot := browser.receive(t, 2*time.Second); snapshot.Type != "run.snapshot" {
		t.Fatalf("the stream opened with %q, want run.snapshot", snapshot.Type)
	}

	const redeliveries = 100
	for range redeliveries {
		daemon.send(t, qaschema.EnvelopeTypeRunEvent, &created.ID, 1,
			eventPayload("page_discovered", "found /login"))
	}

	event := browser.nextEvent(t, 3*time.Second)
	if event.Event.Seq != 1 || event.Event.Code != "page_discovered" {
		t.Fatalf("got event %+v, want seq 1 page_discovered", event.Event)
	}
	// Nothing else: 99 redeliveries produced no further frames.
	browser.quiet(t, 400*time.Millisecond)

	events := listEvents(t, owner, created.ID, "")
	if len(events) != 1 {
		t.Fatalf("%d redeliveries stored %d rows, want 1: %+v", redeliveries, len(events), events)
	}
}

// Acceptance: a daemon cut off mid-run reconnects and replays from its own
// buffer; nothing is lost and nothing is duplicated, and a browser resumes
// with ?since.
func TestADaemonReconnectResumesWithoutGapsOrDuplicates(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://resume.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "discover")
	daemon.receive(t, time.Second)

	for seq := int64(1); seq <= 3; seq++ {
		daemon.send(t, qaschema.EnvelopeTypeRunEvent, &created.ID, seq, eventPayload("step", "before the cut"))
	}
	waitFor(t, 2*time.Second, func() bool { return len(listEvents(t, owner, created.ID, "")) == 3 })

	// Cut the connection mid-run, exactly as a laptop lid or a proxy would.
	_ = daemon.conn.CloseNow()

	// The daemon comes back and replays from its last unacked event. There is
	// no resume handshake in contract D on purpose: the (run_id, seq) unique
	// index is what makes an unconditional replay safe.
	reconnected := env.dialDaemon(t, runtimeID, token)
	reconnected.hello(t)
	for seq := int64(2); seq <= 5; seq++ {
		reconnected.send(t, qaschema.EnvelopeTypeRunEvent, &created.ID, seq, eventPayload("step", "after the cut"))
	}
	waitFor(t, 3*time.Second, func() bool { return len(listEvents(t, owner, created.ID, "")) == 5 })

	events := listEvents(t, owner, created.ID, "")
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("the stream has a gap or a duplicate at index %d: %+v", i, events)
		}
	}
	// The replayed 2 and 3 kept the message they were first stored with: a
	// redelivery is a no-op, not an overwrite.
	if events[1].Message != "before the cut" {
		t.Fatalf("a redelivery overwrote seq 2: %+v", events[1])
	}

	// A browser resumes from where it left off and sees only what it missed.
	resumed := env.dialBrowser(t, owner, created.ID, "3")
	if snapshot := resumed.receive(t, 2*time.Second); snapshot.Type != "run.snapshot" {
		t.Fatalf("the resumed stream opened with %q, want run.snapshot", snapshot.Type)
	}
	for want := int64(4); want <= 5; want++ {
		got := resumed.nextEvent(t, 2*time.Second)
		if got.Event.Seq != want {
			t.Fatalf("resumed stream sent seq %d, want %d", got.Event.Seq, want)
		}
	}
	resumed.quiet(t, 300*time.Millisecond)
}

// Acceptance: a runtime token from one organization cannot touch a run in
// another. The org on the wire is never consulted — it comes from the token —
// so this is the test that the derivation actually holds.
func TestARuntimeTokenCannotTouchAnotherOrganizationsRun(t *testing.T) {
	env := newQAEnv(t)

	victim := env.NewOrg(t)
	victimProject := env.project(t, victim, "https://victim.example.com")
	victimRuntime, victimToken := env.pairedRuntime(t, victim)
	victimDaemon := env.dialDaemon(t, victimRuntime, victimToken)
	victimDaemon.hello(t)
	victimRun := startRun(t, victim, victimProject, victimRuntime, "discover")
	victimDaemon.receive(t, time.Second)

	attacker := env.NewOrg(t)
	attackerRuntime, attackerToken := env.pairedRuntime(t, attacker)
	attackerDaemon := env.dialDaemon(t, attackerRuntime, attackerToken)
	attackerDaemon.hello(t)

	// A well-formed frame naming another tenant's run. Contract D has no
	// per-frame error channel, so the rejection is the connection closing.
	attackerDaemon.send(t, qaschema.EnvelopeTypeRunEvent, &victimRun.ID, 1,
		eventPayload("page_discovered", "not mine"))
	attackerDaemon.expectClosed(t, 3*time.Second)

	// Nothing was written to the victim's run.
	if events := listEvents(t, victim, victimRun.ID, ""); len(events) != 0 {
		t.Fatalf("a cross-tenant frame stored %d events: %+v", len(events), events)
	}
	if status := getRun(t, victim, victimRun.ID).Status; status != "assigned" {
		t.Fatalf("a cross-tenant frame moved the run to %q", status)
	}

	// A result frame is refused the same way, and on a fresh connection so the
	// close above is not what is being observed.
	second := env.dialDaemon(t, attackerRuntime, attackerToken)
	second.hello(t)
	second.send(t, qaschema.EnvelopeTypeRunResult, &victimRun.ID, 1,
		qaschema.RunResultPayload{Status: qaschema.RunResultPayloadStatusCompleted})
	second.expectClosed(t, 3*time.Second)

	if status := getRun(t, victim, victimRun.ID).Status; status != "assigned" {
		t.Fatalf("a cross-tenant result finished the run as %q", status)
	}

	// A hello claiming somebody else's runtime is refused too: the token
	// already said which machine this is.
	third := env.dialDaemon(t, attackerRuntime, attackerToken)
	third.send(t, qaschema.EnvelopeTypeHello, nil, 1, qaschema.HelloPayload{
		RuntimeID: victimRuntime.String(),
		Version:   "0.1.0-test",
		Browsers:  []qaschema.HelloPayloadBrowsersItem{qaschema.HelloPayloadBrowsersItemChromium},
		Agents:    []qaschema.AgentCapability{},
	})
	third.expectClosed(t, 3*time.Second)
}

// Acceptance: cancelling a run makes it `cancelled` and the assigned daemon
// receives run.cancel.
func TestCancelReachesTheAssignedDaemon(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://cancelled.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "discover")
	daemon.receive(t, time.Second) // run.assign

	resp := owner.Post(t, "/api/v1/runs/"+created.ID.String()+"/cancel", nil)
	resp.ExpectStatus(t, http.StatusOK)
	var cancelled runView
	resp.JSON(t, &cancelled)
	if cancelled.Status != "cancelled" {
		t.Fatalf("got status %q, want cancelled", cancelled.Status)
	}

	frame := daemon.receive(t, 2*time.Second)
	if frame.Type != qaschema.EnvelopeTypeRunCancel {
		t.Fatalf("got a %s frame, want run.cancel", frame.Type)
	}
	if frame.RunID == nil || *frame.RunID != created.ID.String() {
		t.Fatalf("run.cancel is for %v, want %s", frame.RunID, created.ID)
	}

	var payload qaschema.RunCancelPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("decode run.cancel payload: %v", err)
	}
	if payload.Reason != qaschema.RunCancelPayloadReasonUserRequested {
		t.Fatalf("got reason %q, want user_requested", payload.Reason)
	}
}

// A result frame becomes the report: executions with their steps, assertions
// and evidence, and the findings that cite them.
func TestRunResultBuildsTheReport(t *testing.T) {
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

	assign := daemon.receive(t, time.Second)
	var assignPayload struct {
		TestCases []json.RawMessage `json:"testCases"`
	}
	if err := json.Unmarshal(assign.Payload, &assignPayload); err != nil {
		t.Fatalf("decode run.assign: %v", err)
	}
	// The approved case travels with the assignment, so the daemon never has
	// to call back into the API for its work list.
	if len(assignPayload.TestCases) != 1 {
		t.Fatalf("run.assign carried %d test cases, want 1", len(assignPayload.TestCases))
	}
	if err := qaschema.MustBeValid("test-case@1", assignPayload.TestCases[0]); err != nil {
		t.Fatalf("the assigned test case is not a valid test-case@1 document: %v", err)
	}

	key, err := artifact.ObjectKey(owner.OrgID, created.ID, created.CreatedAt, "TC-001", "shot.png")
	if err != nil {
		t.Fatalf("build artifact key: %v", err)
	}

	daemon.send(t, qaschema.EnvelopeTypeRunResult, &created.ID, 1, qaschema.RunResultPayload{
		Status: qaschema.RunResultPayloadStatusCompleted,
		Executions: []qaschema.ExecutionResult{{
			Version: 1, TestCaseID: "TC-001", Result: qaschema.OutcomeFail,
			FailureClass: failureClass(qaschema.FailureClassPRODUCTBUG),
			Message:      strp("the save button did nothing"),
			Steps: []qaschema.StepResult{
				{Index: 0, Action: qaschema.StepActionNavigate, Status: qaschema.OutcomePass},
				{Index: 1, Action: qaschema.StepActionClick, Status: qaschema.OutcomeFail,
					Message: strp("timed out waiting for /dashboard")},
			},
			Assertions: []qaschema.AssertionResult{{
				Index: 0, Type: qaschema.AssertionTypeURLMatches, Status: qaschema.OutcomeFail,
				Expected: strp("/dashboard"), Actual: strp("/login"),
			}},
			Artifacts: []qaschema.Artifact{{ID: "a1", Kind: qaschema.ArtifactKindScreenshot, Key: key}},
			StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			EndedAt:   time.Now().UTC().Format(time.RFC3339),
		}},
		Findings: []qaschema.Finding{{
			Version: 1, TestCaseID: "TC-001", StepIndex: intp(1),
			FailureClass: qaschema.FailureClassPRODUCTBUG,
			Summary:      strp("POST /api/session returned 500"),
			RootCause:    "the backend returned an internal server error",
			Confidence:   0.94,
			Evidence:     []qaschema.ArtifactID{"a1"},
		}},
	})

	waitFor(t, 5*time.Second, func() bool { return getRun(t, owner, created.ID).Status == "failed" })

	final := getRun(t, owner, created.ID)
	// The verdict is derived here from the executions that landed, not taken
	// from the daemon: "completed" means the harness finished, not that the
	// application under test passed.
	if final.Counters.Total != 1 || final.Counters.Failed != 1 {
		t.Fatalf("got counters %+v, want one execution and one failure", final.Counters)
	}

	resp := owner.Get(t, "/api/v1/runs/"+created.ID.String()+"/report")
	resp.ExpectStatus(t, http.StatusOK)

	var report struct {
		Run        runView `json:"run"`
		Executions []struct {
			TestCaseRef  string `json:"testCaseRef"`
			Result       string `json:"result"`
			FailureClass string `json:"failureClass"`
			Steps        []struct {
				Index  int    `json:"index"`
				Action string `json:"action"`
				Result string `json:"result"`
			} `json:"steps"`
			Assertions []struct {
				Expected string `json:"expected"`
				Actual   string `json:"actual"`
			} `json:"assertions"`
			Artifacts []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"artifacts"`
		} `json:"executions"`
		Findings []struct {
			FailureClass string  `json:"failureClass"`
			Confidence   float64 `json:"confidence"`
			Evidence     []struct {
				Name string `json:"name"`
			} `json:"evidence"`
		} `json:"findings"`
	}
	resp.JSON(t, &report)

	if len(report.Executions) != 1 {
		t.Fatalf("the report has %d executions, want 1", len(report.Executions))
	}
	execution := report.Executions[0]
	if execution.TestCaseRef != "TC-001" || execution.Result != "fail" || execution.FailureClass != "PRODUCT_BUG" {
		t.Fatalf("got execution %+v, want a failed TC-001 classified PRODUCT_BUG", execution)
	}
	if len(execution.Steps) != 2 || execution.Steps[1].Result != "fail" {
		t.Fatalf("got steps %+v, want two with the second failing", execution.Steps)
	}
	// The analyst cannot classify a failure without the expected/actual pair,
	// so it has to survive ingest.
	if len(execution.Assertions) != 1 || execution.Assertions[0].Actual != "/login" {
		t.Fatalf("got assertions %+v, want the observed value preserved", execution.Assertions)
	}
	if len(execution.Artifacts) != 1 || execution.Artifacts[0].Name != "shot.png" {
		t.Fatalf("got artifacts %+v, want shot.png", execution.Artifacts)
	}
	if execution.Artifacts[0].URL == "" {
		t.Fatal("the report carries no download url for its evidence")
	}

	if len(report.Findings) != 1 {
		t.Fatalf("the report has %d findings, want 1", len(report.Findings))
	}
	// The finding cites the artifact by the daemon's run-local handle, which
	// has to be resolved to the row it became.
	if len(report.Findings[0].Evidence) != 1 || report.Findings[0].Evidence[0].Name != "shot.png" {
		t.Fatalf("got evidence %+v, want the screenshot the finding cited", report.Findings[0].Evidence)
	}
}

// Acceptance: the presigned URL a run is granted can only write inside that
// run's own prefix.
func TestPresignEndpointIsBoundToTheRunsOwnPrefix(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://presign.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "discover")
	daemon.receive(t, time.Second)

	// The daemon authenticates with its runtime token, not a session.
	client := env.Anonymous(t).WithBearer(token)
	path := "/api/v1/runs/" + created.ID.String() + "/artifacts/presign"

	ownKey, err := artifact.ObjectKey(owner.OrgID, created.ID, created.CreatedAt, "TC-001", "trace.zip")
	if err != nil {
		t.Fatalf("build key: %v", err)
	}
	resp := client.Post(t, path, map[string]string{"key": ownKey, "contentType": "application/zip"})
	resp.ExpectStatus(t, http.StatusCreated)

	var signed struct {
		URL, Key, Method string
	}
	resp.JSON(t, &signed)
	if signed.Key != ownKey || signed.Method != http.MethodPut {
		t.Fatalf("got %+v, want a PUT for %s", signed, ownKey)
	}

	t.Run("a key outside the run's prefix is refused", func(t *testing.T) {
		for name, key := range map[string]string{
			"another run": artifact.KeyPrefix(owner.OrgID, uuid.New(), created.CreatedAt) + "trace.zip",
			"another org": artifact.KeyPrefix(uuid.New(), created.ID, created.CreatedAt) + "trace.zip",
			"a traversal": artifact.KeyPrefix(owner.OrgID, created.ID, created.CreatedAt) + "../escaped.zip",
		} {
			t.Run(name, func(t *testing.T) {
				client.Post(t, path, map[string]string{"key": key}).
					ExpectError(t, http.StatusForbidden, "forbidden")
			})
		}
	})

	t.Run("a session cannot mint an upload url", func(t *testing.T) {
		owner.Post(t, path, map[string]string{"key": ownKey}).
			ExpectError(t, http.StatusUnauthorized, "unauthorized")
	})

	t.Run("another runtime's token cannot mint one for this run", func(t *testing.T) {
		stranger := env.NewOrg(t)
		_, strangerToken := env.pairedRuntime(t, stranger)
		env.Anonymous(t).WithBearer(strangerToken).
			Post(t, path, map[string]string{"key": ownKey}).
			ExpectError(t, http.StatusNotFound, "not_found")
	})

	t.Run("the window closes with the run", func(t *testing.T) {
		owner.Post(t, "/api/v1/runs/"+created.ID.String()+"/cancel", nil).ExpectStatus(t, http.StatusOK)
		client.Post(t, path, map[string]string{"key": ownKey}).
			ExpectError(t, http.StatusConflict, "conflict")
	})
}

// A runtime that stops reporting does not leave a run in limbo: past the
// liveness window the run is finished with a reason a user can read.
func TestARunWhoseRuntimeStopsReportingFinishesAsAnError(t *testing.T) {
	env := newQAEnv(t, func(cfg *config) {
		// The production window is 30 seconds; the behaviour under test is the
		// same at 200 milliseconds and the test does not have to wait for it.
		cfg.Run.OnlineWithin = 200 * time.Millisecond
		cfg.Scheduler = runpkg.SchedulerConfig{Poll: 50 * time.Millisecond, Sweep: 50 * time.Millisecond}
	})
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "https://lost.example.com")
	runtimeID, token := env.pairedRuntime(t, owner)

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)
	created := startRun(t, owner, projectID, runtimeID, "discover")
	daemon.receive(t, time.Second)

	// The daemon vanishes without a close frame, the way a killed process does.
	_ = daemon.conn.CloseNow()

	waitFor(t, 5*time.Second, func() bool { return getRun(t, owner, created.ID).Status == "error" })

	final := getRun(t, owner, created.ID)
	if final.Error == nil || final.Error.Code != "runtime_lost" {
		t.Fatalf("got error %+v, want a runtime_lost reason", final.Error)
	}
	// A domain message, not a driver one.
	if final.Error.Message == "" {
		t.Fatal("the failed run carries no explanation")
	}
}

func strp(s string) *string                                       { return &s }
func intp(i int) *int                                             { return &i }
func failureClass(c qaschema.FailureClass) *qaschema.FailureClass { return &c }
