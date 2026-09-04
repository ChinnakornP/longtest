package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type decodeTarget struct {
	Email string `json:"email"`
	Count int    `json:"count"`
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
		wantErr     bool
		wantStatus  int
		wantCode    Code
	}{
		{
			name:        "a well-formed object",
			contentType: "application/json",
			body:        `{"email":"a@example.com","count":3}`,
		},
		{
			name:        "content type with a charset",
			contentType: "application/json; charset=utf-8",
			body:        `{"email":"a@example.com"}`,
		},
		{
			name:        "no content type",
			body:        `{"email":"a@example.com"}`,
			wantErr:     true,
			wantStatus:  http.StatusUnsupportedMediaType,
			wantCode:    CodeUnsupportedMediaType,
			contentType: "",
		},
		{
			name:        "form content type",
			contentType: "application/x-www-form-urlencoded",
			body:        `email=a@example.com`,
			wantErr:     true,
			wantStatus:  http.StatusUnsupportedMediaType,
			wantCode:    CodeUnsupportedMediaType,
		},
		{
			name:        "malformed json",
			contentType: "application/json",
			body:        `{"email":`,
			wantErr:     true,
			wantStatus:  http.StatusBadRequest,
			wantCode:    CodeBadRequest,
		},
		{
			name:        "empty body",
			contentType: "application/json",
			body:        ``,
			wantErr:     true,
			wantStatus:  http.StatusBadRequest,
			wantCode:    CodeBadRequest,
		},
		{
			// A misspelled field would otherwise be silently dropped, and the
			// endpoint would act on a zero value the caller never sent.
			name:        "unknown field",
			contentType: "application/json",
			body:        `{"e-mail":"a@example.com"}`,
			wantErr:     true,
			wantStatus:  http.StatusUnprocessableEntity,
			wantCode:    CodeValidationFailed,
		},
		{
			name:        "wrong type for a field",
			contentType: "application/json",
			body:        `{"count":"three"}`,
			wantErr:     true,
			wantStatus:  http.StatusUnprocessableEntity,
			wantCode:    CodeValidationFailed,
		},
		{
			name:        "two json values in one body",
			contentType: "application/json",
			body:        `{"email":"a@example.com"}{"email":"b@example.com"}`,
			wantErr:     true,
			wantStatus:  http.StatusBadRequest,
			wantCode:    CodeBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			var dst decodeTarget
			err := DecodeJSON(httptest.NewRecorder(), req, &dst)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("DecodeJSON: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			apiErr := AsError(err)
			if apiErr.Status != tt.wantStatus {
				t.Fatalf("status: got %d, want %d (%v)", apiErr.Status, tt.wantStatus, err)
			}
			if apiErr.Code != tt.wantCode {
				t.Fatalf("code: got %q, want %q", apiErr.Code, tt.wantCode)
			}
		})
	}
}

func TestDecodeJSONBoundsTheBody(t *testing.T) {
	t.Parallel()

	// One byte of JSON structure plus a string larger than the whole limit.
	body := `{"email":"` + strings.Repeat("a", int(DefaultMaxBodyBytes)+1) + `"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	var dst decodeTarget
	err := DecodeJSON(httptest.NewRecorder(), req, &dst)
	if err == nil {
		t.Fatal("an oversized body must be rejected")
	}
	if apiErr := AsError(err); apiErr.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413 (%v)", apiErr.Status, err)
	}
}

// The message must not quote the body: a login body contains a password.
func TestDecodeJSONDoesNotEchoTheBody(t *testing.T) {
	t.Parallel()

	const password = "hunter2-is-the-password" //nolint:gosec // test fixture, not a credential
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"a@example.com","password":"`+password+`"`))
	req.Header.Set("Content-Type", "application/json")

	var dst decodeTarget
	err := DecodeJSON(httptest.NewRecorder(), req, &dst)
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if strings.Contains(AsError(err).Message, password) {
		t.Fatalf("the error message echoed the request body: %s", AsError(err).Message)
	}
}

func TestPathUUID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"a uuid", "018f3a8d-9c1a-7111-a222-0123456789ab", false},
		{"missing", "", true},
		{"not a uuid", "not-a-uuid", true},
		{"sql injection attempt", "1' OR '1'='1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/orgs/x/members", nil)
			req.SetPathValue("orgID", tt.value)

			_, err := PathUUID(req, "orgID")
			if tt.wantErr != (err != nil) {
				t.Fatalf("PathUUID(%q): got %v, wantErr=%v", tt.value, err, tt.wantErr)
			}
			if err != nil && AsError(err).Status != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400", AsError(err).Status)
			}
		})
	}
}
