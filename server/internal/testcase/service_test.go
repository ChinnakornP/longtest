package testcase

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// The review lifecycle is deliberately not a free-for-all: an archived case is
// reinstated as a draft rather than jumping straight back into the regression
// suite a reviewer never re-read.
func TestAllowedTransition(t *testing.T) {
	const (
		draft    = dbgen.TestCaseStatusDraft
		approved = dbgen.TestCaseStatusApproved
		archived = dbgen.TestCaseStatusArchived
	)

	tests := []struct {
		name string
		from dbgen.TestCaseStatus
		to   dbgen.TestCaseStatus
		want bool
	}{
		{name: "a draft is approved", from: draft, to: approved, want: true},
		{name: "a draft is discarded", from: draft, to: archived, want: true},
		{name: "an approved case is retired", from: approved, to: archived, want: true},
		{name: "an approved case goes back for rework", from: approved, to: draft, want: true},
		{name: "an archived case is reopened as a draft", from: archived, to: draft, want: true},
		{name: "an archived case cannot rejoin the suite directly", from: archived, to: approved},
		{name: "an unknown status is never a source", from: dbgen.TestCaseStatus("bogus"), to: draft},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowedTransition(tc.from, tc.to); got != tc.want {
				t.Fatalf("allowedTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	for _, raw := range []string{"draft", "approved", "archived"} {
		if _, err := parseStatus(raw); err != nil {
			t.Errorf("parseStatus(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{"", "Draft", "blessed", "draft "} {
		if _, err := parseStatus(raw); err == nil {
			t.Errorf("parseStatus(%q) accepted it", raw)
		}
	}
}

// An absent filter means "every status", which is a different thing from a
// filter that happens to match nothing.
func TestParseStatusFilter(t *testing.T) {
	filter, err := parseStatusFilter("")
	if err != nil {
		t.Fatalf("empty filter: %v", err)
	}
	if filter.Valid {
		t.Fatal("an empty filter was turned into a status")
	}

	filter, err = parseStatusFilter("approved")
	if err != nil {
		t.Fatalf("approved filter: %v", err)
	}
	if !filter.Valid || filter.TestCaseStatus != dbgen.TestCaseStatusApproved {
		t.Fatalf("got %+v, want approved", filter)
	}

	if _, err := parseStatusFilter("nonsense"); err == nil {
		t.Fatal("accepted an unknown status as a filter")
	}
}

// --- payload edit validation ----------------------------------------------

// editableDocument is a minimal, contract-valid test-case@1 body, with the
// caller free to break one field of it.
func editableDocument(t *testing.T, mutate func(map[string]any)) json.RawMessage {
	t.Helper()

	document := map[string]any{
		"version":  1,
		"id":       "TC-001",
		"name":     "Log in with valid credentials",
		"priority": "high",
		"category": "functional",
		"steps": []any{
			map[string]any{"action": "navigate", "url": "/login"},
			map[string]any{"action": "click", "target": map[string]any{"ref": "login.submit"}},
		},
		"assertions": []any{
			map[string]any{"type": "urlMatches", "value": "/dashboard"},
		},
	}
	if mutate != nil {
		mutate(document)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the fixture document: %v", err)
	}
	return raw
}

// The contract check runs before any row is read, so every case here is decided
// without a database.
func TestValidatePayload(t *testing.T) {
	tests := []struct {
		name string
		edit PayloadEdit
		// wantStatus is 0 when the edit is expected to be accepted.
		wantStatus int
		// wantField is the key expected under details.fields, if any.
		wantField string
		// wantPointer is a JSON Pointer expected among details.errors, if any.
		wantPointer string
	}{
		{
			name: "a whole valid document is accepted",
			edit: PayloadEdit{BaseVersion: 1, Payload: editableDocument(t, nil)},
		},
		{
			name:       "an absent baseVersion is refused rather than assumed",
			edit:       PayloadEdit{Payload: editableDocument(t, nil)},
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "baseVersion",
		},
		{
			name:       "a negative baseVersion is refused",
			edit:       PayloadEdit{BaseVersion: -3, Payload: editableDocument(t, nil)},
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "baseVersion",
		},
		{
			name:       "an absent payload is refused",
			edit:       PayloadEdit{BaseVersion: 1},
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "payload",
		},
		{
			name:       "a blank payload is refused",
			edit:       PayloadEdit{BaseVersion: 1, Payload: json.RawMessage("  \n ")},
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "payload",
		},
		{
			name:        "unparsable JSON is a contract failure at the root",
			edit:        PayloadEdit{BaseVersion: 1, Payload: json.RawMessage("{not json")},
			wantStatus:  http.StatusUnprocessableEntity,
			wantPointer: "",
		},
		{
			name: "an action outside the v1 vocabulary is located by pointer",
			edit: PayloadEdit{BaseVersion: 1, Payload: editableDocument(t, func(d map[string]any) {
				d["steps"].([]any)[0].(map[string]any)["action"] = "teleport"
			})},
			wantStatus:  http.StatusUnprocessableEntity,
			wantPointer: "/steps/0",
		},
		{
			name: "a missing required field is located by pointer",
			edit: PayloadEdit{BaseVersion: 1, Payload: editableDocument(t, func(d map[string]any) {
				delete(d, "assertions")
			})},
			wantStatus:  http.StatusUnprocessableEntity,
			wantPointer: "",
		},
		{
			name: "a case with no steps at all is refused",
			edit: PayloadEdit{BaseVersion: 1, Payload: editableDocument(t, func(d map[string]any) {
				d["steps"] = []any{}
			})},
			wantStatus:  http.StatusUnprocessableEntity,
			wantPointer: "/steps",
		},
		{
			name: "a contract version other than 1 is refused",
			edit: PayloadEdit{BaseVersion: 1, Payload: editableDocument(t, func(d map[string]any) {
				d["version"] = 2
			})},
			wantStatus:  http.StatusUnprocessableEntity,
			wantPointer: "/version",
		},
		{
			name: "a field the contract does not define is refused",
			edit: PayloadEdit{BaseVersion: 1, Payload: editableDocument(t, func(d map[string]any) {
				d["runAsRoot"] = true
			})},
			wantStatus:  http.StatusUnprocessableEntity,
			wantPointer: "/runAsRoot",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			document, err := validatePayload(tc.edit)
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("validatePayload: %v", err)
				}
				if document.ID != "TC-001" {
					t.Fatalf("got id %q, want TC-001", document.ID)
				}
				return
			}

			var apiErr *httpx.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("got %v, want an *httpx.Error", err)
			}
			if apiErr.Status != tc.wantStatus {
				t.Fatalf("got status %d, want %d", apiErr.Status, tc.wantStatus)
			}
			if tc.wantField != "" {
				fields, _ := apiErr.Details["fields"].(map[string]string)
				if _, named := fields[tc.wantField]; !named {
					t.Fatalf("got details %+v, want a %q field", apiErr.Details, tc.wantField)
				}
				return
			}
			// Everything else is a contract failure, and a contract failure has
			// to say WHERE: an editor highlights the field from this pointer.
			problems, ok := apiErr.Details["errors"].([]qaschema.ValidationError)
			if !ok || len(problems) == 0 {
				t.Fatalf("got details %+v, want a non-empty errors list", apiErr.Details)
			}
			if !locatedAt(problems, tc.wantPointer) {
				t.Fatalf("no problem reported at %q; got %+v", tc.wantPointer, problems)
			}
		})
	}
}

func locatedAt(problems []qaschema.ValidationError, pointer string) bool {
	for _, problem := range problems {
		if strings.HasPrefix(problem.InstancePath, pointer) {
			return true
		}
	}
	return false
}

// A document can fail hundreds of keywords at once, and an error body larger
// than the request that caused it is a way to make this process allocate.
func TestSchemaViolationBoundsItsDetails(t *testing.T) {
	problems := make([]qaschema.ValidationError, maxSchemaProblems+7)
	for i := range problems {
		problems[i] = qaschema.ValidationError{InstancePath: "/steps/0", Keyword: "enum"}
	}

	err := schemaViolation(problems)
	if err.Status != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want 422", err.Status)
	}
	reported, ok := err.Details["errors"].([]qaschema.ValidationError)
	if !ok {
		t.Fatalf("got details %+v, want an errors list", err.Details)
	}
	if len(reported) != maxSchemaProblems {
		t.Fatalf("reported %d problems, want the cap of %d", len(reported), maxSchemaProblems)
	}
	// Truncation is reported, never silent: a client that sees 50 problems has
	// to know it is not looking at all of them.
	if got := err.Details["truncated"]; got != 7 {
		t.Fatalf("got truncated %v, want 7", got)
	}

	// A short list is reported whole, with no truncation key at all.
	short := schemaViolation(problems[:2])
	if _, present := short.Details["truncated"]; present {
		t.Fatal("a complete list was marked truncated")
	}
}
