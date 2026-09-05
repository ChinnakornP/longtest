package run

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/testcase"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// The evidence graph a result frame carries, checked before any of it is
// written.
//
// reviewEvidence is pure so it can be tested exactly here: the rules it
// enforces are about the frame's own consistency, and none of them needs a
// database to state or to break.

// testPrefix is the shape artifact.KeyPrefix produces. Written out rather than
// built, so a change to the layout shows up as a failure here rather than as
// two functions quietly agreeing on a new one.
const testPrefix = "orgs/11111111-1111-4111-8111-111111111111/runs/2026-09-05/22222222-2222-4222-8222-222222222222/"

func shot(id, caseRef, name string) qaschema.Artifact {
	return qaschema.Artifact{
		ID:   id,
		Kind: qaschema.ArtifactKindScreenshot,
		Key:  testPrefix + caseRef + "/" + name,
	}
}

func finding(caseRef string, evidence ...string) qaschema.Finding {
	return qaschema.Finding{
		Version:      1,
		TestCaseID:   caseRef,
		StepIndex:    intPtr(1),
		FailureClass: qaschema.FailureClassPRODUCTBUG,
		RootCause:    "the row never appeared",
		Confidence:   0.9,
		Evidence:     evidence,
	}
}

func intPtr(v int) *int { return &v }

func TestReviewEvidence(t *testing.T) {
	tests := []struct {
		name    string
		payload resultPayload
		// wantRules is the rule of each rejection, in order. Empty means the
		// frame is accepted.
		wantRules []string
		// wantDetail is a substring every rejection detail together must
		// contain — the handle or key that tells an operator which two things
		// collided.
		wantDetail []string
	}{
		{
			// What a daemon at or after LONG-17 sends. Every executor-minted
			// handle is namespaced `e{n}-`, and each execution's evidence is
			// listed twice on purpose: once run-level, once inside the
			// execution. Those are one artifact, not a collision.
			name: "a namespaced frame, with each artifact listed at both levels",
			payload: resultPayload{
				Artifacts: []qaschema.Artifact{
					shot("e0-screenshot-0", "TC-001", "shot.png"),
					shot("e1-screenshot-0", "TC-002", "shot.png"),
					{ID: "analysis-0", Kind: qaschema.ArtifactKindReport, Key: testPrefix + "TC-001/evidence.json"},
				},
				Executions: []qaschema.ExecutionResult{
					{TestCaseID: "TC-001", Artifacts: []qaschema.Artifact{shot("e0-screenshot-0", "TC-001", "shot.png")}},
					{TestCaseID: "TC-002", Artifacts: []qaschema.Artifact{shot("e1-screenshot-0", "TC-002", "shot.png")}},
				},
				Findings: []qaschema.Finding{
					finding("TC-001", "e0-screenshot-0", "analysis-0"),
					finding("TC-002", "e1-screenshot-0"),
				},
			},
		},
		{
			// The frame a daemon before LONG-17 sends: the executor's counter
			// restarts at zero for every case, so forty cases produce forty
			// artifacts called screenshot-0. Ingest keyed one run-wide map on
			// them and the last one won.
			name: "an un-namespaced frame from an older daemon",
			payload: resultPayload{
				Artifacts: []qaschema.Artifact{
					shot("screenshot-0", "TC-001", "shot.png"),
					shot("screenshot-0", "TC-002", "shot.png"),
				},
				Findings: []qaschema.Finding{finding("TC-001", "screenshot-0")},
			},
			wantRules:  []string{RuleDuplicateArtifactHandle},
			wantDetail: []string{`"screenshot-0"`, "TC-001/shot.png", "TC-002/shot.png"},
		},
		{
			// The collision inside an execution's own list, which is the other
			// place handles are minted.
			name: "a handle reused across two executions",
			payload: resultPayload{
				Executions: []qaschema.ExecutionResult{
					{TestCaseID: "TC-001", Artifacts: []qaschema.Artifact{shot("screenshot-0", "TC-001", "shot.png")}},
					{TestCaseID: "TC-002", Artifacts: []qaschema.Artifact{shot("screenshot-0", "TC-002", "shot.png")}},
				},
			},
			wantRules:  []string{RuleDuplicateArtifactHandle},
			wantDetail: []string{"TC-001/shot.png", "TC-002/shot.png"},
		},
		{
			// The same handle for the same object is what a normal frame does.
			// Only a handle naming two DIFFERENT objects is a violation.
			name: "the same handle for the same key is one artifact, not two",
			payload: resultPayload{
				Artifacts: []qaschema.Artifact{
					shot("e0-screenshot-0", "TC-001", "shot.png"),
					shot("e0-screenshot-0", "TC-001", "shot.png"),
				},
			},
		},
		{
			name: "a finding citing a handle the frame does not carry",
			payload: resultPayload{
				Artifacts: []qaschema.Artifact{shot("e0-screenshot-0", "TC-001", "shot.png")},
				Findings:  []qaschema.Finding{finding("TC-001", "e7-screenshot-0")},
			},
			wantRules:  []string{RuleUnknownEvidenceHandle},
			wantDetail: []string{`"e7-screenshot-0"`},
		},
		{
			// finding@1 says minItems 1. The envelope validator checks that on
			// the way in; this is the same rule where the row would be written,
			// so a loosened schema or a second caller cannot get past it.
			name: "a finding citing nothing at all",
			payload: resultPayload{
				Findings: []qaschema.Finding{finding("TC-001")},
			},
			wantRules: []string{RuleFindingWithoutEvidence},
		},
		{
			// Every problem in one pass: an operator fixing a broken producer
			// should see all of it, not the first one and then the next after
			// a re-run.
			name: "several problems are all reported",
			payload: resultPayload{
				Artifacts: []qaschema.Artifact{
					shot("screenshot-0", "TC-001", "shot.png"),
					shot("screenshot-0", "TC-002", "shot.png"),
				},
				Findings: []qaschema.Finding{
					finding("TC-001", "nope-1"),
					finding("TC-002"),
				},
			},
			wantRules: []string{
				RuleDuplicateArtifactHandle,
				RuleUnknownEvidenceHandle,
				RuleFindingWithoutEvidence,
			},
		},
		{
			// A run-level artifact — a discovery HAR — has no {testCaseRef}
			// segment. Naming the collision must still work rather than read
			// the file name as a case ref.
			name: "a run-level artifact colliding with a case's",
			payload: resultPayload{
				Artifacts: []qaschema.Artifact{
					{ID: "har-0", Kind: qaschema.ArtifactKindNetwork, Key: testPrefix + "discovery.har"},
					{ID: "har-0", Kind: qaschema.ArtifactKindNetwork, Key: testPrefix + "TC-001/second.har"},
				},
			},
			wantRules:  []string{RuleDuplicateArtifactHandle},
			wantDetail: []string{"discovery.har", "TC-001/second.har"},
		},
		{
			name:    "an empty frame has nothing to refuse",
			payload: resultPayload{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewEvidence(tc.payload, testPrefix)

			rules := make([]string, len(got))
			details := make([]string, len(got))
			for i, rejection := range got {
				rules[i] = rejection.Rule
				details[i] = rejection.Detail
			}
			if len(rules) != len(tc.wantRules) {
				t.Fatalf("rules = %v, want %v", rules, tc.wantRules)
			}
			for i := range rules {
				if rules[i] != tc.wantRules[i] {
					t.Fatalf("rules = %v, want %v", rules, tc.wantRules)
				}
			}

			joined := strings.Join(details, " | ")
			for _, want := range tc.wantDetail {
				if !strings.Contains(joined, want) {
					t.Errorf("no rejection detail names %q; details were %s", want, joined)
				}
			}
		})
	}
}

// A duplicate handle names the case at fault and the step convention
// testcase.Rejection uses, so an alert on the run event reads the same
// whichever review produced it.
func TestReviewEvidenceRejectionCarriesTheSharedShape(t *testing.T) {
	got := reviewEvidence(resultPayload{
		Artifacts: []qaschema.Artifact{
			shot("screenshot-0", "TC-001", "shot.png"),
			shot("screenshot-0", "TC-002", "shot.png"),
		},
		Findings: []qaschema.Finding{
			{Version: 1, TestCaseID: "TC-003", FailureClass: qaschema.FailureClassUNKNOWN, RootCause: "?"},
		},
	}, testPrefix)
	if len(got) != 2 {
		t.Fatalf("got %d rejections, want 2", len(got))
	}

	// The collision is reported against the case whose artifact lost, which is
	// the second one — the first is the entry already bound.
	if got[0].TestCaseID != "TC-002" {
		t.Errorf("the duplicate names %q, want the case whose artifact would have been overwritten", got[0].TestCaseID)
	}
	// -1 is testcase.Rejection's "the case, not one step of it". A finding
	// with no stepIndex is a whole-case verdict and has to render the same way.
	if got[0].StepIndex != -1 || got[1].StepIndex != -1 {
		t.Errorf("step indexes = %d, %d; want -1 for a problem that is not a step's",
			got[0].StepIndex, got[1].StepIndex)
	}

	// The wire shape is testcase.Rejection's, not a second one invented here:
	// the plan retry prompt, the run event and any alert all read these names.
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal rejection: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode rejection: %v", err)
	}
	for _, name := range []string{"testCaseId", "stepIndex", "rule", "detail"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("the rejection has no %q field; it must be the same JSON as testcase.Rejection", name)
		}
	}
	if len(fields) != 4 {
		t.Errorf("the rejection carries %d fields: %s", len(fields), encoded)
	}
}

// The event a refused result leaves behind is the one a plan rejection leaves,
// with a different code: an alert matches on data.rejections[].rule, and a
// second shape for the second half of the same frame would need a second alert.
func TestResultRejectedEventMatchesThePlanRejectionShape(t *testing.T) {
	refused := &resultRejected{
		RunID: uuid.New(),
		Refused: []testcase.Rejection{
			{TestCaseID: "TC-002", StepIndex: -1, Rule: RuleDuplicateArtifactHandle, Detail: "..."},
			{TestCaseID: "TC-003", StepIndex: -1, Rule: RuleDuplicateArtifactHandle, Detail: "..."},
			{TestCaseID: "TC-004", StepIndex: 2, Rule: RuleUnknownEvidenceHandle, Detail: "..."},
		},
	}

	var _ rejectedFrame = refused
	var _ rejectedFrame = &planRejected{}

	event := refused.Event()
	if event.Code != "result_rejected" {
		t.Errorf("code = %q", event.Code)
	}
	if event.Level != dbgen.RunEventLevelError {
		t.Errorf("level = %q; a refused result is not an informational line", event.Level)
	}
	if event.Phase != qaschema.RunEventPayloadPhaseReport {
		t.Errorf("phase = %q, want the phase the report would have been assembled in", event.Phase)
	}
	if _, ok := event.Data["rejections"]; !ok {
		t.Error("the event carries no rejections, so nothing can alert on the rule")
	}
	if event.Data["stored"] != 0 {
		t.Errorf("stored = %v; a refused frame stores nothing", event.Data["stored"])
	}

	// The message names the rules and their counts, never a detail: a detail
	// can quote what a model wrote, and model output on a hijacked run is page
	// content.
	message := refused.Message()
	if !strings.Contains(message, RuleDuplicateArtifactHandle+" x2") ||
		!strings.Contains(message, RuleUnknownEvidenceHandle+" x1") {
		t.Errorf("message = %q, want the rules with their counts", message)
	}
	if strings.Contains(message, "...") {
		t.Errorf("message = %q, want no rejection detail in it", message)
	}
	if event.Message != message {
		t.Errorf("the event says %q and the run row says %q", event.Message, message)
	}
}

func TestCaseRefOfKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "a case's artifact", key: testPrefix + "TC-001/shot.png", want: "TC-001"},
		{name: "a run-level artifact", key: testPrefix + "discovery.har", want: ""},
		{name: "a key from another run", key: "orgs/other/runs/x/shot.png", want: "orgs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := caseRefOfKey(testPrefix, tc.key); got != tc.want {
				t.Fatalf("caseRefOfKey(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}
