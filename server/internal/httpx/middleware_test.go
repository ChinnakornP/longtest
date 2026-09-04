package httpx

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	t.Parallel()

	var seen string
	h := Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}), RequestID(discardLogger()))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	if seen == "" {
		t.Fatal("no request id in the context")
	}
	if got := rec.Header().Get("X-Request-ID"); got != seen {
		t.Fatalf("response header %q does not match the context id %q", got, seen)
	}
}

// An inbound id is echoed into logs and a response header, so a client must
// not be able to put a newline or 4 KiB of text there.
func TestRequestIDRejectsUnsafeInboundValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		inbound string
		keep    bool
	}{
		{"a sane trace id", "trace-abc_123.4", true},
		{"a uuid", "018f3a8d-9c1a-7111-a222-0123456789ab", true},
		{"log injection", "abc\ninjected=\"line\"", false},
		{"header injection", "abc\r\nX-Admin: true", false},
		{"too long", strings.Repeat("a", 65), false},
		{"spaces", "two words", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var seen string
			h := Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
			}), RequestID(discardLogger()))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
			// Set directly: http.Header.Set would not carry an invalid value.
			req.Header["X-Request-Id"] = []string{tt.inbound}

			h.ServeHTTP(httptest.NewRecorder(), req)

			if tt.keep && seen != tt.inbound {
				t.Fatalf("dropped a usable inbound id: got %q, want %q", seen, tt.inbound)
			}
			if !tt.keep && seen == tt.inbound {
				t.Fatalf("kept an unsafe inbound id: %q", seen)
			}
			if seen == "" {
				t.Fatal("no id was assigned")
			}
		})
	}
}

func TestRecoverTurnsAPanicIntoTheEnvelope(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a nil map write, say")
	}), RequestID(logger), Recover())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/me", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("body: %s", rec.Body.String())
	}
	// The stack belongs in the log, never in the response.
	if strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("the response leaked a stack trace: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "handler panicked") {
		t.Fatalf("the panic was not logged: %s", logs.String())
	}
}

func TestAccessLogRecordsTheStatusTheClientSaw(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	h := Chain(Handler(func(_ http.ResponseWriter, _ *http.Request) error {
		return NotFound("no such run")
	}), RequestID(logger), AccessLog())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/runs/x", nil))

	line := logs.String()
	if !strings.Contains(line, `"status":404`) {
		t.Fatalf("access log did not record the status: %s", line)
	}
	if !strings.Contains(line, `"request_id"`) {
		t.Fatalf("access log is missing the request id: %s", line)
	}
}

// The query string is where a token most often ends up by accident.
func TestAccessLogDoesNotRecordTheQueryString(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), RequestID(logger), AccessLog())

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/runs?token=super-secret-value", nil))

	if strings.Contains(logs.String(), "super-secret-value") {
		t.Fatalf("the access log recorded a query-string value: %s", logs.String())
	}
}

func TestCORS(t *testing.T) {
	t.Parallel()

	const allowed = "http://localhost:3000"
	mw := CORS(CORSConfig{AllowedOrigins: []string{allowed}, MaxAge: time.Minute})
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), mw)

	t.Run("an allowed origin gets credentialed CORS headers", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Origin", allowed)
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowed {
			t.Fatalf("allow-origin: got %q, want %q", got, allowed)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Fatalf("allow-credentials: got %q", got)
		}
		if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
			t.Fatal("Vary must include Origin or a cache can cross origins")
		}
	})

	t.Run("an unlisted origin gets none", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		h.ServeHTTP(rec, req)

		// With Allow-Credentials, reflecting an arbitrary origin would let any
		// site read a signed-in user's data.
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("allow-origin was set for an unlisted origin: %q", got)
		}
	})

	t.Run("preflight advertises X-Org-ID", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/api/v1/orgs", nil)
		req.Header.Set("Origin", allowed)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status: got %d, want 204", rec.Code)
		}
		// Without this the web app cannot send the header the tenancy
		// middleware requires, and every call fails with a 403.
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "X-Org-ID") {
			t.Fatalf("allow-headers: got %q", rec.Header().Get("Access-Control-Allow-Headers"))
		}
		if got := rec.Header().Get("Access-Control-Max-Age"); got != "60" {
			t.Fatalf("max-age: got %q, want 60", got)
		}
	})
}

func TestTimeoutCancelsTheHandlerContext(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(done)
		w.WriteHeader(http.StatusOK)
	}), Timeout(10*time.Millisecond))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/me", nil))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler context was never cancelled")
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), SecurityHeaders())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s: got %q, want %q", header, got, want)
		}
	}
}
