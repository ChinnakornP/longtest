package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// The planner, end to end over the real control plane: a plan arrives as a
// run.result frame, is checked against what the project actually has, and
// becomes draft test cases — or becomes nothing at all.
//
// These are the acceptance criteria of LONG-16. Every one of them is about the
// boundary between "an AI wrote this" and "this is a row in our database", so
// none of them can be tested below the socket.

// goldenDir is internal/testcase's testdata, reused rather than copied.
//
// The fixture app's application map and the plan written against it are one
// pair: a copy here would drift from the one the unit tests assert on, and the
// first thing to break would be the property this file exists to prove.
const goldenDir = "../../internal/testcase/testdata"

func goldenJSON(t *testing.T, name, schemaID string) json.RawMessage {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := qaschema.MustBeValid(schemaID, raw); err != nil {
		t.Fatalf("%s is not a valid %s: %v", name, schemaID, err)
	}
	return raw
}

// planningEnv is a project with an application map, two registered fixtures
// and a daemon online: everything a planning run needs and nothing it does not.
type planningEnv struct {
	*qaEnv
	owner     *authtest.Client
	projectID uuid.UUID
	runtimeID uuid.UUID
	daemon    *daemonClient
}

func newPlanningEnv(t *testing.T) *planningEnv {
	t.Helper()

	env := newQAEnv(t)
	owner := env.NewOrg(t)
	// The base URL matches the golden map's, so a navigate step in the plan is
	// a navigate step against this project.
	projectID := env.project(t, owner, "http://127.0.0.1:4173")
	runtimeID, token := env.pairedRuntime(t, owner)

	for _, fixture := range []string{"logged_in_as_admin", "seeded_employee"} {
		owner.Post(t, "/api/v1/projects/"+projectID.String()+"/fixtures",
			map[string]string{"name": fixture, "description": "test fixture"}).
			ExpectStatus(t, http.StatusCreated)
	}

	daemon := env.dialDaemon(t, runtimeID, token)
	daemon.hello(t)

	p := &planningEnv{qaEnv: env, owner: owner, projectID: projectID, runtimeID: runtimeID, daemon: daemon}
	p.discover(t)
	return p
}

// discover runs a discover run whose result is the golden application map, so
// the project has the element refs a plan is checked against.
func (p *planningEnv) discover(t *testing.T) {
	t.Helper()

	created := startRun(t, p.owner, p.projectID, p.runtimeID, "discover")
	p.daemon.receive(t, time.Second) // run.assign
	p.daemon.send(t, qaschema.EnvelopeTypeRunResult, &created.ID, p.daemon.next(), map[string]any{
		"status": "completed",
		"appMap": goldenJSON(t, "fixture-app-appmap.json", "application-map@1"),
	})
	waitFor(t, 5*time.Second, func() bool {
		return getRun(t, p.owner, created.ID).Status == "passed"
	})
}

// plan runs a plan run whose result is the given test-plan@1 document, and
// returns the finished run.
func (p *planningEnv) plan(t *testing.T, document json.RawMessage) runView {
	t.Helper()

	created := startRun(t, p.owner, p.projectID, p.runtimeID, "plan")
	p.daemon.receive(t, time.Second) // run.assign
	p.daemon.send(t, qaschema.EnvelopeTypeRunResult, &created.ID, p.daemon.next(), map[string]any{
		"status":   "completed",
		"testPlan": document,
	})
	waitFor(t, 5*time.Second, func() bool {
		return terminalRunStatus(getRun(t, p.owner, created.ID).Status)
	})
	return getRun(t, p.owner, created.ID)
}

func terminalRunStatus(status string) bool {
	switch status {
	case "passed", "failed", "error", "cancelled":
		return true
	default:
		return false
	}
}

type testCaseView struct {
	ID       uuid.UUID `json:"id"`
	Ref      string    `json:"ref"`
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	Priority string    `json:"priority"`
	Category string    `json:"category"`
	Version  int       `json:"version"`
}

func (p *planningEnv) testCases(t *testing.T) []testCaseView {
	t.Helper()

	resp := p.owner.Get(t, "/api/v1/projects/"+p.projectID.String()+"/test-cases?limit=100")
	resp.ExpectStatus(t, http.StatusOK)

	var body struct {
		TestCases []testCaseView `json:"testCases"`
		Total     int64          `json:"total"`
	}
	resp.JSON(t, &body)
	return body.TestCases
}

func (p *planningEnv) approve(t *testing.T, refs ...string) {
	t.Helper()

	byRef := map[string]uuid.UUID{}
	for _, tc := range p.testCases(t) {
		byRef[tc.Ref] = tc.ID
	}
	for _, ref := range refs {
		id, ok := byRef[ref]
		if !ok {
			t.Fatalf("no test case %s to approve", ref)
		}
		p.owner.Do(t, http.MethodPatch, "/api/v1/test-cases/"+id.String(),
			map[string]string{"status": "approved"}).ExpectStatus(t, http.StatusOK)
	}
}

// The planner cannot be held to a fixture list it was never shown, so the
// project's registered names travel with the assignment.
//
// Names only, and the frame is validated against daemon-envelope@1 on the way
// out: a property that could carry a value would have to exist in the contract
// first, and it does not.
func TestAPlanningAssignmentCarriesTheFixtureNames(t *testing.T) {
	env := newPlanningEnv(t)

	startRun(t, env.owner, env.projectID, env.runtimeID, "plan")
	assign := env.daemon.receive(t, time.Second)

	var payload struct {
		Fixtures []string `json:"fixtures"`
	}
	if err := json.Unmarshal(assign.Payload, &payload); err != nil {
		t.Fatalf("decode run.assign: %v", err)
	}
	got := map[string]bool{}
	for _, name := range payload.Fixtures {
		got[name] = true
	}
	if !got["logged_in_as_admin"] || !got["seeded_employee"] {
		t.Fatalf("run.assign carried fixtures %v, want the two this project registered", payload.Fixtures)
	}

	// A discover run writes no test case, so it is told nothing about logins.
	startRun(t, env.owner, env.projectID, env.runtimeID, "discover")
}

// Acceptance: a plan written against the fixture app's map becomes draft test
// cases covering all five categories, and every one of them is stored with the
// exact bytes the planner wrote.
func TestAPlanBecomesDraftTestCases(t *testing.T) {
	env := newPlanningEnv(t)
	plan := goldenJSON(t, "fixture-app-plan.json", "test-plan@1")

	finished := env.plan(t, plan)
	if finished.Status != "passed" {
		t.Fatalf("the planning run finished as %q (%+v)", finished.Status, finished.Error)
	}

	stored := env.testCases(t)
	var planned struct {
		TestCases []json.RawMessage `json:"testCases"`
	}
	if err := json.Unmarshal(plan, &planned); err != nil {
		t.Fatalf("decode the golden plan: %v", err)
	}
	if len(stored) != len(planned.TestCases) {
		t.Fatalf("stored %d cases, the plan had %d", len(stored), len(planned.TestCases))
	}

	categories := map[string]int{}
	for _, tc := range stored {
		if tc.Status != "draft" {
			t.Errorf("%s was stored as %q; a planned case is a draft until a human says otherwise", tc.Ref, tc.Status)
		}
		categories[tc.Category]++
	}
	for _, category := range qaschema.TestCaseCategoryValues {
		if categories[string(category)] == 0 {
			t.Errorf("nothing was stored in category %q", category)
		}
	}

	// The run says what it did, on the stream a browser is watching.
	if !hasEventCode(listEvents(t, env.owner, finished.ID, ""), "plan_stored") {
		t.Error("the run stream carries no plan_stored event")
	}
}

// Acceptance: a plan naming an element ref the project's map does not carry is
// rejected whole, no part of it reaches the database, and the run says why.
//
// The ref check is what makes TARGET_NOT_FOUND unreachable for a stored case:
// a target the map cannot resolve never becomes a row, so it never becomes a
// failed execution against a customer's application hours later.
func TestAPlanWithAnUnknownRefStoresNothing(t *testing.T) {
	env := newPlanningEnv(t)

	var plan map[string]any
	if err := json.Unmarshal(goldenJSON(t, "fixture-app-plan.json", "test-plan@1"), &plan); err != nil {
		t.Fatalf("decode the golden plan: %v", err)
	}
	// One invented ref, in the last case of an otherwise perfect plan.
	cases := plan["testCases"].([]any)
	last := cases[len(cases)-1].(map[string]any)
	last["steps"] = []any{map[string]any{
		"action": "click", "target": map[string]any{"ref": "emp.btn.archive"},
	}}
	broken, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	finished := env.plan(t, broken)
	if finished.Status != "error" {
		t.Fatalf("a rejected plan finished the run as %q", finished.Status)
	}
	if finished.Error == nil || finished.Error.Code != string(qaschema.RunErrorCodeAgentOutputInvalid) {
		t.Fatalf("got error %+v, want %s", finished.Error, qaschema.RunErrorCodeAgentOutputInvalid)
	}

	// Not one case, not even the nine that were fine. A suite silently missing
	// the case that would have caught the bug reads exactly like a suite that
	// never had it.
	if stored := env.testCases(t); len(stored) != 0 {
		t.Fatalf("a rejected plan left %d cases behind", len(stored))
	}

	events := listEvents(t, env.owner, finished.ID, "")
	if !hasEventCode(events, "plan_rejected") {
		t.Fatal("the run stream does not say the plan was rejected")
	}
	// The rule, not the model's words: a rejection detail quotes the document
	// the model wrote, and on a hijacked run that is page content.
	for _, event := range events {
		if event.Code == "plan_rejected" && event.Message == "" {
			t.Fatal("the plan_rejected event carries no reason")
		}
	}
}

// Acceptance: re-planning a project does not put cases a human already
// approved back into the review queue.
//
// The dedupe is on behaviour, not on the id: this test re-plans with cases
// renamed and renumbered, which is what a second planning run actually
// produces.
func TestRePlanningDoesNotDuplicateApprovedCases(t *testing.T) {
	env := newPlanningEnv(t)
	plan := goldenJSON(t, "fixture-app-plan.json", "test-plan@1")

	env.plan(t, plan)
	env.approve(t, "TC-001", "TC-002", "TC-003")

	before := env.testCases(t)
	approvedVersions := map[string]int{}
	for _, tc := range before {
		if tc.Status == "approved" {
			approvedVersions[tc.Ref] = tc.Version
		}
	}
	if len(approvedVersions) != 3 {
		t.Fatalf("approved %d cases, want 3", len(approvedVersions))
	}

	finished := env.plan(t, renumbered(t, plan))
	if finished.Status != "passed" {
		t.Fatalf("the second planning run finished as %q (%+v)", finished.Status, finished.Error)
	}

	after := env.testCases(t)
	if len(after) != len(before) {
		var refs []string
		for _, tc := range after {
			refs = append(refs, tc.Ref)
		}
		t.Fatalf("a re-plan grew the suite from %d to %d cases: %v", len(before), len(after), refs)
	}

	for _, tc := range after {
		want, wasApproved := approvedVersions[tc.Ref]
		if !wasApproved {
			continue
		}
		if tc.Status != "approved" {
			t.Errorf("%s is %q after a re-plan; approval is not something the planner may undo", tc.Ref, tc.Status)
		}
		if tc.Version != want {
			t.Errorf("%s went from version %d to %d; an approved case must not be rewritten", tc.Ref, want, tc.Version)
		}
	}
}

// renumbered is the golden plan as a second run would write it: same
// behaviour, different ids and names.
func renumbered(t *testing.T, plan json.RawMessage) json.RawMessage {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(plan, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, entry := range decoded["testCases"].([]any) {
		tc := entry.(map[string]any)
		tc["id"] = "TC-9" + string(rune('0'+i%10)) + string(rune('0'+i/10))
		tc["name"] = "Rewritten: " + tc["name"].(string)
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// Acceptance: an approved case is what the next execute run runs.
func TestApprovedCasesAreExecutedNext(t *testing.T) {
	env := newPlanningEnv(t)
	env.plan(t, goldenJSON(t, "fixture-app-plan.json", "test-plan@1"))
	env.approve(t, "TC-002", "TC-007")

	created := startRun(t, env.owner, env.projectID, env.runtimeID, "execute")
	assign := env.daemon.receive(t, time.Second)

	var payload struct {
		TestCases []json.RawMessage `json:"testCases"`
	}
	if err := json.Unmarshal(assign.Payload, &payload); err != nil {
		t.Fatalf("decode run.assign: %v", err)
	}
	if len(payload.TestCases) != 2 {
		t.Fatalf("run.assign carried %d cases, want the 2 that were approved", len(payload.TestCases))
	}

	refs := map[string]bool{}
	for _, document := range payload.TestCases {
		if err := qaschema.MustBeValid("test-case@1", document); err != nil {
			t.Fatalf("an assigned case is not a valid test-case@1: %v", err)
		}
		var header struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(document, &header); err != nil {
			t.Fatalf("decode an assigned case: %v", err)
		}
		refs[header.ID] = true
	}
	if !refs["TC-002"] || !refs["TC-007"] {
		t.Fatalf("the assignment carried %v, want TC-002 and TC-007", refs)
	}
	if created.Mode != "execute" {
		t.Fatalf("mode = %q", created.Mode)
	}
}

// Acceptance: the coverage endpoint points at the workflow the fixture app was
// given no test for.
func TestCoverageEndpointFindsTheGap(t *testing.T) {
	env := newPlanningEnv(t)
	env.plan(t, goldenJSON(t, "fixture-app-plan.json", "test-plan@1"))
	// Only approved cases count as coverage: a draft nobody has read is not a
	// test this project runs.
	env.approve(t, "TC-001", "TC-002", "TC-003", "TC-006", "TC-007", "TC-009")

	resp := env.owner.Get(t, "/api/v1/projects/"+env.projectID.String()+"/coverage")
	resp.ExpectStatus(t, http.StatusOK)

	var report struct {
		ApprovedCases int `json:"approvedCases"`
		Workflows     []struct {
			Ref            string `json:"ref"`
			Status         string `json:"status"`
			Risk           string `json:"risk"`
			SuggestedTests int    `json:"suggestedTests"`
			Suggestions    []struct {
				Category string `json:"category"`
				Reason   string `json:"reason"`
			} `json:"suggestions"`
		} `json:"workflows"`
		Categories []struct {
			Category       string `json:"category"`
			Approved       int    `json:"approved"`
			SuggestedTests int    `json:"suggestedTests"`
		} `json:"categories"`
		SuggestedTestCount int    `json:"suggestedTestCount"`
		Summary            string `json:"summary"`
	}
	resp.JSON(t, &report)

	if report.ApprovedCases != 6 {
		t.Fatalf("the report counts %d approved cases, want 6", report.ApprovedCases)
	}

	var edit struct {
		found          bool
		status, risk   string
		suggestedTests int
		reasons        []string
	}
	for _, workflow := range report.Workflows {
		if workflow.Ref != "wf.edit_employee" {
			continue
		}
		edit.found, edit.status, edit.risk = true, workflow.Status, workflow.Risk
		edit.suggestedTests = workflow.SuggestedTests
		for _, suggestion := range workflow.Suggestions {
			edit.reasons = append(edit.reasons, suggestion.Category+": "+suggestion.Reason)
		}
	}
	if !edit.found {
		t.Fatal("the report does not mention wf.edit_employee")
	}
	if edit.status != "uncovered" {
		t.Errorf("wf.edit_employee is %q, want uncovered: nothing in the suite opens the edit form", edit.status)
	}
	if edit.risk != "high" {
		t.Errorf("an untested workflow behind a login is %q risk", edit.risk)
	}
	if edit.suggestedTests == 0 || len(edit.reasons) == 0 {
		t.Errorf("the gap comes with %d suggestions and no reasons", edit.suggestedTests)
	}

	// ui_behavior has an approved case (TC-007); error_handling has one too
	// (TC-009). Neither should be reported as an empty category.
	for _, category := range report.Categories {
		if category.Category == "ui_behavior" && category.Approved == 0 {
			t.Error("ui_behavior is reported empty despite TC-007 being approved")
		}
	}
	if report.SuggestedTestCount == 0 {
		t.Error("a suite with an untested workflow suggested nothing")
	}
	if report.Summary == "" {
		t.Error("the report has no summary")
	}
}

// A precondition naming a fixture the project has not registered is a
// whole-plan rejection, not a case that is quietly stored and fails to start
// weeks later.
func TestAPlanNamingAnUnregisteredFixtureIsRejected(t *testing.T) {
	env := newPlanningEnv(t)

	env.owner.Do(t, http.MethodDelete,
		"/api/v1/projects/"+env.projectID.String()+"/fixtures/seeded_employee", nil).
		ExpectStatus(t, http.StatusNoContent)

	finished := env.plan(t, goldenJSON(t, "fixture-app-plan.json", "test-plan@1"))
	if finished.Status != "error" {
		t.Fatalf("a plan naming an unregistered fixture finished as %q", finished.Status)
	}
	if stored := env.testCases(t); len(stored) != 0 {
		t.Fatalf("a rejected plan left %d cases behind", len(stored))
	}
}

// The fixture registry holds names. There is no request body that carries a
// credential into it, and no response that carries one out, because there is
// no column that holds one.
func TestFixtureRegistryHoldsNamesOnly(t *testing.T) {
	env := newQAEnv(t)
	owner := env.NewOrg(t)
	projectID := env.project(t, owner, "http://127.0.0.1:4173")
	path := "/api/v1/projects/" + projectID.String() + "/fixtures"

	// Registering twice is a retry, not a conflict.
	for i := 0; i < 2; i++ {
		owner.Post(t, path, map[string]string{
			"name": "logged_in_as_admin", "description": "an administrator session",
		}).ExpectStatus(t, http.StatusCreated)
	}

	resp := owner.Get(t, path)
	resp.ExpectStatus(t, http.StatusOK)
	var body struct {
		Fixtures []struct {
			Name        string `json:"name"`
			Reference   string `json:"reference"`
			Description string `json:"description"`
		} `json:"fixtures"`
	}
	resp.JSON(t, &body)

	if len(body.Fixtures) != 1 {
		t.Fatalf("registering the same name twice produced %d fixtures", len(body.Fixtures))
	}
	if body.Fixtures[0].Reference != "fixture:logged_in_as_admin" {
		t.Fatalf("reference = %q", body.Fixtures[0].Reference)
	}

	// A name that a planner could not legally emit is refused here too, so the
	// registry and the contract cannot disagree about what a fixture is.
	for _, bad := range []string{"Logged_In", "9lives", "has spaces", "has-dashes", ""} {
		owner.Post(t, path, map[string]string{"name": bad}).
			ExpectStatus(t, http.StatusUnprocessableEntity)
	}

	owner.Do(t, http.MethodDelete, path+"/logged_in_as_admin", nil).
		ExpectStatus(t, http.StatusNoContent)
	owner.Do(t, http.MethodDelete, path+"/logged_in_as_admin", nil).
		ExpectStatus(t, http.StatusNotFound)
}

// The daemon's delivery is at-least-once, so the same result frame arrives
// twice whenever a daemon reconnects mid-acknowledgement. The second delivery
// must change nothing a reader can see: not the suite, and not the stream.
func TestARedeliveredPlanChangesNothing(t *testing.T) {
	env := newPlanningEnv(t)
	plan := goldenJSON(t, "fixture-app-plan.json", "test-plan@1")

	created := startRun(t, env.owner, env.projectID, env.runtimeID, "plan")
	env.daemon.receive(t, time.Second) // run.assign

	result := map[string]any{"status": "completed", "testPlan": plan}
	env.daemon.send(t, qaschema.EnvelopeTypeRunResult, &created.ID, env.daemon.next(), result)
	waitFor(t, 5*time.Second, func() bool {
		return terminalRunStatus(getRun(t, env.owner, created.ID).Status)
	})

	before := env.testCases(t)
	beforeEvents := listEvents(t, env.owner, created.ID, "")

	// The same frame again, on the same connection, exactly as a redelivery
	// would arrive.
	env.daemon.send(t, qaschema.EnvelopeTypeRunResult, &created.ID, env.daemon.next(), result)
	time.Sleep(300 * time.Millisecond)

	after := env.testCases(t)
	if len(after) != len(before) {
		t.Fatalf("a redelivered plan grew the suite from %d to %d cases", len(before), len(after))
	}
	for i, tc := range after {
		if tc.Version != before[i].Version {
			t.Errorf("%s went from version %d to %d on a redelivery", tc.Ref, before[i].Version, tc.Version)
		}
	}

	afterEvents := listEvents(t, env.owner, created.ID, "")
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("a redelivery added %d events to the stream", len(afterEvents)-len(beforeEvents))
	}
}

func hasEventCode(events []eventView, code string) bool {
	for _, event := range events {
		if event.Code == code {
			return true
		}
	}
	return false
}
