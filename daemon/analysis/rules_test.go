package analysis

import (
	"testing"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The rule pass, as a table: what evidence produces which class, and — as
// importantly — what evidence produces no rule at all.
func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Bundle)
		// want is the class, or the empty string when no rule should fire and
		// the failure belongs to the model.
		want qaschema.FailureClass
		rule string
	}{
		{
			name: "a request that never came back is a network error",
			setup: func(b *Bundle) {
				b.Network = []NetworkEntry{{Method: "GET", URL: "http://app/employees", Status: nil}}
			},
			want: qaschema.FailureClassNETWORKERROR,
			rule: RuleTransportFailure,
		},
		{
			name: "the browser's own transport error is a network error",
			setup: func(b *Bundle) {
				b.Execution.Message = ptr("page.goto: net::ERR_CONNECTION_REFUSED at http://app/")
			},
			want: qaschema.FailureClassNETWORKERROR,
			rule: RuleTransportFailure,
		},
		{
			name: "a 401 is an authentication error",
			setup: func(b *Bundle) {
				b.Network = []NetworkEntry{{Method: "GET", URL: "http://app/api/me", Status: status(401)}}
			},
			want: qaschema.FailureClassAUTHENTICATIONERROR,
			rule: RuleUnauthorized,
		},
		{
			name: "a 403 is an authentication error too",
			setup: func(b *Bundle) {
				b.Network = []NetworkEntry{{Method: "POST", URL: "http://app/api/employees", Status: status(403)}}
			},
			want: qaschema.FailureClassAUTHENTICATIONERROR,
			rule: RuleUnauthorized,
		},
		{
			name: "a deadline with no failing request is a timeout",
			setup: func(b *Bundle) {
				b.Execution.Message = ptr("locator.click: Timeout 30000ms exceeded")
				b.Network = []NetworkEntry{}
			},
			want: qaschema.FailureClassTIMEOUT,
			rule: RuleDeadline,
		},
		{
			// The guard that keeps a 500 visible. A page slow because a
			// request failed and the retry is spinning ends in a deadline,
			// and filing that as TIMEOUT hides the 500 from the report.
			name: "a deadline with a failing request behind it is not a timeout",
			setup: func(b *Bundle) {
				b.Execution.Message = ptr("locator.click: Timeout 30000ms exceeded")
				b.Network = []NetworkEntry{{Method: "POST", URL: "http://app/api/employees", Status: status(500)}}
			},
			want: "",
		},
		{
			name: "an executor-declared environment error is kept",
			setup: func(b *Bundle) {
				b.Execution.FailureClass = ptr(qaschema.FailureClassENVIRONMENTERROR)
				b.Execution.Message = ptr("the browser could not be launched")
			},
			want: qaschema.FailureClassENVIRONMENTERROR,
			rule: RuleExecutorClassified,
		},
		{
			// The two classes that are a judgement call are never laundered
			// into a rule's confidence, whoever asserted them.
			name: "an executor-declared product bug still goes to the model",
			setup: func(b *Bundle) {
				b.Execution.FailureClass = ptr(qaschema.FailureClassPRODUCTBUG)
			},
			want: "",
		},
		{
			name: "an executor-declared UNKNOWN is not a classification",
			setup: func(b *Bundle) {
				b.Execution.FailureClass = ptr(qaschema.FailureClassUNKNOWN)
			},
			want: "",
		},
		{
			// The acceptance case: a 500 is the judgement the model is for.
			name: "a 500 goes to the model",
			setup: func(b *Bundle) {
				b.Network = []NetworkEntry{{Method: "POST", URL: "http://app/api/employees", Status: status(500)}}
			},
			want: "",
		},
		{
			name: "a locator that stopped matching goes to the model",
			setup: func(b *Bundle) {
				b.FailedStep.Message = ptr("locator resolved to 0 elements")
			},
			want: "",
		},
		{
			name:  "a bare failure with no signal goes to the model",
			setup: func(*Bundle) {},
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := bundleFor("TC-001")
			tc.setup(&b)

			verdict, decided := Classify(b)
			if tc.want == "" {
				if decided {
					t.Fatalf("a rule fired (%s -> %s) on a failure the model should judge",
						verdict.Rule, verdict.FailureClass)
				}
				return
			}
			if !decided {
				t.Fatalf("no rule fired, want %s", tc.want)
			}
			if verdict.FailureClass != tc.want {
				t.Fatalf("class = %s, want %s", verdict.FailureClass, tc.want)
			}
			if verdict.Rule != tc.rule {
				t.Fatalf("rule = %q, want %q", verdict.Rule, tc.rule)
			}
			if verdict.RootCause == "" {
				t.Fatal("a verdict nobody can check is the same problem as an invented one")
			}
			if len(verdict.Evidence) == 0 {
				t.Fatal("a rule verdict must cite the evidence it read")
			}
		})
	}
}

// Auth is checked before the timeout: a session that expired produces a 401 and
// then a wait that times out looking for a page that will never load, and
// reporting the wait is reporting the symptom.
func TestUnauthorizedBeatsTimeout(t *testing.T) {
	b := bundleFor("TC-001")
	b.Execution.Message = ptr("locator.click: Timeout 30000ms exceeded")
	b.Network = []NetworkEntry{{Method: "GET", URL: "http://app/api/me", Status: status(401)}}

	verdict, decided := Classify(b)
	if !decided || verdict.FailureClass != qaschema.FailureClassAUTHENTICATIONERROR {
		t.Fatalf("verdict = %+v, want the 401 to win", verdict)
	}
}

// A page that renders the word "timeout" must not be able to classify its own
// failure. Only executor-authored messages are searched.
func TestPageContentCannotTriggerARule(t *testing.T) {
	b := bundleFor("TC-001")
	b.Execution.Message = ptr("the assertion did not hold")
	b.FailedStep.Message = nil
	b.FailedAssertions = []qaschema.AssertionResult{{
		Index: 0, Type: qaschema.AssertionTypeTextContains, Status: qaschema.OutcomeFail,
		Actual: ptr("Request timeout: net::ERR_CONNECTION_REFUSED — please retry"),
	}}

	if verdict, decided := Classify(b); decided {
		t.Fatalf("page text classified its own failure as %s via %s", verdict.FailureClass, verdict.Rule)
	}
}

// A bundle with nothing to cite cannot produce a finding@1 at all, so it is
// never claimed by a rule.
func TestClassifyRefusesABundleWithNoEvidence(t *testing.T) {
	b := bundleFor("TC-001")
	b.Artifacts = nil
	b.Network = []NetworkEntry{{Method: "GET", URL: "http://app/", Status: nil}}

	if _, decided := Classify(b); decided {
		t.Fatal("a rule claimed a bundle it cannot write a finding for")
	}
}

// Partition is what decides whether an AI CLI is started at all.
func TestPartitionSeparatesTheModelsWork(t *testing.T) {
	network := bundleFor("TC-001")
	network.Network = []NetworkEntry{{Method: "GET", URL: "http://app/", Status: nil}}
	product := bundleFor("TC-002")
	product.Network = []NetworkEntry{{Method: "POST", URL: "http://app/api/employees", Status: status(500)}}

	decided, ambiguous := Partition([]Bundle{network, product})
	if len(decided) != 1 || decided[0].TestCaseRef != "TC-001" {
		t.Fatalf("decided = %+v", decided)
	}
	if len(ambiguous) != 1 || ambiguous[0].TestCaseRef != "TC-002" {
		t.Fatalf("ambiguous = %+v", ambiguous)
	}
}

// The acceptance criterion behind the rule pass: a run whose every failure is
// network, auth or timeout leaves nothing for a model to do.
func TestARunOfNetworkFailuresNeedsNoModel(t *testing.T) {
	var bundles []Bundle
	for _, ref := range []string{"TC-001", "TC-002", "TC-003"} {
		b := bundleFor(ref)
		b.Network = []NetworkEntry{{Method: "GET", URL: "http://app/", Status: nil}}
		bundles = append(bundles, b)
	}

	decided, ambiguous := Partition(bundles)
	if len(decided) != 3 || len(ambiguous) != 0 {
		t.Fatalf("decided %d, ambiguous %d; want every failure decided by rule", len(decided), len(ambiguous))
	}
}
