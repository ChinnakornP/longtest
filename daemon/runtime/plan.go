package runtime

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
	"github.com/ChinnakornP/longtest/daemon/security"
)

// The planning phase: what the model is given, and what it has to satisfy
// before the plan leaves this machine.
//
// The backend re-checks all of this on ingest and is the authority; this gate
// exists because it runs while the model is still there to be asked again. A
// plan rejected here costs one more attempt out of three and the run still
// finishes with a usable suite. The same plan rejected at the backend costs
// the whole run, and by then the workspace is the only place the reasons live.

// planReview builds the [agent.Task.Review] check for a planning task.
//
// The three things it knows that the contract cannot: which element refs this
// run's application map actually holds, which fixtures this daemon can
// establish, and where the run is allowed to navigate. A plan that satisfies
// test-plan@1 and fails any of them is a plan whose every case would end as
// TARGET_NOT_FOUND, or as an unestablishable precondition, or as a request to
// a host the run has no business touching.
func planReview(appMap *qaschema.ApplicationMap, baseURL string, fixtures map[string]struct{}, scrubber *security.Scrubber) func([]byte) []string {
	refs := elementRefs(appMap)
	gate := &security.PlanGate{
		Egress:        egressFor(baseURL),
		BaseURL:       baseURL,
		KnownFixtures: fixtures,
		Scrubber:      scrubber,
	}

	return func(output []byte) []string {
		var plan qaschema.TestPlan
		if err := json.Unmarshal(output, &plan); err != nil {
			// Unreachable: the schema check runs first and this is the same
			// document. Reported rather than ignored, because "the gate did
			// not run" must never look like "the gate passed".
			return []string{fmt.Sprintf("plan: the document could not be re-read for review: %v", err)}
		}

		problems := make([]string, 0, 8)
		for _, violation := range gate.Check(&plan) {
			problems = append(problems, violation.String())
		}
		problems = append(problems, unknownRefs(&plan, refs)...)
		if len(problems) == 0 {
			return nil
		}
		return problems
	}
}

// unknownRefs is the check the security gate deliberately does not make: it is
// about whether a target exists, not about whether it is safe.
//
// A target by ref that no element carries is the single most common way an
// AI-authored plan fails, and it fails late — at execution, on the customer's
// application, in a case somebody approved. Catching it here turns a failed run
// into one more attempt.
func unknownRefs(plan *qaschema.TestPlan, refs map[string]struct{}) []string {
	if len(refs) == 0 {
		// No map means no ref can resolve. Saying so once is more useful than
		// saying it once per step.
		if planUsesRefs(plan) {
			return []string{"plan: unknown_element_ref: this run has no application map, so no target ref can resolve; " +
				"use {\"locator\": \"...\", \"unstable\": true} or run discovery first"}
		}
		return nil
	}

	var problems []string
	for i := range plan.TestCases {
		tc := &plan.TestCases[i]
		for j := range tc.Steps {
			if ref, ok := targetRef(tc.Steps[j].Target); ok {
				if _, known := refs[ref]; !known {
					problems = append(problems, fmt.Sprintf(
						"%s step %d: unknown_element_ref: no element %q is in this run's application map",
						tc.ID, j, ref))
				}
			}
		}
		for j := range tc.Assertions {
			if ref, ok := targetRef(tc.Assertions[j].Target); ok {
				if _, known := refs[ref]; !known {
					problems = append(problems, fmt.Sprintf(
						"%s assertion %d: unknown_element_ref: no element %q is in this run's application map",
						tc.ID, j, ref))
				}
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func planUsesRefs(plan *qaschema.TestPlan) bool {
	for i := range plan.TestCases {
		tc := &plan.TestCases[i]
		for j := range tc.Steps {
			if _, ok := targetRef(tc.Steps[j].Target); ok {
				return true
			}
		}
		for j := range tc.Assertions {
			if _, ok := targetRef(tc.Assertions[j].Target); ok {
				return true
			}
		}
	}
	return false
}

func targetRef(t *qaschema.Target) (string, bool) {
	if t == nil || t.Ref == nil {
		return "", false
	}
	return *t.Ref, true
}

// elementRefs is every ref the map names, flattened across pages.
func elementRefs(appMap *qaschema.ApplicationMap) map[string]struct{} {
	if appMap == nil {
		return nil
	}
	out := map[string]struct{}{}
	for _, page := range appMap.Pages {
		for _, element := range page.Elements {
			out[element.Ref] = struct{}{}
		}
	}
	return out
}

// egressFor is the allowlist a plan's navigate steps are held to: the run's own
// base URL host and nothing else.
//
// Derived from the run rather than configured, so a run can never be told that
// more of the internet is in scope than the run is about. Private addresses are
// allowed because a staging environment on a customer's LAN is the case this
// product exists to serve, and the host is pinned to one name anyway.
func egressFor(baseURL string) *security.EgressPolicy {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	policy, err := security.NewEgressPolicy(security.EgressRules{
		Allow:                []string{parsed.Hostname()},
		AllowPrivateNetworks: true,
	})
	if err != nil {
		return nil
	}
	return policy
}

// fixtureNames is what the planner is told it may reference, and
// knownFixtures is what its answer is held to.
//
// Both come from the run assignment — the backend's fixture registry for this
// project (daemon-envelope@1 1.1.0) — so a name the model was offered can
// never be a name the gate rejects. Deriving one of them from this machine
// instead would make the two disagree whenever the machine and the project
// were configured differently, and the model would be blamed for the gap.
//
// The registry says which fixtures a project has DECLARED, which is a
// different question from whether this machine can establish one. That second
// question is the executor's, and it answers it at the moment a precondition
// runs, with a fixture-unavailable failure naming the fixture.
func (rc *runController) fixtureNames() []string {
	return rc.payload.Fixtures
}

func (rc *runController) knownFixtures() map[string]struct{} {
	// An empty (non-nil) set rather than nil: PlanGate skips the fixture rule
	// entirely when KnownFixtures is nil, and "this project has registered no
	// fixtures" must mean every fixture reference is rejected, not that none
	// is checked.
	out := make(map[string]struct{}, len(rc.payload.Fixtures))
	for _, name := range rc.payload.Fixtures {
		out[name] = struct{}{}
	}
	return out
}

// narratePlan puts the shape of an accepted plan on the run's event stream.
//
// Counts and category names only. The plan's own prose is model output written
// after reading the application under test, and this event goes to the backend
// as a message a browser renders.
func (rc *runController) narratePlan(plan *qaschema.TestPlan) {
	byCategory := map[string]int{}
	byPriority := map[string]int{}
	unstable := 0
	for i := range plan.TestCases {
		tc := &plan.TestCases[i]
		byCategory[string(tc.Category)]++
		byPriority[string(tc.Priority)]++
		for j := range tc.Steps {
			if t := tc.Steps[j].Target; t != nil && t.Locator != nil {
				unstable++
			}
		}
	}

	var missing []string
	for _, category := range qaschema.TestCaseCategoryValues {
		if byCategory[string(category)] == 0 {
			missing = append(missing, string(category))
		}
	}

	message := fmt.Sprintf("planned %d test cases across %d of %d categories",
		len(plan.TestCases), len(qaschema.TestCaseCategoryValues)-len(missing),
		len(qaschema.TestCaseCategoryValues))
	level := qaschema.RunEventPayloadLevelInfo
	if len(missing) > 0 {
		// Not an error: a read-only application honestly has no validation to
		// test. It is a warning because the usual cause is a model that
		// stopped early, and that is worth a reviewer's eye.
		level = qaschema.RunEventPayloadLevelWarn
		message += "; nothing for " + strings.Join(missing, ", ")
	}

	rc.emit(level, "plan_summary", message, map[string]any{
		"testCases":         len(plan.TestCases),
		"byCategory":        byCategory,
		"byPriority":        byPriority,
		"missingCategories": missing,
		"unstableTargets":   unstable,
	})
}
