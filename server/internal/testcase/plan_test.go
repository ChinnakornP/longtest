package testcase

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// goldenPlan and goldenMap are the fixture app's application map and a plan
// written against it. They are the reference pair the whole gate is judged on:
// the plan is what a good planner produces, and every rejection test below is
// that plan with one field broken.
func goldenPlan(t *testing.T) []byte {
	t.Helper()
	return readTestdata(t, "fixture-app-plan.json")
}

func goldenMap(t *testing.T) qaschema.ApplicationMap {
	t.Helper()

	raw := readTestdata(t, "fixture-app-appmap.json")
	if err := qaschema.MustBeValid("application-map@1", raw); err != nil {
		t.Fatalf("the golden application map is not a valid application-map@1: %v", err)
	}
	var appMap qaschema.ApplicationMap
	if err := json.Unmarshal(raw, &appMap); err != nil {
		t.Fatalf("decode the golden application map: %v", err)
	}
	return appMap
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

// goldenContext is the project the golden plan was written against.
func goldenContext(t *testing.T) PlanContext {
	t.Helper()

	appMap := goldenMap(t)
	refs := map[string]struct{}{}
	for _, page := range appMap.Pages {
		for _, element := range page.Elements {
			refs[element.Ref] = struct{}{}
		}
	}
	return PlanContext{
		ElementRefs: refs,
		Fixtures: map[string]struct{}{
			"logged_in_as_admin": {},
			"seeded_employee":    {},
		},
		ExistingByFingerprint: map[string]ExistingCase{},
	}
}

// The acceptance criterion the whole task turns on: a plan written against the
// fixture app's map is schema-valid, resolves every ref, and covers all five
// categories.
//
// "Resolves every ref" is what makes TARGET_NOT_FOUND unreachable for these
// cases: an element the map does not name cannot get past ReviewPlan, so a
// case that is stored is a case whose targets the executor's locator chain can
// look up.
func TestGoldenPlanIsAccepted(t *testing.T) {
	review := ReviewPlan(goldenPlan(t), goldenContext(t))

	if !review.OK() {
		t.Fatalf("the golden plan was rejected:\n%s", strings.Join(review.Problems(), "\n"))
	}
	if len(review.Accepted) == 0 {
		t.Fatal("the golden plan produced no accepted cases")
	}
	if missing := review.MissingCategories(); len(missing) > 0 {
		t.Errorf("the golden plan covers no %v; it is meant to cover all five categories", missing)
	}
	for _, category := range qaschema.TestCaseCategoryValues {
		if review.Categories[category] == 0 {
			t.Errorf("no case in category %q", category)
		}
	}

	// Every accepted case keeps the exact bytes the planner wrote. A payload
	// this backend re-encoded is not the document a client validates against
	// the contract.
	documents := plannedDocuments(t, goldenPlan(t))
	for i, accepted := range review.Accepted {
		if string(accepted.Document) != string(documents[i]) {
			t.Fatalf("case %s was re-encoded on the way through review", accepted.Ref)
		}
	}
}

func plannedDocuments(t *testing.T, plan []byte) []json.RawMessage {
	t.Helper()
	var decoded struct {
		TestCases []json.RawMessage `json:"testCases"`
	}
	if err := json.Unmarshal(plan, &decoded); err != nil {
		t.Fatalf("decode the plan: %v", err)
	}
	return decoded.TestCases
}

// Every rejection is the golden plan with exactly one thing wrong, so what the
// test proves is that the one thing is what was caught.
func TestReviewPlanRejections(t *testing.T) {
	tests := []struct {
		name string
		// mutate breaks one field of the first test case.
		mutate   func(t *testing.T, tc map[string]any)
		wantRule string
	}{
		{
			name: "a target ref no element carries",
			mutate: func(_ *testing.T, tc map[string]any) {
				steps := tc["steps"].([]any)
				steps[1].(map[string]any)["target"] = map[string]any{"ref": "emp.table.archive"}
			},
			wantRule: RuleUnknownElementRef,
		},
		{
			name: "a ref in an assertion rather than a step",
			mutate: func(_ *testing.T, tc map[string]any) {
				assertions := tc["assertions"].([]any)
				assertions[1].(map[string]any)["target"] = map[string]any{"ref": "emp.nonexistent"}
			},
			wantRule: RuleUnknownElementRef,
		},
		{
			name: "a fixture nobody registered",
			mutate: func(_ *testing.T, tc map[string]any) {
				tc["preconditions"] = []any{"fixture:logged_in_as_root"}
			},
			wantRule: RuleUnknownFixture,
		},
		{
			name: "a raw locator that is not flagged unstable",
			mutate: func(_ *testing.T, tc map[string]any) {
				steps := tc["steps"].([]any)
				steps[1].(map[string]any)["target"] = map[string]any{"locator": "#emp-table", "unstable": false}
			},
			// unstable:false fails the schema's oneOf before it reaches the
			// gate, which is the right layer for it: the contract says the
			// flag is a const.
			wantRule: RuleSchema,
		},
		{
			name: "an action outside the v1 vocabulary",
			mutate: func(_ *testing.T, tc map[string]any) {
				steps := tc["steps"].([]any)
				steps[0].(map[string]any)["action"] = "evaluate"
			},
			wantRule: RuleSchema,
		},
		{
			name: "an assertion type outside the v1 vocabulary",
			mutate: func(_ *testing.T, tc map[string]any) {
				assertions := tc["assertions"].([]any)
				assertions[0].(map[string]any)["type"] = "screenshotMatches"
			},
			wantRule: RuleSchema,
		},
		{
			name: "a literal login in place of a fixture reference",
			mutate: func(_ *testing.T, tc map[string]any) {
				tc["preconditions"] = []any{"sign in as admin@example.test / hunter2"}
			},
			wantRule: RuleSchema,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := mutateFirstCase(t, goldenPlan(t), tc.mutate)
			review := ReviewPlan(plan, goldenContext(t))

			if review.OK() {
				t.Fatal("the plan was accepted")
			}
			if len(review.Accepted) != 0 {
				t.Fatalf("a rejected plan produced %d storable cases; rejection is all-or-nothing",
					len(review.Accepted))
			}
			if !hasRule(review.Rejections, tc.wantRule) {
				t.Fatalf("wanted a %s rejection, got:\n%s", tc.wantRule, strings.Join(review.Problems(), "\n"))
			}
		})
	}
}

// A plan is taken whole or not at all: one bad case must not leave the nine
// good ones behind, because a suite silently missing the case that would have
// caught the bug reads exactly like a suite that never had it.
func TestOneBadCaseRejectsTheWholePlan(t *testing.T) {
	plan := decodePlan(t, goldenPlan(t))
	cases := plan["testCases"].([]any)
	last := cases[len(cases)-1].(map[string]any)
	last["steps"] = []any{map[string]any{
		"action": "click", "target": map[string]any{"ref": "emp.does_not_exist"},
	}}

	review := ReviewPlan(encodePlan(t, plan), goldenContext(t))
	if review.OK() {
		t.Fatal("a plan with an unresolvable ref in its last case was accepted")
	}
	if len(review.Accepted) != 0 {
		t.Fatalf("%d cases survived a rejected plan", len(review.Accepted))
	}
}

// Two cases sharing an id would collide on (project_id, ref) and the second
// would silently overwrite the first.
func TestDuplicateIDsInOnePlanAreRejected(t *testing.T) {
	plan := decodePlan(t, goldenPlan(t))
	cases := plan["testCases"].([]any)
	cases[1].(map[string]any)["id"] = cases[0].(map[string]any)["id"]

	review := ReviewPlan(encodePlan(t, plan), goldenContext(t))
	if !hasRule(review.Rejections, RuleDuplicateRef) {
		t.Fatalf("wanted a %s rejection, got:\n%s", RuleDuplicateRef, strings.Join(review.Problems(), "\n"))
	}
}

// A project with no application map cannot resolve any ref, and the honest
// answer is a rejection with one line rather than one per step.
func TestAPlanAgainstNoMapIsRejected(t *testing.T) {
	ctx := goldenContext(t)
	ctx.ElementRefs = map[string]struct{}{}

	review := ReviewPlan(goldenPlan(t), ctx)
	if review.OK() {
		t.Fatal("a plan full of refs was accepted against a project with no map")
	}
	if !hasRule(review.Rejections, RuleUnknownElementRef) {
		t.Fatalf("wanted %s, got:\n%s", RuleUnknownElementRef, strings.Join(review.Problems(), "\n"))
	}
}

// Re-running the planner on a project must not add a second row for a test it
// already has — whatever the status of the one it has.
//
// Approved must not be re-queued for review, archived must not come back after
// somebody retired it, and draft must not be stored twice under two ids, which
// is what a re-plan produces because a planner renumbers its cases every run.
func TestExistingCasesAreDeduped(t *testing.T) {
	documents := plannedDocuments(t, goldenPlan(t))

	ctx := goldenContext(t)
	for ref, status := range map[string]string{"TC-001": "approved", "TC-003": "archived"} {
		document := documentWithID(t, documents, ref)
		fingerprint, ok := FingerprintDocument(document)
		if !ok {
			t.Fatalf("%s does not fingerprint", ref)
		}
		ctx.ExistingByFingerprint[fingerprint] = ExistingCase{Ref: "OLD-" + ref, Status: status}
	}

	review := ReviewPlan(goldenPlan(t), ctx)
	if !review.OK() {
		t.Fatalf("the plan was rejected:\n%s", strings.Join(review.Problems(), "\n"))
	}
	if len(review.Duplicates) != 2 {
		t.Fatalf("got %d duplicates, want 2: %+v", len(review.Duplicates), review.Duplicates)
	}
	if len(review.Accepted) != len(documents)-2 {
		t.Fatalf("got %d accepted, want %d", len(review.Accepted), len(documents)-2)
	}
	for _, accepted := range review.Accepted {
		if accepted.Ref == "TC-001" || accepted.Ref == "TC-003" {
			t.Fatalf("%s was stored despite matching a case the project already has", accepted.Ref)
		}
	}
	// Why it was dropped travels with the report: a reviewer reads "you
	// approved this" and "somebody archived this" very differently.
	for _, duplicate := range review.Duplicates {
		if duplicate.ExistingStatus == "" || duplicate.ExistingRef == "" {
			t.Errorf("duplicate %+v does not say what it matched", duplicate)
		}
	}
	// The category counts cover the reviewed plan, duplicates included: what a
	// plan covers is not reduced by the coverage already being approved.
	if got := review.Categories[qaschema.TestCaseCategoryFunctional]; got != 2 {
		t.Fatalf("functional counted %d, want 2", got)
	}
}

// The dedupe has to survive a re-plan that renames and re-prioritises, because
// that is what a second planning run actually produces.
func TestFingerprintIgnoresCosmeticChanges(t *testing.T) {
	documents := plannedDocuments(t, goldenPlan(t))
	original := documentWithID(t, documents, "TC-002")

	base, ok := FingerprintDocument(original)
	if !ok {
		t.Fatal("the original does not fingerprint")
	}

	tests := []struct {
		name   string
		mutate func(tc map[string]any)
		same   bool
	}{
		{name: "renumbered", same: true, mutate: func(tc map[string]any) { tc["id"] = "TC-417" }},
		{name: "rephrased", same: true, mutate: func(tc map[string]any) { tc["name"] = "Add a new employee record" }},
		{name: "redescribed", same: true, mutate: func(tc map[string]any) { tc["description"] = "Written again by a later run." }},
		{name: "reprioritised", same: true, mutate: func(tc map[string]any) { tc["priority"] = "low" }},
		{name: "recategorised", same: true, mutate: func(tc map[string]any) { tc["category"] = "ui_behavior" }},
		{name: "retagged", same: true, mutate: func(tc map[string]any) { tc["tags"] = []any{"smoke"} }},
		{
			name: "a step given a timeout",
			same: true,
			mutate: func(tc map[string]any) {
				tc["steps"].([]any)[0].(map[string]any)["timeoutMs"] = 9000
			},
		},
		{
			name: "preconditions reordered",
			same: true,
			mutate: func(tc map[string]any) {
				tc["preconditions"] = []any{"fixture:logged_in_as_admin"}
			},
		},
		{
			name: "a different value typed into a field",
			mutate: func(tc map[string]any) {
				tc["steps"].([]any)[2].(map[string]any)["value"] = "Grace"
			},
		},
		{
			name: "a different element clicked",
			mutate: func(tc map[string]any) {
				tc["steps"].([]any)[1].(map[string]any)["target"] = map[string]any{"ref": "emp.edit"}
			},
		},
		{
			name: "a step removed",
			mutate: func(tc map[string]any) {
				steps := tc["steps"].([]any)
				tc["steps"] = steps[:len(steps)-1]
			},
		},
		{
			name: "an assertion added",
			mutate: func(tc map[string]any) {
				tc["assertions"] = append(tc["assertions"].([]any),
					map[string]any{"type": "noConsoleError"})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var decoded map[string]any
			if err := json.Unmarshal(original, &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			tc.mutate(decoded)
			raw, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			got, ok := FingerprintDocument(raw)
			if !ok {
				t.Fatal("the mutated case does not fingerprint")
			}
			if tc.same && got != base {
				t.Fatal("a cosmetic change produced a different fingerprint, so a re-plan would duplicate the case")
			}
			if !tc.same && got == base {
				t.Fatal("a behavioural change produced the same fingerprint, so a real new case would be dropped as a duplicate")
			}
		})
	}
}

// A number an executor treats identically must fingerprint identically, or a
// re-plan that writes 1 where the last one wrote 1.0 duplicates the case.
func TestFingerprintNormalisesAssertionValues(t *testing.T) {
	build := func(value string) string {
		var tc qaschema.TestCase
		raw := []byte(`{
			"version": 1, "id": "TC-1", "name": "n", "priority": "low", "category": "functional",
			"steps": [{"action": "navigate", "url": "/"}],
			"assertions": [{"type": "elementCount", "target": {"ref": "emp.row"}, "value": ` + value + `}]
		}`)
		if err := json.Unmarshal(raw, &tc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return Fingerprint(&tc)
	}
	if build("1") != build("1.0") {
		t.Fatal("1 and 1.0 fingerprinted differently")
	}
	if build("1") == build("2") {
		t.Fatal("1 and 2 fingerprinted the same")
	}
}

// --- helpers --------------------------------------------------------------

func decodePlan(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var plan map[string]any
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatalf("decode the golden plan: %v", err)
	}
	return plan
}

func encodePlan(t *testing.T, plan map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("encode the plan: %v", err)
	}
	return raw
}

func mutateFirstCase(t *testing.T, raw []byte, mutate func(*testing.T, map[string]any)) []byte {
	t.Helper()
	plan := decodePlan(t, raw)
	// TC-002 rather than TC-001: it has a fill step, a click step and two
	// assertions, so one mutation function can reach every shape the table
	// needs without an index that means something different per case.
	cases := plan["testCases"].([]any)
	mutate(t, cases[1].(map[string]any))
	return encodePlan(t, plan)
}

func documentWithID(t *testing.T, documents []json.RawMessage, id string) json.RawMessage {
	t.Helper()
	for _, document := range documents {
		var header struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(document, &header); err != nil {
			t.Fatalf("decode a planned case: %v", err)
		}
		if header.ID == id {
			return document
		}
	}
	t.Fatalf("the golden plan has no case %s", id)
	return nil
}

func hasRule(rejections []Rejection, rule string) bool {
	for _, rejection := range rejections {
		if rejection.Rule == rule {
			return true
		}
	}
	return false
}

// The enum guards are the layer under the schema. Nothing gets past
// ReviewPlan's schema check with an action outside the vocabulary, which is
// exactly why they are tested directly: a control whose only proof is that
// another control catches everything first is not a control.
func TestEnumGuardsRejectWhatTheSchemaWouldHaveCaught(t *testing.T) {
	ctx := goldenContext(t)

	var rejections []Rejection
	checkStep(&rejections, "TC-1", 0, &qaschema.Step{Action: "evaluate"}, ctx)
	if !hasRule(rejections, RuleUnknownAction) {
		t.Errorf("an unknown action produced %+v", rejections)
	}

	rejections = nil
	checkAssertion(&rejections, "TC-1", 0, &qaschema.Assertion{Type: "screenshotMatches"}, ctx)
	if !hasRule(rejections, RuleUnknownAssertion) {
		t.Errorf("an unknown assertion type produced %+v", rejections)
	}

	// A click with no target at all: the schema requires one, and so does this.
	rejections = nil
	checkStep(&rejections, "TC-1", 3, &qaschema.Step{Action: qaschema.StepActionClick}, ctx)
	if !hasRule(rejections, RuleNoTarget) {
		t.Errorf("a targetless click produced %+v", rejections)
	}

	// navigate, screenshot and press are legitimately targetless.
	for _, action := range []qaschema.StepAction{
		qaschema.StepActionNavigate, qaschema.StepActionScreenshot, qaschema.StepActionPress,
	} {
		rejections = nil
		checkStep(&rejections, "TC-1", 0, &qaschema.Step{Action: action}, ctx)
		if len(rejections) != 0 {
			t.Errorf("a targetless %s was rejected: %+v", action, rejections)
		}
	}
}
