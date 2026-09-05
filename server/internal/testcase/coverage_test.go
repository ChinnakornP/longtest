package testcase

import (
	"encoding/json"
	"testing"

	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// approvedGolden turns the golden plan into an approved suite, so the coverage
// report is computed over exactly the cases the plan test accepted.
func approvedGolden(t *testing.T, skip ...string) []ApprovedCase {
	t.Helper()

	skipped := map[string]struct{}{}
	for _, ref := range skip {
		skipped[ref] = struct{}{}
	}

	var out []ApprovedCase
	for _, document := range plannedDocuments(t, goldenPlan(t)) {
		var decoded qaschema.TestCase
		if err := json.Unmarshal(document, &decoded); err != nil {
			t.Fatalf("decode a planned case: %v", err)
		}
		if _, drop := skipped[decoded.ID]; drop {
			continue
		}
		out = append(out, ApprovedCase{Ref: decoded.ID, Case: decoded})
	}
	return out
}

func goldenCoverage(t *testing.T, skip ...string) CoverageReport {
	t.Helper()

	approved := approvedGolden(t, skip...)
	counts := map[qaschema.TestCaseCategory]int{}
	for _, a := range approved {
		counts[a.Case.Category]++
	}
	return Coverage(CoverageInput{Map: goldenMap(t), Approved: approved, CategoryCounts: counts})
}

func workflowLine(t *testing.T, report CoverageReport, ref string) WorkflowCoverage {
	t.Helper()
	for _, line := range report.Workflows {
		if line.Ref == ref {
			return line
		}
	}
	t.Fatalf("the report has no workflow %s", ref)
	return WorkflowCoverage{}
}

// The acceptance criterion: on a fixture app with a hole left in it on
// purpose, the report points at the hole.
//
// The golden plan covers creating and searching employees and leaves deleting
// one untested — it is the destructive workflow and the plan says so in its own
// coverageNotes. Nothing in this function knows that; it is derived from the
// map and the suite.
func TestCoverageFindsTheDeliberateGap(t *testing.T) {
	report := goldenCoverage(t)

	// Nothing opens the edit form at all: not the page, not one of its
	// fields. Uncovered, behind a login, so it is the report's top item.
	edited := workflowLine(t, report, "wf.edit_employee")
	if edited.Status != CoverageUncovered {
		t.Fatalf("wf.edit_employee is %s, want %s", edited.Status, CoverageUncovered)
	}
	if edited.Risk != RiskHigh {
		t.Errorf("an untouched workflow behind a login is %s risk, want %s", edited.Risk, RiskHigh)
	}
	if edited.SuggestedTests == 0 {
		t.Error("an uncovered workflow suggested no tests")
	}
	if len(edited.CoveringCaseRefs) != 0 {
		t.Errorf("wf.edit_employee reports covering cases %v", edited.CoveringCaseRefs)
	}
	// An uncovered workflow that submits a form is worth a validation case as
	// well as a functional one, and being behind a login is worth an
	// error_handling one. The count is the suggestions, not a guess.
	if edited.SuggestedTests != len(edited.Suggestions) {
		t.Errorf("suggestedTests %d does not match %d suggestions", edited.SuggestedTests, len(edited.Suggestions))
	}

	// Deleting is reached as far as the list and the table and stops there:
	// partial, with the control nobody clicks named. Saying which step is
	// missing is the difference between a report and a scolding.
	deleted := workflowLine(t, report, "wf.delete_employee")
	if deleted.Status != CoveragePartial {
		t.Fatalf("wf.delete_employee is %s, want %s", deleted.Status, CoveragePartial)
	}
	if !contains(deleted.MissingRefs, "emp.delete") {
		t.Errorf("missingRefs %v does not name emp.delete", deleted.MissingRefs)
	}
	if deleted.SuggestedTests == 0 {
		t.Error("a partially covered workflow suggested no tests")
	}

	created := workflowLine(t, report, "wf.create_employee")
	if created.Status != CoverageCovered {
		t.Fatalf("wf.create_employee is %s, want %s: TC-002 walks the whole path", created.Status, CoverageCovered)
	}
	if !contains(created.CoveringCaseRefs, "TC-002") {
		t.Errorf("wf.create_employee is covered by %v, want TC-002 among them", created.CoveringCaseRefs)
	}
	if created.SuggestedTests != 0 {
		t.Errorf("a covered workflow suggested %d tests", created.SuggestedTests)
	}

	searched := workflowLine(t, report, "wf.search_employees")
	if searched.Status != CoverageCovered {
		t.Errorf("wf.search_employees is %s, want %s", searched.Status, CoverageCovered)
	}

	// Sign-in is only walked as far as its failure path: TC-009 fills the form
	// and is refused, so it never reaches the employee list. Partial, not
	// covered — and that distinction is the point, because "somebody started
	// this and stopped" is a different backlog item from "nobody has been
	// here".
	signIn := workflowLine(t, report, "wf.sign_in")
	if signIn.Status != CoveragePartial {
		t.Fatalf("wf.sign_in is %s, want %s", signIn.Status, CoveragePartial)
	}
	if signIn.Ratio <= 0 || signIn.Ratio >= 1 {
		t.Errorf("a partial workflow has ratio %v, want strictly between 0 and 1", signIn.Ratio)
	}
	if !contains(signIn.MissingRefs, "page.employees") {
		t.Errorf("missingRefs %v does not name page.employees", signIn.MissingRefs)
	}

	if report.SuggestedTestCount == 0 {
		t.Error("a suite with an uncovered workflow suggested nothing")
	}
	if report.Summary == "" {
		t.Error("the report has no summary")
	}
}

// A workflow the union of several cases walks, but no single case does, is
// partial. Three cases doing a third each is not the same as anyone having
// tested the workflow.
func TestCoverageDoesNotUnionAcrossCases(t *testing.T) {
	appMap := goldenMap(t)
	split := []ApprovedCase{
		{Ref: "TC-A", Case: caseTouching(t, "page.employees", "emp.row")},
		{Ref: "TC-B", Case: caseTouching(t, "emp.delete", "emp.table")},
	}
	report := Coverage(CoverageInput{Map: appMap, Approved: split})

	line := workflowLine(t, report, "wf.delete_employee")
	if line.Status == CoverageCovered {
		t.Fatal("two cases walking half the path each were reported as covering it")
	}
	if line.Status != CoveragePartial {
		t.Fatalf("got %s, want %s", line.Status, CoveragePartial)
	}
}

// A raw locator contributes no coverage: the map cannot say what it resolves
// to, and counting an invented selector is how a suite is reported complete
// because a model guessed at a CSS class.
func TestUnstableLocatorsDoNotCount(t *testing.T) {
	locator := "table[data-testid=employee-table] tr"
	unstable := true
	tc := qaschema.TestCase{
		Version: 1, ID: "TC-L", Name: "n", Priority: "low", Category: "functional",
		Steps: []qaschema.Step{{
			Action: qaschema.StepActionClick,
			Target: &qaschema.Target{Locator: &locator, Unstable: &unstable},
		}},
		Assertions: []qaschema.Assertion{{Type: qaschema.AssertionTypeNoConsoleError}},
	}
	report := Coverage(CoverageInput{Map: goldenMap(t), Approved: []ApprovedCase{{Ref: "TC-L", Case: tc}}})

	if line := workflowLine(t, report, "wf.search_employees"); line.Ratio != 0 {
		t.Fatalf("a raw locator produced %v coverage", line.Ratio)
	}
}

// A category with no approved case at all is its own kind of blind spot,
// independent of which workflows are walked.
func TestCoverageReportsEmptyCategories(t *testing.T) {
	report := goldenCoverage(t, "TC-009", "TC-010")

	var errorHandling CategoryCoverage
	for _, line := range report.Categories {
		if line.Category == qaschema.TestCaseCategoryErrorHandling {
			errorHandling = line
		}
	}
	if errorHandling.Approved != 0 {
		t.Fatalf("error_handling counted %d approved cases, want 0", errorHandling.Approved)
	}
	if errorHandling.SuggestedTests == 0 {
		t.Error("a category with no approved case suggested nothing")
	}
	if len(report.Categories) != len(qaschema.TestCaseCategoryValues) {
		t.Errorf("the report has %d category lines, want %d",
			len(report.Categories), len(qaschema.TestCaseCategoryValues))
	}
}

// The report is a pure function of its input: the same map and the same suite
// produce the same bytes. That is what lets it be diffed run over run.
func TestCoverageIsDeterministic(t *testing.T) {
	first, err := json.Marshal(goldenCoverage(t))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	second, err := json.Marshal(goldenCoverage(t))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two runs over the same input produced different reports")
	}
}

// A gap must be counted once. A page already on a workflow's path is reported
// through that workflow, not again on its own.
func TestPagesOnAWorkflowPathAreNotCountedTwice(t *testing.T) {
	report := goldenCoverage(t)
	for _, page := range report.Pages {
		for _, workflow := range goldenMap(t).Workflows {
			if contains(refStrings(workflow.Path), page.Ref) {
				t.Fatalf("page %s is reported on its own and on workflow %s", page.Ref, workflow.ID)
			}
		}
	}
}

func caseTouching(t *testing.T, refs ...string) qaschema.TestCase {
	t.Helper()

	tc := qaschema.TestCase{
		Version: 1, ID: "TC-X", Name: "n", Priority: "low", Category: "functional",
		Assertions: []qaschema.Assertion{{Type: qaschema.AssertionTypeNoConsoleError}},
	}
	for _, ref := range refs {
		target := ref
		tc.Steps = append(tc.Steps, qaschema.Step{
			Action: qaschema.StepActionClick,
			Target: &qaschema.Target{Ref: &target},
		})
	}
	return tc
}

// refStrings copies a workflow path. qaschema.Ref is an alias for string, so
// this is a copy rather than a conversion — the slice types are identical and
// the compiler would accept the path directly, but naming the intent here
// keeps the call site readable.
func refStrings(refs []qaschema.Ref) []string {
	return append([]string(nil), refs...)
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// Asserting an element is NOT there is evidence the case did not reach it.
//
// This is the rule that keeps TC-009 — sign in with the wrong password, assert
// the employee table is hidden — from being reported as coverage of the
// employee list it never saw.
func TestAbsenceIsNotCoverage(t *testing.T) {
	target := "emp.table"
	hidden := qaschema.WaitForStepStateHidden

	absent := qaschema.TestCase{
		Version: 1, ID: "TC-N", Name: "n", Priority: "low", Category: "error_handling",
		Steps: []qaschema.Step{
			{Action: qaschema.StepActionNavigate, URL: strptr("/login")},
			{Action: qaschema.StepActionWaitFor, Target: &qaschema.Target{Ref: &target}, State: &hidden},
		},
		Assertions: []qaschema.Assertion{
			{Type: qaschema.AssertionTypeHidden, Target: &qaschema.Target{Ref: &target}},
		},
	}
	report := Coverage(CoverageInput{Map: goldenMap(t), Approved: []ApprovedCase{{Ref: "TC-N", Case: absent}}})

	for _, page := range report.Pages {
		if page.Ref == "page.employees" && page.Status == CoverageCovered {
			t.Fatal("asserting the employee table is hidden was counted as reaching the employee list")
		}
	}
	if line := workflowLine(t, report, "wf.search_employees"); line.Ratio != 0 {
		t.Fatalf("an absence assertion produced %v coverage of the search workflow", line.Ratio)
	}

	// The same target, asserted present, does count.
	present := absent
	present.Steps = present.Steps[:1]
	present.Assertions = []qaschema.Assertion{
		{Type: qaschema.AssertionTypeVisible, Target: &qaschema.Target{Ref: &target}},
	}
	report = Coverage(CoverageInput{Map: goldenMap(t), Approved: []ApprovedCase{{Ref: "TC-P", Case: present}}})
	if line := workflowLine(t, report, "wf.search_employees"); line.Ratio == 0 {
		t.Fatal("asserting the employee table is visible counted as no coverage at all")
	}
}

func strptr(s string) *string { return &s }
