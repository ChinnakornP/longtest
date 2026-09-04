package security_test

import (
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

func gate(t *testing.T) *security.PlanGate {
	t.Helper()
	rules, err := security.TargetRules("https://demo.example.test", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := security.NewEgressPolicy(rules)
	if err != nil {
		t.Fatal(err)
	}
	sc := security.NewScrubber()
	if err := sc.Add(fakePassword); err != nil {
		t.Fatal(err)
	}
	return &security.PlanGate{
		Egress:        p,
		BaseURL:       "https://demo.example.test",
		KnownFixtures: map[string]struct{}{"logged_in_as_admin": {}},
		Scrubber:      sc,
	}
}

func planWith(steps ...qaschema.Step) *qaschema.TestPlan {
	return &qaschema.TestPlan{
		Version: 1, Rationale: "r", CoverageNotes: "c",
		TestCases: []qaschema.TestCase{{
			Version: 1, ID: "TC-001", Name: "case",
			Priority:   qaschema.TestCasePriorityMedium,
			Category:   qaschema.TestCaseCategoryFunctional,
			Steps:      steps,
			Assertions: []qaschema.Assertion{{Type: qaschema.AssertionTypeNoConsoleError}},
		}},
	}
}

func rules(vs []security.Violation) map[string]bool {
	out := map[string]bool{}
	for _, v := range vs {
		out[v.Rule] = true
	}
	return out
}

func TestGateResolvesRelativeNavigateAgainstTheBaseURL(t *testing.T) {
	g := gate(t)
	if got := g.Check(planWith(qaschema.Step{
		Action: qaschema.StepActionNavigate, URL: strPtr("/employees?q=1"),
	})); len(got) != 0 {
		t.Fatalf("a same-origin relative navigate was rejected: %v", got)
	}
}

// A protocol-relative URL looks relative and is not: //attacker.test/ resolves
// to https://attacker.test/.
func TestGateCatchesAProtocolRelativeNavigate(t *testing.T) {
	g := gate(t)
	got := g.Check(planWith(qaschema.Step{
		Action: qaschema.StepActionNavigate, URL: strPtr("//attacker.test/beacon"),
	}))
	if !rules(got)[security.RuleEgress] {
		t.Fatalf("a protocol-relative navigate was not caught: %v", got)
	}
}

func TestGateRejectsACredentialInAStepValue(t *testing.T) {
	g := gate(t)
	got := g.Check(planWith(qaschema.Step{
		Action: qaschema.StepActionFill,
		Target: &qaschema.Target{Ref: refPtr("password")},
		Value:  strPtr(fakePassword),
	}))
	if !rules(got)[security.RuleCredentialInPlan] {
		t.Fatalf("a literal credential in a step value was accepted: %v", got)
	}
	// The violation must not quote the credential it is complaining about.
	for _, v := range got {
		if v.Rule == security.RuleCredentialInPlan && strings.Contains(v.Detail, fakePassword) {
			t.Fatalf("the violation detail discloses the credential: %q", v.Detail)
		}
	}
}

func TestGateRejectsAnUnknownFixture(t *testing.T) {
	g := gate(t)
	plan := planWith(qaschema.Step{Action: qaschema.StepActionNavigate, URL: strPtr("/")})
	plan.TestCases[0].Preconditions = []qaschema.Precondition{"fixture:logged_in_as_root"}
	if !rules(g.Check(plan))[security.RuleUnknownFixture] {
		t.Fatal("an invented fixture was accepted")
	}

	plan.TestCases[0].Preconditions = []qaschema.Precondition{"the user is logged in as an admin"}
	if !rules(g.Check(plan))[security.RulePreconditionShape] {
		t.Fatal("a prose precondition was accepted")
	}
}

func TestGateRequiresRawLocatorsToBeFlagged(t *testing.T) {
	g := gate(t)
	got := g.Check(planWith(qaschema.Step{
		Action: qaschema.StepActionClick,
		Target: &qaschema.Target{Locator: strPtr("button.delete")},
	}))
	if !rules(got)[security.RuleUnstableLocator] {
		t.Fatalf("an unflagged raw locator was accepted: %v", got)
	}

	yes := true
	if got := g.Check(planWith(qaschema.Step{
		Action: qaschema.StepActionClick,
		Target: &qaschema.Target{Locator: strPtr("button.delete"), Unstable: &yes},
	})); len(got) != 0 {
		t.Fatalf("a correctly flagged locator was rejected: %v", got)
	}
}

func TestGateRejectsAnUnknownAction(t *testing.T) {
	g := gate(t)
	got := g.Check(planWith(qaschema.Step{Action: qaschema.StepAction("execute")}))
	if !rules(got)[security.RuleUnknownAction] {
		t.Fatalf("an action outside the v1 vocabulary was accepted: %v", got)
	}
}

func TestGateBoundsPlanSize(t *testing.T) {
	g := gate(t)
	g.MaxSteps = 3
	steps := make([]qaschema.Step, 5)
	for i := range steps {
		steps[i] = qaschema.Step{Action: qaschema.StepActionNavigate, URL: strPtr("/")}
	}
	if !rules(g.Check(planWith(steps...)))[security.RulePlanTooLarge] {
		t.Fatal("an oversized case was accepted")
	}
}

func TestGateReportsEveryViolationNotJustTheFirst(t *testing.T) {
	g := gate(t)
	got := g.Check(planWith(
		qaschema.Step{Action: qaschema.StepActionNavigate, URL: strPtr("https://attacker.test/")},
		qaschema.Step{Action: qaschema.StepActionFill,
			Target: &qaschema.Target{Locator: strPtr("input#p")}, Value: strPtr(fakePassword)},
	))
	r := rules(got)
	for _, want := range []string{
		security.RuleEgress, security.RuleCredentialInPlan, security.RuleUnstableLocator,
	} {
		if !r[want] {
			t.Errorf("rule %q did not fire; a retry loop would need one round per problem: %v", want, got)
		}
	}
}

func TestGateRejectsAWrongVersion(t *testing.T) {
	g := gate(t)
	p := planWith(qaschema.Step{Action: qaschema.StepActionNavigate, URL: strPtr("/")})
	p.Version = 2
	if !rules(g.Check(p))[security.RuleBadVersion] {
		t.Fatal("a plan claiming a different contract version was accepted")
	}
	if got := g.Check(nil); len(got) == 0 {
		t.Fatal("a nil plan was accepted")
	}
}

// Without an egress policy the gate has no basis to allow anything, so it
// must fail closed rather than skip the check.
func TestGateWithoutAPolicyFailsClosed(t *testing.T) {
	g := &security.PlanGate{BaseURL: "https://demo.example.test"}
	got := g.Check(planWith(qaschema.Step{
		Action: qaschema.StepActionNavigate, URL: strPtr("/employees"),
	}))
	if !rules(got)[security.RuleEgress] {
		t.Fatalf("a misconfigured gate allowed a navigation: %v", got)
	}
}
