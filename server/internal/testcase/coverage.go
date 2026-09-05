package testcase

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// Coverage: the answer to "what should this system be tested for that it is
// not?".
//
// It is deliberately computed, not asked of a model. The question has an exact
// answer — the application map says which workflows and pages exist, the
// approved suite says which of them anything walks — and an answer a reviewer
// can check by reading two lists is worth more than a paragraph of plausible
// prose. The model's job is writing the cases this report asks for; deciding
// what is missing is arithmetic.
//
// The one judgement in here is what "covered" means, and it is the strict
// reading: a workflow is covered when ONE approved case touches every ref on
// its path. Taking the union across cases would report a workflow as tested
// because three different cases each walked a third of it, which is not the
// same thing as anybody having tested that workflow.

// CoverageStatus is how well the approved suite covers one workflow or page.
type CoverageStatus string

// The three states. `partial` exists because "somebody started this and
// stopped" is a different backlog item from "nobody has been here".
const (
	CoverageCovered   CoverageStatus = "covered"
	CoveragePartial   CoverageStatus = "partial"
	CoverageUncovered CoverageStatus = "uncovered"
)

// RiskLevel ranks what to write next.
type RiskLevel string

// The three levels, ordered.
const (
	RiskHigh   RiskLevel = "high"
	RiskMedium RiskLevel = "medium"
	RiskLow    RiskLevel = "low"
)

// WorkflowCoverage is one workflow's line in the report.
type WorkflowCoverage struct {
	Ref             string         `json:"ref"`
	Name            string         `json:"name"`
	ExpectedOutcome string         `json:"expectedOutcome,omitempty"`
	Status          CoverageStatus `json:"status"`
	// Ratio is the best single case's share of the workflow's path, 0..1
	// rounded to two places.
	Ratio float64 `json:"coverageRatio"`
	// CoveringCaseRefs are the approved cases that walk the whole path.
	CoveringCaseRefs []string `json:"coveringCaseRefs"`
	// MissingRefs are the path steps no single case reaches, in path order.
	MissingRefs []string `json:"missingRefs,omitempty"`
	// AuthRequired says the workflow crosses a page behind a login, which is
	// what separates a high-risk gap from a merely untested one.
	AuthRequired bool      `json:"authRequired"`
	Risk         RiskLevel `json:"risk"`
	// SuggestedTests is how many cases to write, and Suggestions says which
	// categories and why, so the number is auditable rather than asserted.
	SuggestedTests int          `json:"suggestedTests"`
	Suggestions    []Suggestion `json:"suggestions,omitempty"`
}

// PageCoverage is one page's line. A page nothing navigates to is a hole the
// workflow list can miss entirely, because discovery only records a workflow
// it recognised as one.
type PageCoverage struct {
	Ref            string         `json:"ref"`
	Path           string         `json:"path"`
	Title          string         `json:"title,omitempty"`
	Status         CoverageStatus `json:"status"`
	AuthRequired   bool           `json:"authRequired"`
	Risk           RiskLevel      `json:"risk"`
	SuggestedTests int            `json:"suggestedTests"`
	Suggestions    []Suggestion   `json:"suggestions,omitempty"`
}

// CategoryCoverage is how many approved cases a project has in one of the five
// contract categories. A project with forty functional cases and no
// error_handling case is not a well-tested project.
type CategoryCoverage struct {
	Category       qaschema.TestCaseCategory `json:"category"`
	Approved       int                       `json:"approved"`
	SuggestedTests int                       `json:"suggestedTests"`
}

// Suggestion is one recommended case, with the reason it is recommended.
type Suggestion struct {
	Category qaschema.TestCaseCategory `json:"category"`
	Reason   string                    `json:"reason"`
}

// CoverageReport is what GET /projects/{id}/coverage returns.
type CoverageReport struct {
	ProjectID string `json:"projectId"`
	// GeneratedAtRunID is the discovery run the map came from, so a reader can
	// tell a gap in the app from a gap in what we have looked at.
	GeneratedAtRunID string             `json:"generatedAtRunId,omitempty"`
	ApprovedCases    int                `json:"approvedCases"`
	Workflows        []WorkflowCoverage `json:"workflows"`
	Pages            []PageCoverage     `json:"pages"`
	Categories       []CategoryCoverage `json:"categories"`
	// SuggestedTestCount is the sum of every suggestion below, which is the
	// one number a dashboard shows.
	SuggestedTestCount int `json:"suggestedTestCount"`
	// Summary is one sentence of plain English. It is assembled from the
	// counts above, never written by a model.
	Summary string `json:"summary"`
}

// CoverageInput is the report's two sources: what exists, and what is tested.
type CoverageInput struct {
	Map qaschema.ApplicationMap
	// Approved is the approved suite. Ref is the database ref ("TC-001"),
	// which is what a reader will look the case up by.
	Approved []ApprovedCase
	// CategoryCounts is the approved-case count per category, counted in the
	// database. Passed in rather than derived from Approved so a case whose
	// payload failed to decode still counts towards its category.
	CategoryCounts map[qaschema.TestCaseCategory]int
}

// ApprovedCase is one member of the approved suite, decoded.
type ApprovedCase struct {
	Ref  string
	Case qaschema.TestCase
}

// Coverage compares the application map with the approved suite.
//
// It is a pure function of its input: same map and same suite, same report.
// That is what lets the fixture-app assertion in the tests be an equality
// check rather than a range.
func Coverage(in CoverageInput) CoverageReport {
	index := indexMap(in.Map)

	touchedByCase := make([]map[string]struct{}, len(in.Approved))
	for i, approved := range in.Approved {
		touchedByCase[i] = touchedRefs(&approved.Case, index)
	}

	report := CoverageReport{
		ApprovedCases: len(in.Approved),
		Workflows:     make([]WorkflowCoverage, 0, len(in.Map.Workflows)),
		Pages:         make([]PageCoverage, 0, len(in.Map.Pages)),
		Categories:    make([]CategoryCoverage, 0, len(qaschema.TestCaseCategoryValues)),
	}
	if in.Map.ProjectID != nil {
		report.ProjectID = *in.Map.ProjectID
	}
	if in.Map.GeneratedAtRunID != nil {
		report.GeneratedAtRunID = *in.Map.GeneratedAtRunID
	}

	for _, workflow := range in.Map.Workflows {
		report.Workflows = append(report.Workflows,
			coverWorkflow(workflow, in.Approved, touchedByCase, index))
	}
	// Every page a workflow already reports on is left out: the same gap
	// listed twice would be counted twice in SuggestedTestCount.
	onWorkflowPath := map[string]struct{}{}
	for _, workflow := range in.Map.Workflows {
		for _, ref := range workflow.Path {
			if page, ok := index.pageOfRef[ref]; ok {
				onWorkflowPath[page] = struct{}{}
			}
		}
	}
	for _, page := range in.Map.Pages {
		if _, onPath := onWorkflowPath[page.ID]; onPath {
			continue
		}
		report.Pages = append(report.Pages, coverPage(page, touchedByCase))
	}

	for _, category := range qaschema.TestCaseCategoryValues {
		approved := in.CategoryCounts[category]
		line := CategoryCoverage{Category: category, Approved: approved}
		if approved == 0 && len(in.Map.Pages) > 0 {
			// One case is what it takes to stop a category being a blind
			// spot. Suggesting more would be guessing at an application this
			// function has only the map of.
			line.SuggestedTests = 1
		}
		report.Categories = append(report.Categories, line)
	}

	for _, workflow := range report.Workflows {
		report.SuggestedTestCount += workflow.SuggestedTests
	}
	for _, page := range report.Pages {
		report.SuggestedTestCount += page.SuggestedTests
	}
	for _, category := range report.Categories {
		report.SuggestedTestCount += category.SuggestedTests
	}
	report.Summary = summarise(report)
	return report
}

// mapIndex is the map turned inside out: the lookups the comparison needs,
// built once instead of scanned per case.
type mapIndex struct {
	// pageOfRef resolves a page ref to itself and an element ref to its page,
	// so a workflow path made of both kinds resolves in one lookup.
	pageOfRef map[string]string
	// pageOfPath resolves a navigate URL's path to the page ref it lands on.
	pageOfPath map[string]string
	// authRequired is the pages behind a login.
	authRequired map[string]bool
	// inputLike is the pages carrying a field a user can get wrong, which is
	// what makes a validation case worth suggesting.
	inputLike map[string]bool
	// elementRefs is every element ref, so a target that names one can be
	// distinguished from one that names nothing.
	elementRefs map[string]struct{}
}

func indexMap(m qaschema.ApplicationMap) mapIndex {
	index := mapIndex{
		pageOfRef:    map[string]string{},
		pageOfPath:   map[string]string{},
		authRequired: map[string]bool{},
		inputLike:    map[string]bool{},
		elementRefs:  map[string]struct{}{},
	}
	for _, page := range m.Pages {
		index.pageOfRef[page.ID] = page.ID
		index.pageOfPath[normalisePath(page.Path)] = page.ID
		index.authRequired[page.ID] = page.AuthRequired != nil && *page.AuthRequired
		for _, element := range page.Elements {
			index.pageOfRef[element.Ref] = page.ID
			index.elementRefs[element.Ref] = struct{}{}
			if inputLike(element.Type) {
				index.inputLike[page.ID] = true
			}
		}
	}
	return index
}

func inputLike(t qaschema.ElementType) bool {
	switch t {
	case qaschema.ElementTypeInput, qaschema.ElementTypeTextarea, qaschema.ElementTypeSelect,
		qaschema.ElementTypeCheckbox, qaschema.ElementTypeRadio, qaschema.ElementTypeForm:
		return true
	default:
		return false
	}
}

// touchedRefs is everywhere one case goes: the element refs it names, and the
// page refs those elements belong to plus the ones it navigates to.
//
// A raw locator contributes nothing on purpose. The map cannot say what it
// resolves to, and counting an invented selector as coverage of a workflow is
// how a suite ends up reported as complete because a model guessed at a CSS
// class.
func touchedRefs(tc *qaschema.TestCase, index mapIndex) map[string]struct{} {
	out := map[string]struct{}{}
	touch := func(ref string) {
		if ref == "" {
			return
		}
		out[ref] = struct{}{}
		if page, ok := index.pageOfRef[ref]; ok {
			out[page] = struct{}{}
		}
	}

	for i := range tc.Steps {
		step := &tc.Steps[i]
		if step.Target != nil && step.Target.Ref != nil && !waitsForAbsence(step) {
			touch(*step.Target.Ref)
		}
		if step.Action == qaschema.StepActionNavigate && step.URL != nil {
			if page, ok := index.pageOfPath[normalisePath(urlPath(*step.URL))]; ok {
				out[page] = struct{}{}
			}
		}
	}
	for i := range tc.Assertions {
		assertion := &tc.Assertions[i]
		if assertion.Type == qaschema.AssertionTypeHidden {
			// Asserting an element is NOT there is evidence the case did not
			// reach it. Counting it as coverage is how "the employee table is
			// hidden on the sign-in error page" gets reported as having tested
			// the employee list.
			continue
		}
		if assertion.Target != nil && assertion.Target.Ref != nil {
			touch(*assertion.Target.Ref)
		}
	}
	return out
}

// waitsForAbsence is the step form of the same rule: waiting for a dialog to
// disappear is not visiting the page it was on.
func waitsForAbsence(step *qaschema.Step) bool {
	if step.Action != qaschema.StepActionWaitFor || step.State == nil {
		return false
	}
	return *step.State == qaschema.WaitForStepStateHidden || *step.State == qaschema.WaitForStepStateDetached
}

func coverWorkflow(workflow qaschema.Workflow, approved []ApprovedCase, touched []map[string]struct{}, index mapIndex) WorkflowCoverage {
	line := WorkflowCoverage{
		Ref:              workflow.ID,
		Name:             workflow.Name,
		ExpectedOutcome:  workflow.ExpectedOutcome,
		CoveringCaseRefs: []string{},
		Status:           CoverageUncovered,
	}
	for _, ref := range workflow.Path {
		if page, ok := index.pageOfRef[ref]; ok && index.authRequired[page] {
			line.AuthRequired = true
		}
	}
	if workflow.AuthRequired != nil && *workflow.AuthRequired {
		line.AuthRequired = true
	}

	if len(workflow.Path) == 0 {
		// A workflow with no path says nothing about the application, so
		// nothing can cover it and nothing is suggested for it.
		line.Status = CoverageCovered
		line.Ratio = 1
		return line
	}

	best := 0
	var bestMissing []string
	for i, set := range touched {
		hit, missing := intersect(workflow.Path, set)
		if hit == len(workflow.Path) {
			line.CoveringCaseRefs = append(line.CoveringCaseRefs, approved[i].Ref)
		}
		if hit > best {
			best, bestMissing = hit, missing
		}
	}
	if best == 0 {
		bestMissing = append([]string(nil), workflow.Path...)
	}
	line.Ratio = round2(float64(best) / float64(len(workflow.Path)))

	switch {
	case len(line.CoveringCaseRefs) > 0:
		line.Status, line.Risk = CoverageCovered, RiskLow
		return line
	case best > 0:
		line.Status, line.Risk = CoveragePartial, RiskMedium
		line.MissingRefs = bestMissing
		line.Suggestions = []Suggestion{{
			Category: qaschema.TestCaseCategoryFunctional,
			Reason: fmt.Sprintf("no single approved case walks %q end to end; %d of %d steps are unreached",
				workflow.Name, len(workflow.Path)-best, len(workflow.Path)),
		}}
	default:
		line.Status = CoverageUncovered
		line.MissingRefs = bestMissing
		line.Risk = RiskMedium
		if line.AuthRequired {
			// A flow behind a login is where a customer's data is. An
			// untested one is the gap that costs, so it outranks an untested
			// public page however many steps that page has.
			line.Risk = RiskHigh
		}
		line.Suggestions = []Suggestion{{
			Category: qaschema.TestCaseCategoryFunctional,
			Reason:   fmt.Sprintf("no approved case walks %q at all", workflow.Name),
		}}
		if workflowHasInput(workflow, index) {
			line.Suggestions = append(line.Suggestions, Suggestion{
				Category: qaschema.TestCaseCategoryValidation,
				Reason:   fmt.Sprintf("%q submits a form, so its rejection path needs a case of its own", workflow.Name),
			})
		}
		if line.AuthRequired {
			line.Suggestions = append(line.Suggestions, Suggestion{
				Category: qaschema.TestCaseCategoryErrorHandling,
				Reason:   fmt.Sprintf("%q is behind a login; what it does to a signed-out visitor is untested", workflow.Name),
			})
		}
	}
	line.SuggestedTests = len(line.Suggestions)
	return line
}

func workflowHasInput(workflow qaschema.Workflow, index mapIndex) bool {
	for _, ref := range workflow.Path {
		if page, ok := index.pageOfRef[ref]; ok && index.inputLike[page] {
			return true
		}
	}
	return false
}

func coverPage(page qaschema.Page, touched []map[string]struct{}) PageCoverage {
	line := PageCoverage{
		Ref:          page.ID,
		Path:         page.Path,
		Title:        page.Title,
		AuthRequired: page.AuthRequired != nil && *page.AuthRequired,
		Status:       CoverageUncovered,
		Risk:         RiskLow,
	}
	for _, set := range touched {
		if _, ok := set[page.ID]; ok {
			line.Status = CoverageCovered
			return line
		}
	}
	line.Risk = RiskMedium
	if line.AuthRequired {
		line.Risk = RiskHigh
	}
	line.Suggestions = []Suggestion{{
		Category: qaschema.TestCaseCategoryNavigation,
		Reason:   fmt.Sprintf("no approved case ever reaches %s", page.Path),
	}}
	line.SuggestedTests = len(line.Suggestions)
	return line
}

// intersect counts how many of a workflow's path refs a case reaches, and
// returns the ones it does not, in path order.
func intersect(path []qaschema.Ref, touched map[string]struct{}) (int, []string) {
	hit := 0
	var missing []string
	for _, ref := range path {
		if _, ok := touched[ref]; ok {
			hit++
			continue
		}
		missing = append(missing, ref)
	}
	return hit, missing
}

func summarise(report CoverageReport) string {
	if report.SuggestedTestCount == 0 {
		return fmt.Sprintf("%d approved cases cover every workflow and page in the map.", report.ApprovedCases)
	}

	uncovered := 0
	for _, workflow := range report.Workflows {
		if workflow.Status != CoverageCovered {
			uncovered++
		}
	}
	unreached := 0
	for _, page := range report.Pages {
		if page.Status != CoverageCovered {
			unreached++
		}
	}
	var missing []string
	for _, category := range report.Categories {
		if category.Approved == 0 {
			missing = append(missing, string(category.Category))
		}
	}
	sort.Strings(missing)

	parts := []string{fmt.Sprintf("%d approved cases", report.ApprovedCases)}
	if uncovered > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d workflows are not walked end to end",
			uncovered, len(report.Workflows)))
	}
	if unreached > 0 {
		parts = append(parts, fmt.Sprintf("%d pages are never reached", unreached))
	}
	if len(missing) > 0 {
		parts = append(parts, "no case at all in "+strings.Join(missing, ", "))
	}
	return fmt.Sprintf("%s. %d more cases suggested.",
		strings.Join(parts, "; "), report.SuggestedTestCount)
}

// urlPath reduces a navigate target to the path a page row would carry. An
// absolute URL to another origin has no page here and simply matches nothing.
func urlPath(raw string) string {
	if strings.HasPrefix(raw, "/") {
		return raw
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j:]
		}
		return "/"
	}
	return raw
}

// normalisePath strips the query, the fragment and a trailing slash, so
// /employees, /employees/ and /employees?q=a are one page.
func normalisePath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	if path == "" {
		return "/"
	}
	return path
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

// CoverageFor assembles the report for one project.
//
// The application map is passed in rather than read here: assembling it is
// internal/project's job, and this package is the one internal/project depends
// on. Taking it as an argument keeps the dependency pointing one way and makes
// the pure function above testable against a hand-written map.
func (s *Service) CoverageFor(ctx context.Context, scope auth.OrgScope, projectID uuid.UUID, appMap qaschema.ApplicationMap) (CoverageReport, error) {
	rows, err := s.store.ListApprovedTestCasePayloads(ctx, dbgen.ListApprovedTestCasePayloadsParams{
		OrgID: scope.OrgID(), ProjectID: projectID,
	})
	if err != nil {
		return CoverageReport{}, fmt.Errorf("list approved test cases: %w", db.Classify(err))
	}
	counts, err := s.store.CountApprovedTestCasesByCategory(ctx, dbgen.CountApprovedTestCasesByCategoryParams{
		OrgID: scope.OrgID(), ProjectID: projectID,
	})
	if err != nil {
		return CoverageReport{}, fmt.Errorf("count approved test cases: %w", db.Classify(err))
	}

	approved := make([]ApprovedCase, 0, len(rows))
	for _, row := range rows {
		var decoded qaschema.TestCase
		if err := json.Unmarshal(row.Payload, &decoded); err != nil {
			// A stored payload that no longer decodes contributes no coverage
			// and is not allowed to fail the report: the honest answer to
			// "what is untested?" is still computable without it, and it would
			// otherwise take the whole endpoint down for one bad row.
			httpx.LoggerFrom(ctx).WarnContext(ctx, "approved test case payload does not decode",
				"test_case_ref", row.Ref, "project_id", projectID)
			continue
		}
		approved = append(approved, ApprovedCase{Ref: row.Ref, Case: decoded})
	}

	report := Coverage(CoverageInput{
		Map:            appMap,
		Approved:       approved,
		CategoryCounts: CategoryCountsFor(counts),
	})
	report.ProjectID = projectID.String()
	return report, nil
}
