package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// The test fixtures below build real contract documents rather than the
// smallest thing Go will marshal: the daemon validates every inbound frame, so
// an invalid fixture would be indistinguishable from a daemon that rejects
// good work.

func testAppMap(baseURL string) map[string]any {
	return map[string]any{
		"version": 1,
		"baseUrl": baseURL,
		"pages": []any{
			map[string]any{
				"id":           "page.employees",
				"path":         "/employees",
				"title":        "Employees",
				"authRequired": true,
				"elements": []any{
					map[string]any{
						"ref":           "emp.btn.add",
						"type":          "button",
						"label":         "Add Employee",
						"locators":      []any{map[string]any{"kind": "testId", "value": "add-emp"}},
						"lastSeenRunId": "018f3a90-11a2-7000-8000-0123456789ab",
					},
				},
			},
		},
		"workflows": []any{},
	}
}

func testCase(id, name string) map[string]any {
	return map[string]any{
		"version":  1,
		"id":       id,
		"name":     name,
		"priority": "high",
		"category": "functional",
		"steps": []any{
			map[string]any{"action": "navigate", "url": "/employees"},
			map[string]any{"action": "click", "target": map[string]any{"ref": "emp.btn.add"}},
		},
		"assertions": []any{
			map[string]any{"type": "visible", "target": map[string]any{"ref": "emp.btn.add"}},
		},
	}
}

type assignOptions struct {
	runID     string
	projectID string
	mode      qaschema.RunAssignPayloadMode
	baseURL   string
	keyPrefix string
	putBase   string
	expiresAt time.Time
	testCases []any
	withMap   bool
}

func assignFrame(t *testing.T, opts assignOptions) qaschema.Envelope {
	t.Helper()

	if opts.runID == "" {
		opts.runID = uuid.NewString()
	}
	if opts.projectID == "" {
		opts.projectID = uuid.NewString()
	}
	if opts.mode == "" {
		opts.mode = qaschema.RunAssignPayloadModeExecute
	}
	if opts.baseURL == "" {
		opts.baseURL = "http://app.internal:3000"
	}
	if opts.keyPrefix == "" {
		opts.keyPrefix = "orgs/testorg/runs/2026-09-04/" + shortID(opts.runID) + "/"
	}
	if opts.putBase == "" {
		opts.putBase = "http://storage.test/bucket"
	}
	if opts.expiresAt.IsZero() {
		opts.expiresAt = time.Now().Add(2 * time.Hour)
	}

	payload := map[string]any{
		"runId":     opts.runID,
		"mode":      string(opts.mode),
		"projectId": opts.projectID,
		"baseUrl":   opts.baseURL,
		"artifactUpload": map[string]any{
			"presignedPutBase": opts.putBase,
			"keyPrefix":        opts.keyPrefix,
			"expiresAt":        opts.expiresAt.UTC().Format(time.RFC3339),
		},
	}
	if opts.withMap {
		payload["appMap"] = testAppMap(opts.baseURL)
	}
	if len(opts.testCases) > 0 {
		payload["testCases"] = opts.testCases
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode assign payload: %v", err)
	}
	return qaschema.Envelope{
		V:       1,
		Type:    qaschema.EnvelopeTypeRunAssign,
		MsgID:   uuid.NewString(),
		RunID:   &opts.runID,
		Seq:     0,
		Ts:      time.Now().UTC().Format(time.RFC3339),
		Payload: raw,
	}
}

func cancelFrame(t *testing.T, runID string, reason qaschema.RunCancelPayloadReason, message string) qaschema.Envelope {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"reason": string(reason), "message": message})
	if err != nil {
		t.Fatalf("encode cancel payload: %v", err)
	}
	return qaschema.Envelope{
		V:       1,
		Type:    qaschema.EnvelopeTypeRunCancel,
		MsgID:   uuid.NewString(),
		RunID:   &runID,
		Seq:     0,
		Ts:      time.Now().UTC().Format(time.RFC3339),
		Payload: raw,
	}
}

// shortID makes a storage-key segment out of a uuid: the key pattern allows
// only [A-Za-z0-9._-], which a uuid satisfies, but a short segment keeps test
// output readable.
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return "run"
}

func decodeAs[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return out
}
