package analysis

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// findingJSON builds an analysis answer the way a model would write one.
func findingJSON(ref string, stepIndex any, evidence ...string) map[string]any {
	if evidence == nil {
		evidence = []string{}
	}
	return map[string]any{
		"version":      1,
		"testCaseId":   ref,
		"stepIndex":    stepIndex,
		"failureClass": "PRODUCT_BUG",
		"rootCause":    "POST /api/employees returned 500",
		"confidence":   0.9,
		"evidence":     evidence,
	}
}

func answer(t *testing.T, findings ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

func TestReview(t *testing.T) {
	tests := []struct {
		name    string
		answer  []map[string]any
		want    []string
		mention string
	}{
		{
			name:   "a well-formed answer passes",
			answer: []map[string]any{findingJSON("TC-001", 1, "e0-screenshot-0")},
		},
		{
			name:   "a whole-case finding with a null step passes",
			answer: []map[string]any{findingJSON("TC-001", nil, "e0-screenshot-0")},
		},
		{
			// The headline rule. A fabricated id survives ingest silently: the
			// backend links only handles it stored, so the finding lands with
			// an empty evidence list and reads like a conclusion nobody
			// bothered to support.
			name:    "an invented artifact id is rejected",
			answer:  []map[string]any{findingJSON("TC-001", 1, "screenshot-9")},
			want:    []string{RuleUnknownEvidence},
			mention: "screenshot-9",
		},
		{
			name:   "one good citation does not excuse one bad one",
			answer: []map[string]any{findingJSON("TC-001", 1, "e0-screenshot-0", "trace-4")},
			want:   []string{RuleUnknownEvidence},
		},
		{
			name:    "a step the test case does not have is rejected",
			answer:  []map[string]any{findingJSON("TC-001", 7, "e0-screenshot-0")},
			want:    []string{RuleUnknownStep},
			mention: "2 step(s)",
		},
		{
			name:   "a finding about a case this analysis did not ask about is rejected",
			answer: []map[string]any{findingJSON("TC-404", 0, "e0-screenshot-0")},
			// The unknown case is rejected AND TC-001 is left uncovered.
			want: []string{RuleMissingFinding, RuleUnknownTestCase},
		},
		{
			name: "two findings for one execution are rejected",
			answer: []map[string]any{
				findingJSON("TC-001", 0, "e0-screenshot-0"),
				findingJSON("TC-001", 1, "e0-screenshot-0"),
			},
			want: []string{RuleDuplicateFinding},
		},
		{
			name:   "a failure the analyst said nothing about is rejected",
			answer: []map[string]any{},
			want:   []string{RuleMissingFinding},
		},
	}

	ctx := NewContext([]Bundle{bundleFor("TC-001")})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rejections := ctx.Review(answer(t, tc.answer...))
			got := rulesOf(rejections)
			if len(got) != len(tc.want) {
				t.Fatalf("rules = %v, want %v (%v)", got, tc.want, Problems(rejections))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("rules = %v, want %v", got, tc.want)
				}
			}
			if tc.mention == "" {
				return
			}
			if !strings.Contains(strings.Join(Problems(rejections), "\n"), tc.mention) {
				t.Fatalf("the feedback does not name %q: %v", tc.mention, Problems(rejections))
			}
		})
	}
}

// One bad citation refuses the whole answer, not the finding it is in.
//
// Keeping the findings that checked out would produce a report whose gaps are
// indistinguishable from failures the analyst had nothing to say about.
func TestOneBadCitationRefusesTheWholeAnswer(t *testing.T) {
	ctx := NewContext([]Bundle{bundleFor("TC-001"), bundleFor("TC-002")})
	problems := ctx.ReviewHook()(answer(t,
		findingJSON("TC-001", 1, "e0-screenshot-0"),
		findingJSON("TC-002", 1, "screenshot-invented"),
	))

	if len(problems) == 0 {
		t.Fatal("the answer was accepted with a fabricated citation in it")
	}
	// ReviewHook returning anything at all is what makes the runner discard
	// the whole document and re-ask; there is no partial-accept path.
	if !strings.Contains(problems[0], "TC-002") {
		t.Fatalf("the feedback should name the offending case: %v", problems)
	}
}

// The gate reports every problem in one pass. A retry that fixes one citation
// and trips over the next burns three attempts on one round of feedback.
func TestReviewReportsEveryProblemAtOnce(t *testing.T) {
	ctx := NewContext([]Bundle{bundleFor("TC-001"), bundleFor("TC-002")})
	rejections := ctx.Review(answer(t, findingJSON("TC-001", 9, "nope", "also-nope")))

	if len(rejections) != 4 {
		t.Fatalf("rejections = %v, want two bad citations, one bad step and one uncovered case",
			Problems(rejections))
	}
}

// The gate is a pure function of the bytes, as agent.Task.Review requires: it
// runs once per attempt, and a check with a side effect would apply it up to
// MaxAttempts times.
func TestReviewIsRepeatable(t *testing.T) {
	ctx := NewContext([]Bundle{bundleFor("TC-001")})
	document := answer(t, findingJSON("TC-001", 9, "invented"))

	first := Problems(ctx.Review(document))
	second := Problems(ctx.Review(document))
	if len(first) != len(second) {
		t.Fatalf("two reviews of one document disagreed: %v vs %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("the feedback reshuffled between attempts: %v vs %v", first, second)
		}
	}
}

// A document that is not an array is reported rather than ignored: "the gate
// did not run" must never look like "the gate passed".
func TestReviewReportsAnUnreadableDocument(t *testing.T) {
	ctx := NewContext([]Bundle{bundleFor("TC-001")})
	rejections := ctx.Review([]byte(`{"version":1}`))

	if len(rejections) != 1 || rejections[0].Rule != RuleSchema {
		t.Fatalf("rejections = %v", Problems(rejections))
	}
}

// A rejection renders the same way the planning gate's does, so a reader of run
// events does not have to learn a second error format.
func TestRejectionRendering(t *testing.T) {
	caseLevel := Rejection{TestCaseID: "TC-001", StepIndex: -1, Rule: RuleUnknownEvidence, Detail: "no such artifact"}
	if got := caseLevel.String(); got != "TC-001: unknown_evidence_artifact: no such artifact" {
		t.Fatalf("String() = %q", got)
	}
	stepLevel := Rejection{TestCaseID: "TC-001", StepIndex: 3, Rule: RuleUnknownStep, Detail: "out of range"}
	if got := stepLevel.String(); got != "TC-001 step 3: unknown_step_index: out of range" {
		t.Fatalf("String() = %q", got)
	}
	resultLevel := Rejection{StepIndex: -1, Rule: RuleSchema, Detail: "not an array"}
	if got := resultLevel.String(); got != "analysis: schema_invalid: not an array" {
		t.Fatalf("String() = %q", got)
	}
}

// A context built from the failures only. A finding on a green row is worse
// than no finding at all.
func TestContextOnlyKnowsTheFailuresItWasAskedAbout(t *testing.T) {
	passing := failedExecution("TC-002")
	passing.Result = qaschema.OutcomePass
	bundles := Collector{}.Collect(
		[]qaschema.ExecutionResult{failedExecution("TC-001"), passing}, nil)

	rejections := NewContext(bundles).Review(answer(t, findingJSON("TC-002", nil, "e0-screenshot-0")))
	if len(rulesOf(rejections)) == 0 || rulesOf(rejections)[1] != RuleUnknownTestCase {
		t.Fatalf("a finding about a passing case was accepted: %v", Problems(rejections))
	}
}
