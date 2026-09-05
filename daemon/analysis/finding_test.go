package analysis

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// Every document this package writes is validated against finding@1 before it
// leaves. The prose is composed from executor messages and request URLs, so a
// maxLength is a limit this code can cross without noticing — and the run would
// then fail at ingest, hours later, with the workspace already swept.
func TestEncodeValidatesAgainstTheContract(t *testing.T) {
	document, err := Encode(Verdict{
		TestCaseRef:  "TC-001",
		FailureClass: qaschema.FailureClassNETWORKERROR,
		Confidence:   0.95,
		Summary:      "the request never completed",
		RootCause:    "GET http://app/ produced no response at all",
		StepIndex:    ptr(1),
		Evidence:     []string{"e0-screenshot-0"},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	finding := decodeFinding(t, document)
	if finding.FailureClass != qaschema.FailureClassNETWORKERROR || finding.Confidence != 0.95 {
		t.Fatalf("finding = %+v", finding)
	}
}

// finding@1 makes stepIndex required and nullable: a whole-case finding must
// carry an explicit null, and the generated struct's omitempty tag would drop
// it. This is why the package writes its own wire type.
func TestEncodeKeepsAnExplicitNullStepIndex(t *testing.T) {
	document, err := Encode(Verdict{
		TestCaseRef:  "TC-001",
		FailureClass: qaschema.FailureClassUNKNOWN,
		RootCause:    "the case failed before its first step ran",
		Evidence:     []string{"e0-screenshot-0"},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := fields["stepIndex"]
	if !ok {
		t.Fatalf("stepIndex was dropped: %s", document)
	}
	if string(raw) != "null" {
		t.Fatalf("stepIndex = %s, want null", raw)
	}
}

// A finding with nothing to cite is a guess, and the contract says so.
func TestEncodeRefusesAFindingWithNoEvidence(t *testing.T) {
	_, err := Encode(Verdict{
		TestCaseRef:  "TC-001",
		FailureClass: qaschema.FailureClassUNKNOWN,
		RootCause:    "no idea",
	})
	if err == nil {
		t.Fatal("a finding with no evidence was encoded")
	}
}

// Prose composed from a page's own URLs can run long; it is clipped to the
// contract's limit rather than failing the run.
func TestEncodeClipsOverlongProse(t *testing.T) {
	document, err := Encode(Verdict{
		TestCaseRef:  "TC-001",
		FailureClass: qaschema.FailureClassNETWORKERROR,
		Summary:      strings.Repeat("x", 900),
		RootCause:    strings.Repeat("y", 9000),
		Evidence:     []string{"e0-screenshot-0"},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	finding := decodeFinding(t, document)
	if len(*finding.Summary) != 500 || len(finding.RootCause) != 8000 {
		t.Fatalf("summary %d, rootCause %d", len(*finding.Summary), len(finding.RootCause))
	}
}

func TestApplyConfidenceFloor(t *testing.T) {
	tests := []struct {
		name       string
		class      string
		confidence float64
		want       qaschema.FailureClass
		downgraded bool
	}{
		{"a confident verdict is kept", "PRODUCT_BUG", 0.9, qaschema.FailureClassPRODUCTBUG, false},
		{"a verdict exactly at the floor is kept", "PRODUCT_BUG", MinConfidence, qaschema.FailureClassPRODUCTBUG, false},
		{"a guess becomes UNKNOWN", "PRODUCT_BUG", 0.2, qaschema.FailureClassUNKNOWN, true},
		{"a low-confidence TEST_BUG becomes UNKNOWN too", "TEST_BUG", 0.1, qaschema.FailureClassUNKNOWN, true},
		{"an UNKNOWN that was already UNKNOWN is not reported as downgraded", "UNKNOWN", 0.1, qaschema.FailureClassUNKNOWN, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			document, err := json.Marshal(map[string]any{
				"version": 1, "testCaseId": "TC-001", "stepIndex": nil,
				"failureClass": tc.class, "rootCause": "because", "confidence": tc.confidence,
				"evidence": []string{"e0-screenshot-0"}, "suggestedFix": "look at the handler",
			})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			out, downgraded, err := ApplyConfidenceFloor([]json.RawMessage{document}, MinConfidence)
			if err != nil {
				t.Fatalf("ApplyConfidenceFloor: %v", err)
			}
			finding := decodeFinding(t, out[0])
			if finding.FailureClass != tc.want {
				t.Fatalf("class = %s, want %s", finding.FailureClass, tc.want)
			}
			if (len(downgraded) == 1) != tc.downgraded {
				t.Fatalf("downgraded = %v, want %v", downgraded, tc.downgraded)
			}

			// Only the class moves. The reasoning, the evidence, the blamed
			// step and the confidence number are what the report shows, and a
			// downgrade that rewrote them would be inventing a different
			// verdict rather than being honest about this one.
			if finding.Confidence != tc.confidence || finding.RootCause != "because" {
				t.Fatalf("the downgrade rewrote more than the class: %+v", finding)
			}
			if finding.SuggestedFix == nil || *finding.SuggestedFix != "look at the handler" {
				t.Fatalf("suggestedFix was lost: %+v", finding)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(out[0], &fields); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(fields["stepIndex"]) != "null" {
				t.Fatalf("the null stepIndex did not survive the downgrade: %s", out[0])
			}
		})
	}
}

// The guarantee that does not depend on the model.
func TestCoverGapsGivesEveryFailureAFinding(t *testing.T) {
	bundles := []Bundle{bundleFor("TC-001"), bundleFor("TC-002"), bundleFor("TC-003")}
	answered, err := Encode(Verdict{
		TestCaseRef: "TC-002", FailureClass: qaschema.FailureClassPRODUCTBUG,
		RootCause: "500 from the API", Confidence: 0.9, Evidence: []string{"e0-screenshot-0"},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	out, filled, err := CoverGaps([]json.RawMessage{answered}, bundles, "the analyst returned no finding for it")
	if err != nil {
		t.Fatalf("CoverGaps: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("findings = %d, want one per failed execution", len(out))
	}
	if len(filled) != 2 || filled[0] != "TC-001" || filled[1] != "TC-003" {
		t.Fatalf("filled = %v", filled)
	}

	for _, document := range out[1:] {
		finding := decodeFinding(t, document)
		if finding.FailureClass != qaschema.FailureClassUNKNOWN {
			t.Fatalf("a synthesised finding claimed %s", finding.FailureClass)
		}
		if finding.Confidence != 0 {
			t.Fatalf("a synthesised finding claimed confidence %v", finding.Confidence)
		}
		// Loud rather than plausible: it says the analysis did not conclude,
		// and cites the bundle so the reader can look at what it was given.
		if !strings.Contains(finding.RootCause, "did not produce a usable verdict") {
			t.Fatalf("the synthesised root cause reads like a real verdict: %q", finding.RootCause)
		}
		if len(finding.Evidence) == 0 {
			t.Fatalf("a synthesised finding cites nothing: %+v", finding)
		}
	}
}

// A finding already present is never duplicated by the gap filler.
func TestCoverGapsLeavesAnsweredFailuresAlone(t *testing.T) {
	answered, err := Encode(Verdict{
		TestCaseRef: "TC-001", FailureClass: qaschema.FailureClassPRODUCTBUG,
		RootCause: "500", Confidence: 0.9, Evidence: []string{"e0-screenshot-0"},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	out, filled, err := CoverGaps([]json.RawMessage{answered}, []Bundle{bundleFor("TC-001")}, "n/a")
	if err != nil {
		t.Fatalf("CoverGaps: %v", err)
	}
	if len(out) != 1 || len(filled) != 0 {
		t.Fatalf("out = %d, filled = %v", len(out), filled)
	}
}

// An execution with nothing citable cannot have a finding@1 written for it. The
// caller gives every bundle an evidence artifact so this cannot happen; if it
// somehow has, saying so is the only honest option left.
func TestCoverGapsReportsAFailureItCannotWriteAFindingFor(t *testing.T) {
	b := bundleFor("TC-001")
	b.Artifacts = nil

	if _, _, err := CoverGaps(nil, []Bundle{b}, "n/a"); err == nil {
		t.Fatal("a failure with no citable evidence was silently skipped")
	}
}

// Every rule verdict encodes. A rule that composed a document the contract
// rejects would fail the run at the last possible moment.
func TestEveryRuleVerdictEncodes(t *testing.T) {
	network := bundleFor("TC-001")
	network.Network = []NetworkEntry{{Method: "GET", URL: "http://app/", Status: nil}}
	auth := bundleFor("TC-002")
	auth.Network = []NetworkEntry{{Method: "GET", URL: "http://app/api/me", Status: status(401)}}
	timeout := bundleFor("TC-003")
	timeout.Execution.Message = ptr("Timeout 30000ms exceeded")
	environment := bundleFor("TC-004")
	environment.Execution.FailureClass = ptr(qaschema.FailureClassENVIRONMENTERROR)

	decided, ambiguous := Partition([]Bundle{network, auth, timeout, environment})
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %+v", ambiguous)
	}
	documents, err := EncodeAll(decided)
	if err != nil {
		t.Fatalf("EncodeAll: %v", err)
	}
	if len(documents) != 4 {
		t.Fatalf("documents = %d", len(documents))
	}
}
