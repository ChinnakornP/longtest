package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ChinnakornP/longtest/server/internal/db"
)

// TestAsErrorMapsDomainErrors is the status-code contract of the whole API:
// every layer below returns a domain error, and this is the one function that
// decides what the client sees.
func TestAsErrorMapsDomainErrors(t *testing.T) {
	t.Parallel()

	// A realistic driver error, i.e. exactly the kind of value that must never
	// reach a response body.
	pgUnique := &pgconn.PgError{
		Code:           "23505",
		Message:        `duplicate key value violates unique constraint "users_email_key"`,
		ConstraintName: "users_email_key",
		TableName:      "users",
	}

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   Code
	}{
		{"api error passes through", NotFound("no such run"), http.StatusNotFound, CodeNotFound},
		{"wrapped api error", fmt.Errorf("load run: %w", Forbidden("nope")), http.StatusForbidden, CodeForbidden},
		{"not found", db.ErrNotFound, http.StatusNotFound, CodeNotFound},
		{"wrapped not found", fmt.Errorf("get project: %w", db.ErrNotFound), http.StatusNotFound, CodeNotFound},
		{"pgx no rows", pgx.ErrNoRows, http.StatusInternalServerError, CodeInternal},
		{"classified pgx no rows", db.Classify(pgx.ErrNoRows), http.StatusNotFound, CodeNotFound},
		{"conflict", db.ErrConflict, http.StatusConflict, CodeConflict},
		{"classified unique violation", db.Classify(pgUnique), http.StatusConflict, CodeConflict},
		{"invalid reference", db.ErrInvalidReference, http.StatusConflict, CodeConflict},
		{"invalid value", db.ErrInvalidValue, http.StatusUnprocessableEntity, CodeValidationFailed},
		{"serialization failure", db.ErrSerializationFailure, http.StatusConflict, CodeConflict},
		{"client hung up", context.Canceled, StatusClientClosedRequest, CodeUnavailable},
		{"deadline exceeded", context.DeadlineExceeded, http.StatusGatewayTimeout, CodeTimeout},
		{"anything else", errors.New("something we did not anticipate"), http.StatusInternalServerError, CodeInternal},
		{"raw driver error", pgUnique, http.StatusInternalServerError, CodeInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := AsError(tt.err)
			if got.Status != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", got.Status, tt.wantStatus)
			}
			if got.Code != tt.wantCode {
				t.Fatalf("code: got %q, want %q", got.Code, tt.wantCode)
			}
			if got.Message == "" {
				t.Fatal("every error must carry a message a client can show")
			}
		})
	}

	if AsError(nil) != nil {
		t.Fatal("AsError(nil) must be nil")
	}
}

// TestWriteErrorNeverLeaksDriverDetail is the guard the error contract exists
// for: a constraint name or a SQL fragment in a response body tells an
// attacker the schema.
func TestWriteErrorNeverLeaksDriverDetail(t *testing.T) {
	t.Parallel()

	driverErr := &pgconn.PgError{
		Code:           "23505",
		Message:        `duplicate key value violates unique constraint "runs_org_id_idempotency_key_key"`,
		Detail:         "Key (org_id, idempotency_key)=(4f2b, abc) already exists.",
		ConstraintName: "runs_org_id_idempotency_key_key",
		TableName:      "runs",
	}

	for _, err := range []error{
		driverErr,
		fmt.Errorf("create run: %w", driverErr),
		fmt.Errorf("create run: %w", db.Classify(driverErr)),
		errors.New("pq: relation \"secret_table\" does not exist"),
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/runs", nil)
		WriteError(rec, req, err)

		body := rec.Body.String()
		for _, leak := range []string{
			"runs_org_id_idempotency_key_key", "idempotency_key", "duplicate key",
			"secret_table", "relation", "Key (org_id",
		} {
			if strings.Contains(body, leak) {
				t.Fatalf("response body leaked %q:\n%s", leak, body)
			}
		}
	}
}

func TestWriteErrorRendersTheEnvelope(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/signup", nil)
	WriteError(rec, req, Invalid(FieldErrors{"email": "must be an e-mail address"}))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type: got %q", ct)
	}
	// no-store: an error body can name the caller's own organization.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control: got %q, want no-store", cc)
	}

	var envelope struct {
		Error struct {
			Code    Code `json:"code"`
			Message string
			Details struct {
				Fields map[string]string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, rec.Body.String())
	}
	if envelope.Error.Code != CodeValidationFailed {
		t.Fatalf("code: got %q", envelope.Error.Code)
	}
	if got := envelope.Error.Details.Fields["email"]; got != "must be an e-mail address" {
		t.Fatalf("details.fields.email: got %q", got)
	}
}

// A 499 has nobody to answer, so nothing is written at all.
func TestWriteErrorWritesNothingToADisconnectedClient(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/me", nil)
	WriteError(rec, req, fmt.Errorf("load user: %w", context.Canceled))

	if rec.Body.Len() != 0 {
		t.Fatalf("wrote a body to a disconnected client: %s", rec.Body.String())
	}
}

// The Handler adapter is what lets a handler `return err` instead of
// remembering to write a response on every failure path.
func TestHandlerRendersReturnedErrors(t *testing.T) {
	t.Parallel()

	h := Handler(func(_ http.ResponseWriter, _ *http.Request) error {
		return Conflict("that already exists")
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/orgs", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"conflict"`) {
		t.Fatalf("body: %s", rec.Body.String())
	}
}
