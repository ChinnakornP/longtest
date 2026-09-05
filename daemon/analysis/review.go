package analysis

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The gate every AI-authored finding passes before it leaves this machine.
//
// It is the analysis counterpart of the plan gate in daemon/runtime/plan.go and
// of ReviewPlan on the server, and it is deliberately the same shape: a stable
// Rule string, a Rejection carrying where the problem is, and a rendering of
// the rejections that goes back to the model as its next attempt's feedback.
// Somebody watching run events should not have to learn a second error format
// to read an analysis rejection after learning the planning one.
//
// What the schema cannot say, and this must:
//
//   - the evidence a finding cites has to be an artifact that execution
//     actually produced. finding@1 requires at least one artifact id and says
//     nothing about whether it names a real file, so a model that invents
//     `screenshot-9` writes a perfectly valid finding@1 pointing at nothing.
//   - the step it blames has to be a step that test case actually has.
//   - the case it is about has to be one this run asked about.
//   - every failed execution has to get exactly one.
//
// The first is the one that matters most. A fabricated evidence id survives
// ingest silently — the backend links only the handles it stored, so the
// finding lands with an empty evidence list and reads, in the report, like a
// conclusion nobody bothered to support.

// Rule identifies why an analysis result was refused. Constants because they
// reach run events and the retry prompt, where a renamed string is a broken
// feedback loop.
const (
	// RuleSchema is a document that is not an array of findings at all.
	RuleSchema = "schema_invalid"
	// RuleUnknownEvidence is the important one: an artifactId that the
	// execution being explained did not produce.
	RuleUnknownEvidence = "unknown_evidence_artifact"
	// RuleUnknownStep is a stepIndex past the end of that case's steps.
	RuleUnknownStep = "unknown_step_index"
	// RuleUnknownTestCase is a finding about a case this analysis did not ask
	// about — a passing case, or one that is not in this run.
	RuleUnknownTestCase = "unknown_test_case"
	// RuleDuplicateFinding is two findings for one execution.
	RuleDuplicateFinding = "duplicate_finding_for_test_case"
	// RuleMissingFinding is a failed execution the analyst said nothing about.
	RuleMissingFinding = "failed_execution_without_finding"
)

// Rejection is one reason an analysis result was refused.
//
// Same fields and same JSON names as testcase.Rejection on the server. They are
// separate types because they are separate contracts that happen to agree today
// — the daemon does not import the server — but a reader of run events sees one
// shape.
type Rejection struct {
	// TestCaseID is the case at fault, empty for a result-level problem.
	TestCaseID string `json:"testCaseId,omitempty"`
	// StepIndex is the offending step, or -1 when the problem is the finding
	// rather than one step of it.
	StepIndex int `json:"stepIndex"`
	// Rule names the check, stable enough to alert on.
	Rule string `json:"rule"`
	// Detail is the human-readable reason.
	Detail string `json:"detail"`
}

func (r Rejection) String() string {
	loc := r.TestCaseID
	if loc == "" {
		loc = "analysis"
	}
	if r.StepIndex >= 0 {
		loc = fmt.Sprintf("%s step %d", loc, r.StepIndex)
	}
	return fmt.Sprintf("%s: %s: %s", loc, r.Rule, r.Detail)
}

// Context is what an analysis result is checked against: this run's own
// executions and test cases, and nothing the model wrote.
type Context struct {
	cases map[string]caseFacts
}

type caseFacts struct {
	artifacts map[string]struct{}
	steps     int
}

// NewContext builds the fact base from the bundles the model was asked about.
//
// Only those bundles. A context built from every execution would accept a
// finding about a case that passed, and "TC-014 failed because…" on a green row
// is worse than no finding at all.
func NewContext(bundles []Bundle) Context {
	cases := make(map[string]caseFacts, len(bundles))
	for _, b := range bundles {
		artifacts := make(map[string]struct{}, len(b.Artifacts))
		for _, artifact := range b.Artifacts {
			artifacts[artifact.ID] = struct{}{}
		}
		cases[b.TestCaseRef] = caseFacts{artifacts: artifacts, steps: b.StepCount()}
	}
	return Context{cases: cases}
}

// Review checks one analysis result — the whole out.json array — and returns
// every problem with it.
//
// Every problem, not the first: a rejection costs a full attempt, and a retry
// that fixes one citation and trips over the next burns three attempts on what
// one round of feedback could have fixed.
func (c Context) Review(document []byte) []Rejection {
	var findings []qaschema.Finding
	if err := json.Unmarshal(document, &findings); err != nil {
		// Unreachable: the schema check runs first and this is the same
		// document. Reported rather than ignored, because "the gate did not
		// run" must never look like "the gate passed".
		return []Rejection{{
			StepIndex: -1, Rule: RuleSchema,
			Detail: fmt.Sprintf("the analysis result could not be re-read for review: %v", err),
		}}
	}

	var rejections []Rejection
	reject := func(r Rejection) { rejections = append(rejections, r) }

	seen := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		facts, known := c.cases[finding.TestCaseID]
		if !known {
			reject(Rejection{
				TestCaseID: finding.TestCaseID, StepIndex: -1, Rule: RuleUnknownTestCase,
				Detail: "this analysis was not asked about that test case; it either passed or is not in this run",
			})
			continue
		}
		if _, repeat := seen[finding.TestCaseID]; repeat {
			reject(Rejection{
				TestCaseID: finding.TestCaseID, StepIndex: -1, Rule: RuleDuplicateFinding,
				Detail: "one execution gets one finding; say it once, with every artifact it rests on in evidence",
			})
			continue
		}
		seen[finding.TestCaseID] = struct{}{}

		checkEvidence(reject, finding, facts)
		checkStepIndex(reject, finding, facts)
	}

	// Coverage last, so the model reads its citation problems before it reads
	// what it left out.
	for _, ref := range sortedKeys(c.cases) {
		if _, ok := seen[ref]; ok {
			continue
		}
		reject(Rejection{
			TestCaseID: ref, StepIndex: -1, Rule: RuleMissingFinding,
			Detail: "this execution failed and the result says nothing about it; every failure gets a finding, " +
				"and UNKNOWN with the reason is the answer when the evidence does not support one",
		})
	}

	sortRejections(rejections)
	return rejections
}

func checkEvidence(reject func(Rejection), finding qaschema.Finding, facts caseFacts) {
	for _, id := range finding.Evidence {
		if _, ok := facts.artifacts[id]; ok {
			continue
		}
		reject(Rejection{
			TestCaseID: finding.TestCaseID, StepIndex: -1, Rule: RuleUnknownEvidence,
			Detail: fmt.Sprintf(
				"evidence %q is not an artifact this execution produced; cite only ids from its artifacts list", id),
		})
	}
}

func checkStepIndex(reject func(Rejection), finding qaschema.Finding, facts caseFacts) {
	if finding.StepIndex == nil {
		// A whole-case finding. Legitimate and common: a case that failed
		// before its first step ran has no step to blame.
		return
	}
	index := *finding.StepIndex
	if index >= 0 && index < facts.steps {
		return
	}
	reject(Rejection{
		TestCaseID: finding.TestCaseID, StepIndex: index, Rule: RuleUnknownStep,
		Detail: fmt.Sprintf("that case has %d step(s), indexed 0..%d; use null when the failure belongs to the case "+
			"rather than to one step", facts.steps, facts.steps-1),
	})
}

// Problems renders the rejections one per line — the shape [agent.Task.Review]
// wants, and the shape the retry prompt frames as untrusted feedback.
func Problems(rejections []Rejection) []string {
	if len(rejections) == 0 {
		return nil
	}
	out := make([]string, len(rejections))
	for i, rejection := range rejections {
		out[i] = rejection.String()
	}
	return out
}

// ReviewHook is this context as [agent.Task.Review] takes it.
//
// Wiring the check here rather than after the phase is what puts it inside the
// retry loop: a rejected analysis is re-asked for with the reasons attached,
// framed as untrusted like any other model-adjacent text, instead of ending the
// run on the first bad citation.
//
// It is a pure function of the bytes, as [agent.Task.Review] requires: the
// context is read-only and nothing here records that it ran.
func (c Context) ReviewHook() func([]byte) []string {
	return func(document []byte) []string { return Problems(c.Review(document)) }
}

func sortedKeys(m map[string]caseFacts) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortRejections orders by case then rule then step, so the same bad result
// produces the same feedback every time. A retry prompt that reshuffles between
// attempts is one more thing that varies when a model's answer changes.
func sortRejections(in []Rejection) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].TestCaseID != in[j].TestCaseID {
			return in[i].TestCaseID < in[j].TestCaseID
		}
		if in[i].Rule != in[j].Rule {
			return in[i].Rule < in[j].Rule
		}
		if in[i].StepIndex != in[j].StepIndex {
			return in[i].StepIndex < in[j].StepIndex
		}
		return in[i].Detail < in[j].Detail
	})
}
