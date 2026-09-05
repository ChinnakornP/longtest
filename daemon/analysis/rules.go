package analysis

import (
	"fmt"
	"strings"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The rule pass: the failures that do not need a model.
//
// Three of the seven failure classes are decidable from the evidence alone. A
// request that was refused at the transport is a NETWORK_ERROR whatever a model
// thinks of it; a 401 on the failing step is an AUTHENTICATION_ERROR; a
// deadline the executor reported and no error response is a TIMEOUT. Running a
// model over these is slower, costs tokens, and is less accurate than reading
// the status code — the model has to infer from prose what the rule reads off
// the wire.
//
// What is deliberately NOT here is the judgement call. A 500 during "create
// employee" is probably a product bug and might be a test posting a malformed
// body; a locator that stopped matching is probably a test bug and might be a
// button the product removed. Deciding those from evidence is what the model is
// for, and a rule that guessed at them would be wrong quietly and often.
//
// Every rule states the evidence it read. A verdict nobody can check is the
// same problem as a model that makes one up.

// Verdict is one classification this package produced without a model.
type Verdict struct {
	TestCaseRef  string
	FailureClass qaschema.FailureClass
	Summary      string
	RootCause    string
	Confidence   float64
	// StepIndex is the step blamed, or nil for a whole-case verdict.
	StepIndex *int
	// Evidence is the artifact ids cited. Always at least one: finding@1
	// requires it, and for good reason.
	Evidence []string
	// Rule names the check that fired, empty for a model-authored finding. It
	// is the same kind of stable string as testcase.Rule* on the server: it
	// ends up in run events, and a renamed one is a broken alert.
	Rule string
	// SuggestedFix is set only where the rule genuinely knows the fix.
	SuggestedFix string
}

// The rules, named so a run event and an alert can agree on what fired.
const (
	// RuleTransportFailure is a request that never produced a response.
	RuleTransportFailure = "network_request_never_completed"
	// RuleUnauthorized is a 401 or 403 on the failing step's traffic.
	RuleUnauthorized = "http_unauthorized"
	// RuleDeadline is the executor reporting that it ran out of time.
	RuleDeadline = "executor_reported_timeout"
	// RuleExecutorClassified is the executor having already classified the
	// failure itself — a browser that would not launch, a fixture that could
	// not be established. It knows things no later reader can reconstruct.
	RuleExecutorClassified = "executor_classified"
	// RuleNoAnalysis is the fallback verdict: the analyst was asked and did
	// not produce a usable answer. It is UNKNOWN and it is loud.
	RuleNoAnalysis = "analysis_unavailable"
)

// ruleConfidence is what a deterministic verdict claims.
//
// Not 1.0, and the distinction is real rather than modesty. The rule is certain
// about what it READ — this request returned 401 — and that is not the same as
// being certain about what it MEANS: a 401 during a login test may be the
// product rejecting a valid credential, which is a product bug wearing an auth
// failure's clothes. The number says "the evidence points here and a person
// looking at it would agree", which is what a reader needs it to say.
const (
	ruleConfidence     = 0.95
	executorConfidence = 0.9
)

// MinConfidence is the floor below which a model's own class is not kept.
//
// A finding under it is not dropped — nothing here ever drops a finding — it is
// recorded as UNKNOWN with the model's reasoning intact. The difference matters
// on a report: UNKNOWN says "we looked and could not tell", which is true and
// useful, while a PRODUCT_BUG the analyst was 20% sure of sends somebody to
// read code that was never broken.
const MinConfidence = 0.4

// Classify runs the rule pass over one bundle.
//
// The second return is whether a rule decided it. False means the bundle needs
// a model — that is the normal case for the failures worth analysing, and it is
// not a failure of this function.
func Classify(b Bundle) (Verdict, bool) {
	evidence := b.ArtifactIDs()
	if len(evidence) == 0 {
		// Nothing to cite means nothing that satisfies finding@1. The caller
		// gives every bundle a citable evidence artifact before it gets here;
		// a bundle that arrived without one is passed to the model, which will
		// be rejected by the gate and reported, rather than turned into a
		// finding that cannot be encoded.
		return Verdict{}, false
	}

	for _, rule := range []func(Bundle, []string) (Verdict, bool){
		classifyExecutor,
		classifyTransport,
		classifyUnauthorized,
		classifyTimeout,
	} {
		if verdict, ok := rule(b, evidence); ok {
			return verdict, true
		}
	}
	return Verdict{}, false
}

// Partition splits bundles into the ones the rules decided and the ones the
// model has to look at.
func Partition(bundles []Bundle) (decided []Verdict, ambiguous []Bundle) {
	for _, b := range bundles {
		if verdict, ok := Classify(b); ok {
			decided = append(decided, verdict)
			continue
		}
		ambiguous = append(ambiguous, b)
	}
	return decided, ambiguous
}

// classifyExecutor keeps a class the executor already set.
//
// The executor is not guessing when it does this: it sets a class from its own
// error code, for failures whose cause is visible only from inside the harness
// — the browser did not launch, the fixture could not be established, the
// sidecar died. Re-deriving that from the evidence is impossible, so a model
// asked to would invent something.
func classifyExecutor(b Bundle, evidence []string) (Verdict, bool) {
	class := b.Execution.FailureClass
	if class == nil || *class == qaschema.FailureClassUNKNOWN || !class.IsValid() {
		return Verdict{}, false
	}
	// PRODUCT_BUG and TEST_BUG are the judgement the model is for. An executor
	// that reported one is reporting a guess, and this pass does not launder a
	// guess into a rule's confidence.
	if *class == qaschema.FailureClassPRODUCTBUG || *class == qaschema.FailureClassTESTBUG {
		return Verdict{}, false
	}

	return Verdict{
		TestCaseRef:  b.TestCaseRef,
		FailureClass: *class,
		Rule:         RuleExecutorClassified,
		Confidence:   executorConfidence,
		Summary:      fmt.Sprintf("the executor classified this failure as %s", *class),
		RootCause: fmt.Sprintf(
			"The executor reported %s before any analysis ran: %s. This is a condition only the harness can observe — "+
				"it is not inferred from the page.",
			*class, executionMessage(b)),
		StepIndex: stepIndexOf(b),
		Evidence:  evidence,
	}, true
}

// classifyTransport fires when a request never produced a response.
//
// This is the least ambiguous signal in the whole run. The application under
// test did not answer wrongly; it did not answer. No amount of reading the DOM
// makes that a product bug in the sense a user cares about.
func classifyTransport(b Bundle, evidence []string) (Verdict, bool) {
	var dead []NetworkEntry
	for _, entry := range b.Network {
		if entry.Status == nil {
			dead = append(dead, entry)
		}
	}
	if len(dead) == 0 {
		if !mentionsTransportError(b) {
			return Verdict{}, false
		}
		return Verdict{
			TestCaseRef:  b.TestCaseRef,
			FailureClass: qaschema.FailureClassNETWORKERROR,
			Rule:         RuleTransportFailure,
			Confidence:   ruleConfidence,
			Summary:      "the browser reported a network-level error",
			RootCause: "The executor's message carries a browser transport error, which means the request did not " +
				"reach the application or its answer never arrived: " + executionMessage(b),
			StepIndex: stepIndexOf(b),
			Evidence:  evidence,
		}, true
	}

	return Verdict{
		TestCaseRef:  b.TestCaseRef,
		FailureClass: qaschema.FailureClassNETWORKERROR,
		Rule:         RuleTransportFailure,
		Confidence:   ruleConfidence,
		Summary:      fmt.Sprintf("%d request(s) never produced a response", len(dead)),
		RootCause: fmt.Sprintf(
			"%s produced no response at all — not an error status, no answer. The application under test was "+
				"unreachable or dropped the connection, so nothing it did or did not render is evidence of anything else.",
			describeRequests(dead)),
		StepIndex:    stepIndexOf(b),
		Evidence:     evidence,
		SuggestedFix: "Check that the application under test is running and reachable from the runtime, then re-run.",
	}, true
}

// classifyUnauthorized fires on a 401 or 403.
//
// Ahead of the timeout rule on purpose: a session that expired mid-run produces
// a 401 and then a wait that times out looking for a page that will never load,
// and reporting the wait is reporting the symptom.
func classifyUnauthorized(b Bundle, evidence []string) (Verdict, bool) {
	var denied []NetworkEntry
	for _, entry := range b.Network {
		if entry.Status != nil && (*entry.Status == 401 || *entry.Status == 403) {
			denied = append(denied, entry)
		}
	}
	if len(denied) == 0 {
		return Verdict{}, false
	}

	return Verdict{
		TestCaseRef:  b.TestCaseRef,
		FailureClass: qaschema.FailureClassAUTHENTICATIONERROR,
		Rule:         RuleUnauthorized,
		Confidence:   ruleConfidence,
		Summary:      fmt.Sprintf("%d request(s) were refused with 401/403", len(denied)),
		RootCause: fmt.Sprintf(
			"%s. The application refused the request on authentication or authorization grounds, so the run was not "+
				"in the state this case assumes and nothing after that point tested what it meant to test.",
			describeRequests(denied)),
		StepIndex: stepIndexOf(b),
		Evidence:  evidence,
		SuggestedFix: "Check the fixture this case depends on: an expired session or a changed credential produces " +
			"exactly this, and so does a permission the test account has lost.",
	}, true
}

// classifyTimeout fires when the executor ran out of time and nothing came back
// an error.
//
// The guard matters: a page that is slow because a request 500'd and the retry
// is spinning is a product failure that happens to end in a deadline, and
// filing it as TIMEOUT hides the 500 from the person reading the report.
func classifyTimeout(b Bundle, evidence []string) (Verdict, bool) {
	if !mentionsTimeout(b) {
		return Verdict{}, false
	}
	for _, entry := range b.Network {
		if entry.Failed() {
			return Verdict{}, false
		}
	}

	return Verdict{
		TestCaseRef:  b.TestCaseRef,
		FailureClass: qaschema.FailureClassTIMEOUT,
		Rule:         RuleDeadline,
		Confidence:   ruleConfidence,
		Summary:      "the step exceeded its deadline with no failing request",
		RootCause: "The executor gave up waiting: " + executionMessage(b) +
			". No request in this execution returned an error status, so the wait was for something the page never " +
			"reached rather than for a request that failed.",
		StepIndex: stepIndexOf(b),
		Evidence:  evidence,
	}, true
}

// transportTokens are the browser's own words for "this never got there". They
// are matched case-insensitively against the executor's message, which is
// executor-authored text and not page content.
var transportTokens = []string{
	"net::err_",
	"econnrefused",
	"econnreset",
	"enotfound",
	"eai_again",
	"ehostunreach",
	"connection refused",
	"connection reset",
	"dns",
}

var timeoutTokens = []string{
	"timeout",
	"timed out",
	"exceeded",
	"deadline",
}

func mentionsTransportError(b Bundle) bool { return mentions(b, transportTokens) }

func mentionsTimeout(b Bundle) bool { return mentions(b, timeoutTokens) }

// mentions searches the executor's own messages, never the page's.
//
// Only Execution.Message and StepResult.Message are read. Both are
// executor-authored summaries — the contract says so in as many words — and an
// assertion's `actual` is not, because that is a string the application under
// test chose. A page that renders the word "timeout" must not be able to change
// how its own failure is classified.
func mentions(b Bundle, tokens []string) bool {
	haystacks := []string{executionMessage(b)}
	if b.FailedStep != nil && b.FailedStep.Message != nil {
		haystacks = append(haystacks, *b.FailedStep.Message)
	}
	for _, haystack := range haystacks {
		lower := strings.ToLower(haystack)
		for _, token := range tokens {
			if strings.Contains(lower, token) {
				return true
			}
		}
	}
	return false
}

func executionMessage(b Bundle) string {
	if b.Execution.Message != nil && *b.Execution.Message != "" {
		return *b.Execution.Message
	}
	if b.FailedStep != nil && b.FailedStep.Message != nil {
		return *b.FailedStep.Message
	}
	return "the executor reported no message"
}

func stepIndexOf(b Bundle) *int {
	if b.FailedStep == nil {
		return nil
	}
	index := b.FailedStep.Index
	return &index
}

// describeRequests names at most three requests. The full list is in the
// evidence bundle the finding cites; a root cause is a sentence, not a log.
func describeRequests(entries []NetworkEntry) string {
	const shown = 3
	parts := make([]string, 0, shown)
	for i, entry := range entries {
		if i == shown {
			parts = append(parts, fmt.Sprintf("and %d more", len(entries)-shown))
			break
		}
		if entry.Status == nil {
			parts = append(parts, fmt.Sprintf("%s %s (no response)", entry.Method, entry.URL))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s returned %d", entry.Method, entry.URL, *entry.Status))
	}
	return strings.Join(parts, "; ")
}
