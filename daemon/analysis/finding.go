package analysis

import (
	"encoding/json"
	"fmt"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// Encoding findings, and the two guarantees that hold whatever the model did.

// findingSchemaID is the contract every document here is validated against.
// Named rather than inlined so a bump to finding@2 is one edit with a compiler
// error at each reader instead of a string search.
const findingSchemaID = "finding@1"

// wireFinding is finding@1 as it goes on the wire.
//
// It exists because qaschema.Finding cannot round-trip one. The contract makes
// `stepIndex` required and nullable — a finding that blames no single step must
// carry `"stepIndex": null`, and that is a real answer rather than a missing
// field — but the generated struct tags it omitempty, so encoding a whole-case
// finding through it drops the property and produces a document that no longer
// validates. Same reason daemon/runtime keeps forwarded findings as raw bytes.
type wireFinding struct {
	Version      int      `json:"version"`
	TestCaseID   string   `json:"testCaseId"`
	ExecutionID  *string  `json:"executionId,omitempty"`
	StepIndex    *int     `json:"stepIndex"`
	FailureClass string   `json:"failureClass"`
	Summary      string   `json:"summary,omitempty"`
	RootCause    string   `json:"rootCause"`
	Confidence   float64  `json:"confidence"`
	Evidence     []string `json:"evidence"`
	SuggestedFix string   `json:"suggestedFix,omitempty"`
}

// Encode renders a verdict as a validated finding@1 document.
//
// It validates its own output rather than trusting the struct tags. This
// package composes root-cause prose from executor messages and request URLs,
// and a maxLength the contract sets is a limit this code can cross without
// noticing — at which point the run would fail at ingest, hours later, with the
// analysis workspace already swept.
func Encode(v Verdict) (json.RawMessage, error) {
	if len(v.Evidence) == 0 {
		return nil, fmt.Errorf("analysis: a finding for %s cites no evidence", v.TestCaseRef)
	}
	document, err := json.Marshal(wireFinding{
		Version:      1,
		TestCaseID:   v.TestCaseRef,
		StepIndex:    v.StepIndex,
		FailureClass: string(v.FailureClass),
		Summary:      truncate(v.Summary, 500),
		RootCause:    truncate(v.RootCause, 8000),
		Confidence:   v.Confidence,
		Evidence:     v.Evidence,
		SuggestedFix: truncate(v.SuggestedFix, 8000),
	})
	if err != nil {
		return nil, fmt.Errorf("analysis: encode the finding for %s: %w", v.TestCaseRef, err)
	}

	result, err := qaschema.ValidateJSON(findingSchemaID, document)
	if err != nil {
		return nil, fmt.Errorf("analysis: validate the finding for %s: %w", v.TestCaseRef, err)
	}
	if !result.Valid {
		return nil, fmt.Errorf("analysis: the finding for %s does not match %s: %s",
			v.TestCaseRef, findingSchemaID, result.Errors[0].String())
	}
	return document, nil
}

// EncodeAll renders every verdict, stopping at the first that cannot be.
func EncodeAll(verdicts []Verdict) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(verdicts))
	for _, verdict := range verdicts {
		document, err := Encode(verdict)
		if err != nil {
			return nil, err
		}
		out = append(out, document)
	}
	return out, nil
}

// ApplyConfidenceFloor downgrades a low-confidence class to UNKNOWN and returns
// the refs it changed.
//
// The one transform this package performs on model output, and it is narrow on
// purpose: the class becomes UNKNOWN and nothing else moves. The reasoning, the
// evidence, the blamed step and the confidence number are all kept exactly as
// written, so the report still shows what the analyst thought and how sure it
// was. A finding is never dropped and never rewritten into a different verdict.
//
// Why downgrade at all, when confidence is displayed anyway: PRODUCT_BUG is not
// a number on a screen, it is a routing decision. It sends somebody to read
// application code. A 0.2-confidence PRODUCT_BUG spends an engineer's afternoon
// on the strength of a guess, and UNKNOWN with the same prose spends five
// minutes and reaches the same place.
func ApplyConfidenceFloor(documents []json.RawMessage, floor float64) ([]json.RawMessage, []string, error) {
	out := make([]json.RawMessage, 0, len(documents))
	var downgraded []string

	for _, document := range documents {
		var finding qaschema.Finding
		if err := json.Unmarshal(document, &finding); err != nil {
			return nil, nil, fmt.Errorf("analysis: re-read a finding to apply the confidence floor: %w", err)
		}
		if finding.Confidence >= floor || finding.FailureClass == qaschema.FailureClassUNKNOWN {
			out = append(out, document)
			continue
		}

		// Rewritten as a field edit on the parsed document rather than by
		// re-encoding the struct: re-encoding would drop a null stepIndex and
		// anything a newer minor version of the contract added.
		patched, err := setFailureClass(document, qaschema.FailureClassUNKNOWN)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, patched)
		downgraded = append(downgraded, finding.TestCaseID)
	}
	return out, downgraded, nil
}

func setFailureClass(document []byte, class qaschema.FailureClass) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return nil, fmt.Errorf("analysis: re-read a finding to downgrade its class: %w", err)
	}
	encoded, err := json.Marshal(string(class))
	if err != nil {
		return nil, fmt.Errorf("analysis: encode the downgraded class: %w", err)
	}
	fields["failureClass"] = encoded
	patched, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("analysis: re-encode a downgraded finding: %w", err)
	}
	return patched, nil
}

// CoverGaps appends an UNKNOWN finding for every bundle the documents do not
// already cover, and returns the refs it had to cover.
//
// This is the guarantee that does not depend on the model: a failed execution
// always leaves the daemon with a finding. The gate asks the analyst for full
// coverage and retries when it does not deliver, but retries run out, and the
// alternative at that point is a report where some red rows carry an
// explanation and others carry nothing — which reads, to the person looking at
// it, exactly like the rows nobody thought were worth explaining.
//
// The findings it writes are loud rather than plausible. They say the analysis
// did not produce an answer, and they cite the evidence bundle, so the reader
// is one click from what the analyst was looking at when it failed to conclude.
func CoverGaps(documents []json.RawMessage, bundles []Bundle, reason string) ([]json.RawMessage, []string, error) {
	covered := make(map[string]struct{}, len(documents))
	for _, document := range documents {
		var finding qaschema.Finding
		if err := json.Unmarshal(document, &finding); err != nil {
			return nil, nil, fmt.Errorf("analysis: re-read a finding to check coverage: %w", err)
		}
		covered[finding.TestCaseID] = struct{}{}
	}

	out := documents
	var filled []string
	for _, b := range bundles {
		if _, ok := covered[b.TestCaseRef]; ok {
			continue
		}
		evidence := b.ArtifactIDs()
		if len(evidence) == 0 {
			// Nothing citable means no finding@1 is possible for this
			// execution. The caller gives every bundle an evidence artifact
			// precisely so this cannot happen; if it somehow has, saying so is
			// the only honest option left.
			return nil, nil, fmt.Errorf(
				"analysis: %s failed and has no artifact to cite, so no finding can be written for it", b.TestCaseRef)
		}
		document, err := Encode(Verdict{
			TestCaseRef:  b.TestCaseRef,
			FailureClass: qaschema.FailureClassUNKNOWN,
			Rule:         RuleNoAnalysis,
			Confidence:   0,
			Summary:      "this failure was not classified",
			RootCause: "The failure analyst did not produce a usable verdict for this execution: " + reason +
				". The evidence bundle cited here is what it was given — the failing step, the assertions that " +
				"disagreed, the console errors and the failed requests — and it is complete whether or not the " +
				"analysis was. This finding is UNKNOWN because nothing concluded, not because the evidence was thin.",
			StepIndex: stepIndexOf(b),
			Evidence:  evidence,
		})
		if err != nil {
			return nil, nil, err
		}
		out = append(out, document)
		filled = append(filled, b.TestCaseRef)
	}
	return out, filled, nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
