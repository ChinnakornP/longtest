package testcase

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// Plan review: the gate every AI-authored test case passes before it is a row.
//
// The daemon has a gate of its own (security.PlanGate) and it runs first,
// which is what lets a bad plan be retried while the model is still there to
// retry it. This one is not a duplicate of it: it is the gate that knows
// things the daemon does not — which element refs this project's application
// map actually holds, which fixtures are registered, and which cases a human
// already approved. A daemon is a customer-side process holding a pairing
// token; "the daemon already checked" is not a reason for the backend to store
// what it is sent.
//
// The rule the whole file serves: the model produces DATA, and data that does
// not satisfy the contract does not become a row. Nothing here repairs a
// document — a plan we edited is not the plan the model wrote, and a
// half-accepted plan is worse than a rejected one, because nobody downstream
// can tell which half is missing.

// testPlanSchemaID is the contract a planner's out.json is validated against.
// Named here rather than inlined so a bump to test-plan@2 is one edit with a
// compiler error at every reader, instead of a string search.
const testPlanSchemaID = "test-plan@1"

// Rule identifies why a case was refused. They are constants because they end
// up in run events and in the retry prompt, where a renamed string is a broken
// feedback loop.
const (
	// RuleSchema is a document that is not a test-plan@1 at all.
	RuleSchema = "schema_invalid"
	// RuleUnknownElementRef is the important one: a target.ref that no element
	// of this project's application map carries. Left unchecked it becomes a
	// TARGET_NOT_FOUND at execution time, hours later, against a customer's
	// application, in a case a reviewer has already approved.
	RuleUnknownElementRef = "unknown_element_ref"
	RuleUnknownAction     = "unknown_action"
	RuleUnknownAssertion  = "unknown_assertion"
	RuleUnknownFixture    = "unknown_fixture"
	RulePreconditionShape = "precondition_not_a_fixture_ref"
	RuleUnstableLocator   = "unstable_locator_not_flagged"
	RuleDuplicateRef      = "duplicate_test_case_id"
	RuleNoTarget          = "step_needs_a_target"
)

// Rejection is one reason a plan was refused.
type Rejection struct {
	// TestCaseID is the case at fault, empty for a plan-level problem.
	TestCaseID string `json:"testCaseId,omitempty"`
	// StepIndex is the offending step, or -1 when the problem is the case
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
		loc = "plan"
	}
	if r.StepIndex >= 0 {
		loc = fmt.Sprintf("%s step %d", loc, r.StepIndex)
	}
	return fmt.Sprintf("%s: %s: %s", loc, r.Rule, r.Detail)
}

// PlanContext is everything a plan is checked against. All of it is read from
// this project's own rows; none of it comes from the plan or from the daemon.
type PlanContext struct {
	// ElementRefs is every element ref in the project's application map. An
	// empty set means the project has no map, which makes every by-ref target
	// unresolvable — and that is a rejection, not a pass: a plan written
	// against a map nobody has is a plan nobody can run.
	ElementRefs map[string]struct{}
	// Fixtures is the registered fixture names, without the `fixture:` prefix.
	Fixtures map[string]struct{}
	// ExistingByFingerprint maps the normalised fingerprint of a case this
	// project already has to that case, so a re-plan recognises it however the
	// model renamed and renumbered it this time.
	//
	// Every status, not just approved. An approved match must not be re-queued
	// for review, an archived one must not reappear after somebody retired it,
	// and a draft one must not be stored a second time under a second id —
	// which is what a re-plan produces, because a planner renumbers its cases
	// every run and the ref is therefore not a stable identity.
	ExistingByFingerprint map[string]ExistingCase
}

// ExistingCase is a case the project already has, as the dedupe sees it.
type ExistingCase struct {
	Ref string
	// Status is why it was skipped, which is the difference between "you
	// approved this last week" and "somebody archived this on purpose".
	Status string
}

// AcceptedCase is one case that survived review, ready to be stored.
type AcceptedCase struct {
	// Document is the exact bytes the planner wrote. It is stored verbatim:
	// re-encoding it would reorder its keys on every run and drop whatever a
	// newer minor version of the contract added.
	Document json.RawMessage
	Ref      string
	Name     string
	Priority qaschema.TestCasePriority
	Category qaschema.TestCaseCategory
}

// DuplicateCase is a planned case that is a re-derivation of one the project
// already has. It is dropped rather than stored: a second row for the same
// test is a second thing to review, run and explain.
type DuplicateCase struct {
	// PlannedRef is what this run called it, ExistingRef what it already is.
	PlannedRef  string `json:"plannedRef"`
	ExistingRef string `json:"existingRef"`
	// ExistingStatus says why it was dropped, which a reviewer reads
	// differently for an approved case than for an archived one.
	ExistingStatus string `json:"existingStatus"`
}

// PlanReview is the verdict on one plan.
//
// Accepted is non-empty only when Rejections is empty: a plan is taken whole
// or not at all. Half of a plan is a suite with holes in it that reads, to
// everyone downstream, exactly like a suite without them.
type PlanReview struct {
	Accepted   []AcceptedCase
	Duplicates []DuplicateCase
	Rejections []Rejection
	// Categories counts the reviewed cases per category, which is what makes
	// "the plan covers all five" checkable by the caller rather than asserted
	// by the model's own prose.
	Categories map[qaschema.TestCaseCategory]int
}

// OK reports whether the plan may be stored.
func (r PlanReview) OK() bool { return len(r.Rejections) == 0 }

// Problems renders the rejections one per line, for the run event and for the
// retry prompt the daemon builds.
func (r PlanReview) Problems() []string {
	out := make([]string, len(r.Rejections))
	for i, rejection := range r.Rejections {
		out[i] = rejection.String()
	}
	return out
}

// MissingCategories names the contract categories this plan produced nothing
// for, sorted. A plan is not rejected for a gap — the model may honestly have
// found nothing to validate on a read-only page — but the gap is reported, and
// the coverage endpoint turns it into a suggestion.
func (r PlanReview) MissingCategories() []qaschema.TestCaseCategory {
	var out []qaschema.TestCaseCategory
	for _, category := range qaschema.TestCaseCategoryValues {
		if r.Categories[category] == 0 {
			out = append(out, category)
		}
	}
	return out
}

// rawPlan is the plan as this package reads it: typed for the checks, raw for
// the storage. Both, because a check needs fields and storage needs bytes, and
// deriving the bytes back from the fields is the lossy step this avoids.
type rawPlan struct {
	TestCases []json.RawMessage `json:"testCases"`
}

// ReviewPlan checks a test-plan@1 document against what this project actually
// has, and returns the cases that may be stored.
//
// It reports every problem rather than stopping at the first. A plan is fixed
// by asking the model again, and a retry that fixes one field and hits the
// next is a slow, expensive loop — three attempts of a full context each.
func ReviewPlan(document []byte, ctx PlanContext) PlanReview {
	review := PlanReview{Categories: map[qaschema.TestCaseCategory]int{}}
	reject := func(r Rejection) { review.Rejections = append(review.Rejections, r) }

	// The schema first, and on its own: every check below reads fields by
	// name, and running them over a document that is not a test plan produces
	// a page of confusing rejections instead of the one true one.
	result, err := qaschema.ValidateJSON(testPlanSchemaID, document)
	if err != nil {
		reject(Rejection{StepIndex: -1, Rule: RuleSchema, Detail: err.Error()})
		return review
	}
	if !result.Valid {
		for _, problem := range result.Errors {
			reject(Rejection{StepIndex: -1, Rule: RuleSchema, Detail: problem.String()})
		}
		return review
	}

	var typed qaschema.TestPlan
	var raw rawPlan
	if err := json.Unmarshal(document, &typed); err != nil {
		reject(Rejection{StepIndex: -1, Rule: RuleSchema, Detail: "test plan is not decodable: " + err.Error()})
		return review
	}
	if err := json.Unmarshal(document, &raw); err != nil {
		reject(Rejection{StepIndex: -1, Rule: RuleSchema, Detail: "test plan is not decodable: " + err.Error()})
		return review
	}
	if len(raw.TestCases) != len(typed.TestCases) {
		// Unreachable through one schema-valid document; checked because the
		// two decodes are what pairs a set of bytes with the fields the checks
		// below run on, and a mispairing would validate case A and store B.
		reject(Rejection{StepIndex: -1, Rule: RuleSchema,
			Detail: "the plan's test cases do not decode consistently"})
		return review
	}

	seen := make(map[string]struct{}, len(typed.TestCases))
	for i := range typed.TestCases {
		tc := &typed.TestCases[i]
		if _, repeat := seen[tc.ID]; repeat {
			reject(Rejection{TestCaseID: tc.ID, StepIndex: -1, Rule: RuleDuplicateRef,
				Detail: "two cases in this plan carry the same id"})
		}
		seen[tc.ID] = struct{}{}
		checkCase(&review.Rejections, tc, ctx)
	}
	if !review.OK() {
		sortRejections(review.Rejections)
		return review
	}

	for i := range typed.TestCases {
		tc := &typed.TestCases[i]
		review.Categories[tc.Category]++

		if existing, duplicate := ctx.ExistingByFingerprint[Fingerprint(tc)]; duplicate {
			review.Duplicates = append(review.Duplicates, DuplicateCase{
				PlannedRef: tc.ID, ExistingRef: existing.Ref, ExistingStatus: existing.Status,
			})
			continue
		}
		review.Accepted = append(review.Accepted, AcceptedCase{
			Document: raw.TestCases[i],
			Ref:      tc.ID,
			Name:     tc.Name,
			Priority: tc.Priority,
			Category: tc.Category,
		})
	}
	return review
}

// checkCase runs every per-case rule.
func checkCase(out *[]Rejection, tc *qaschema.TestCase, ctx PlanContext) {
	add := func(r Rejection) { *out = append(*out, r) }

	for _, precondition := range tc.Preconditions {
		name, ok := strings.CutPrefix(precondition, "fixture:")
		if !ok {
			// The schema pattern already forbids this shape. It is re-checked
			// because this is the rule that keeps a raw credential out of a
			// precondition, and a control that holds only while another
			// control holds is not a control.
			add(Rejection{TestCaseID: tc.ID, StepIndex: -1, Rule: RulePreconditionShape,
				Detail: "a precondition must be a fixture reference, never a literal login"})
			continue
		}
		if _, known := ctx.Fixtures[name]; !known {
			add(Rejection{TestCaseID: tc.ID, StepIndex: -1, Rule: RuleUnknownFixture,
				Detail: fmt.Sprintf("no fixture named %q is registered for this project", name)})
		}
	}

	for i := range tc.Steps {
		checkStep(out, tc.ID, i, &tc.Steps[i], ctx)
	}
	for i := range tc.Assertions {
		checkAssertion(out, tc.ID, i, &tc.Assertions[i], ctx)
	}
}

func checkStep(out *[]Rejection, caseID string, idx int, step *qaschema.Step, ctx PlanContext) {
	add := func(r Rejection) { *out = append(*out, r) }

	if !knownAction(step.Action) {
		add(Rejection{TestCaseID: caseID, StepIndex: idx, Rule: RuleUnknownAction,
			Detail: fmt.Sprintf("action %q is not in the v1 vocabulary (%s)", step.Action, joinActions())})
		return
	}
	if step.Target == nil {
		if targetlessAction(step.Action) {
			return
		}
		add(Rejection{TestCaseID: caseID, StepIndex: idx, Rule: RuleNoTarget,
			Detail: fmt.Sprintf("a %s step must name a target", step.Action)})
		return
	}
	checkTarget(out, caseID, idx, step.Target, ctx)
}

func checkAssertion(out *[]Rejection, caseID string, idx int, assertion *qaschema.Assertion, ctx PlanContext) {
	if !knownAssertion(assertion.Type) {
		*out = append(*out, Rejection{TestCaseID: caseID, StepIndex: -1, Rule: RuleUnknownAssertion,
			Detail: fmt.Sprintf("assertion %d uses type %q, which is not in the v1 vocabulary (%s)",
				idx, assertion.Type, joinAssertions())})
		return
	}
	// urlMatches, httpStatusNot and noConsoleError are about the page rather
	// than about an element, and carry no target to resolve.
	if assertion.Target == nil {
		return
	}
	checkTarget(out, caseID, -1, assertion.Target, ctx)
}

// checkTarget is the ref existence check this whole gate exists for.
func checkTarget(out *[]Rejection, caseID string, idx int, target *qaschema.Target, ctx PlanContext) {
	add := func(r Rejection) { *out = append(*out, r) }

	if target.Ref != nil {
		ref := *target.Ref
		if _, known := ctx.ElementRefs[ref]; !known {
			add(Rejection{TestCaseID: caseID, StepIndex: idx, Rule: RuleUnknownElementRef,
				Detail: fmt.Sprintf("no element %q exists in this project's application map", ref)})
		}
		return
	}
	if target.Locator == nil {
		// oneOf in the schema makes this unreachable; same reasoning as the
		// precondition shape check above.
		add(Rejection{TestCaseID: caseID, StepIndex: idx, Rule: RuleNoTarget,
			Detail: "a target must carry either a ref or a locator"})
		return
	}
	if target.Unstable == nil || !*target.Unstable {
		add(Rejection{TestCaseID: caseID, StepIndex: idx, Rule: RuleUnstableLocator,
			Detail: "a raw locator must be flagged unstable:true so the run is reportable as non-deterministic"})
	}
}

// targetlessAction reports whether an action is legitimately targetless:
// navigate points at a URL, screenshot at the whole viewport, and press may
// send a key to whatever holds focus.
func targetlessAction(a qaschema.StepAction) bool {
	switch a {
	case qaschema.StepActionNavigate, qaschema.StepActionScreenshot, qaschema.StepActionPress:
		return true
	default:
		return false
	}
}

func knownAction(a qaschema.StepAction) bool {
	for _, known := range qaschema.StepActionValues {
		if known == a {
			return true
		}
	}
	return false
}

func knownAssertion(a qaschema.AssertionType) bool {
	for _, known := range qaschema.AssertionTypeValues {
		if known == a {
			return true
		}
	}
	return false
}

func joinActions() string {
	out := make([]string, len(qaschema.StepActionValues))
	for i, v := range qaschema.StepActionValues {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

func joinAssertions() string {
	out := make([]string, len(qaschema.AssertionTypeValues))
	for i, v := range qaschema.AssertionTypeValues {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

func sortRejections(in []Rejection) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].TestCaseID != in[j].TestCaseID {
			return in[i].TestCaseID < in[j].TestCaseID
		}
		return in[i].StepIndex < in[j].StepIndex
	})
}

// --- dedupe ---------------------------------------------------------------

// Fingerprint is the identity of a test case as a *behaviour*, not as a
// document.
//
// Two cases with the same fingerprint drive the browser through the same
// actions against the same elements and check the same things. Everything a
// re-plan is free to change without changing the test is excluded: the id
// (a fresh plan renumbers), the name and description (a fresh plan rephrases),
// the priority and category (a judgement the model makes again each time), the
// tags, and every timeoutMs and message.
//
// That exclusion list is the whole feature. A dedupe on the id would be
// defeated by TC-004 becoming TC-007; a dedupe on the raw bytes would be
// defeated by a comma. Either would mean every planning run refills a
// reviewer's queue with the cases they approved last week.
func Fingerprint(tc *qaschema.TestCase) string {
	sum := sha256.Sum256(canonicalJSON(normalise(tc)))
	return hex.EncodeToString(sum[:])
}

// FingerprintDocument is Fingerprint for a stored test-case@1 payload. A
// document that does not decode has no behaviour to compare, so it gets no
// fingerprint and simply never matches.
func FingerprintDocument(document []byte) (string, bool) {
	var tc qaschema.TestCase
	if err := json.Unmarshal(document, &tc); err != nil {
		return "", false
	}
	return Fingerprint(&tc), true
}

// normalStep and normalAssertion are the significant fields, in a fixed order.
// Structs rather than maps, so the field order is the declaration order and
// the encoding cannot shift under a Go release.
type normalStep struct {
	Action     string `json:"a"`
	Target     string `json:"t,omitempty"`
	URL        string `json:"u,omitempty"`
	Value      string `json:"v,omitempty"`
	Key        string `json:"k,omitempty"`
	State      string `json:"s,omitempty"`
	By         string `json:"b,omitempty"`
	Button     string `json:"btn,omitempty"`
	ClickCount int    `json:"cc,omitempty"`
	Checked    *bool  `json:"c,omitempty"`
	FullPage   *bool  `json:"f,omitempty"`
}

type normalAssertion struct {
	Type     string   `json:"a"`
	Target   string   `json:"t,omitempty"`
	Value    string   `json:"v,omitempty"`
	Operator string   `json:"o,omitempty"`
	Ignore   []string `json:"i,omitempty"`
}

type normalCase struct {
	Preconditions []string          `json:"p,omitempty"`
	Steps         []normalStep      `json:"s"`
	Assertions    []normalAssertion `json:"x"`
}

func normalise(tc *qaschema.TestCase) normalCase {
	out := normalCase{
		Steps:      make([]normalStep, 0, len(tc.Steps)),
		Assertions: make([]normalAssertion, 0, len(tc.Assertions)),
	}
	// Copied rather than aliased: the sort below must not reorder the caller's
	// document. Preconditions are a set — establishing a login and seeding data
	// in the other order is the same starting state — but the document they
	// came from is stored verbatim, and reordering it here would change the
	// bytes a client validates against the contract.
	out.Preconditions = append(out.Preconditions, tc.Preconditions...)
	sort.Strings(out.Preconditions)

	for i := range tc.Steps {
		s := &tc.Steps[i]
		step := normalStep{
			Action:  string(s.Action),
			Target:  normalTarget(s.Target),
			Checked: s.Checked,
		}
		if s.URL != nil {
			step.URL = *s.URL
		}
		if s.Value != nil {
			step.Value = *s.Value
		}
		if s.Key != nil {
			step.Key = *s.Key
		}
		if s.State != nil {
			step.State = string(*s.State)
		}
		if s.By != nil {
			step.By = string(*s.By)
		}
		if s.Button != nil {
			step.Button = string(*s.Button)
		}
		if s.ClickCount != nil {
			step.ClickCount = *s.ClickCount
		}
		if s.FullPage != nil {
			step.FullPage = s.FullPage
		}
		out.Steps = append(out.Steps, step)
	}

	for i := range tc.Assertions {
		a := &tc.Assertions[i]
		assertion := normalAssertion{
			Type:   string(a.Type),
			Target: normalTarget(a.Target),
			Value:  scalarValue(a.Value),
			Ignore: append([]string(nil), a.IgnorePatterns...),
		}
		if a.Operator != nil {
			assertion.Operator = string(*a.Operator)
		}
		out.Assertions = append(out.Assertions, assertion)
	}
	return out
}

// normalTarget flattens a target to one comparable string. A ref and a raw
// locator are namespaced apart, so `ref:x` and `locator:x` never collide.
func normalTarget(t *qaschema.Target) string {
	switch {
	case t == nil:
		return ""
	case t.Ref != nil:
		return "ref:" + *t.Ref
	case t.Locator != nil:
		return "locator:" + *t.Locator
	default:
		return ""
	}
}

// scalarValue renders an assertion's value canonically, so the 3 a model wrote
// as `3` and the one it wrote as `3.0` fingerprint the same.
func scalarValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	if s, ok := decoded.(string); ok {
		return s
	}
	return string(canonicalJSON(decoded))
}

// canonicalJSON encodes without HTML escaping and without a trailing newline,
// so the bytes hashed depend only on the value.
func canonicalJSON(v any) []byte {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// Every type reachable here is a struct of strings, ints and bools.
		return []byte(fmt.Sprintf("%v", v))
	}
	return []byte(strings.TrimSuffix(sb.String(), "\n"))
}
