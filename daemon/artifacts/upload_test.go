package artifacts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

const testPrefix = "orgs/8fbd0a86/runs/2026-09-04/2f1c9d6e/"

func testSpec(base string) qaschema.ArtifactUpload {
	return qaschema.ArtifactUpload{
		PresignedPutBase: base,
		KeyPrefix:        testPrefix,
		ExpiresAt:        time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

func fixedClock(t time.Time) UploaderOption {
	return WithClock(
		func() time.Time { return t },
		func(context.Context, time.Duration) error { return nil },
	)
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestValidatePrefix(t *testing.T) {
	valid := []string{
		testPrefix,
		"orgs/org1/runs/2026-01-31/run1/",
	}
	for _, prefix := range valid {
		if err := ValidatePrefix(prefix); err != nil {
			t.Fatalf("ValidatePrefix(%q) = %v, want nil", prefix, err)
		}
	}

	invalid := []string{
		"",
		"runs/2026-09-04/run/",                  // no org
		"orgs/org1/runs/2026-09-04/run1",        // no trailing slash
		"orgs/../runs/2026-09-04/run1/",         // traversal in org
		"orgs/org1/runs/2026-09-04/..//",        // traversal in run
		"orgs/org1/runs/20260904/run1/",         // wrong date shape
		"orgs/org1/runs/2026-09-04/run1/extra/", // too deep
		"/orgs/org1/runs/2026-09-04/run1/",      // absolute
		"orgs/org1/runs/2026-09-04/.hidden/",    // segment must start alnum
	}
	for _, prefix := range invalid {
		if err := ValidatePrefix(prefix); err == nil {
			t.Fatalf("ValidatePrefix(%q) = nil, want an error", prefix)
		}
	}
}

func TestKeyUnder(t *testing.T) {
	got, err := KeyUnder(testPrefix, "TC-001", "screenshot-1.png")
	if err != nil {
		t.Fatalf("KeyUnder: %v", err)
	}
	if want := testPrefix + "TC-001/screenshot-1.png"; got != want {
		t.Fatalf("KeyUnder = %q, want %q", got, want)
	}

	runLevel, err := KeyUnder(testPrefix, "discovery.har")
	if err != nil {
		t.Fatalf("KeyUnder run-level: %v", err)
	}
	if want := testPrefix + "discovery.har"; runLevel != want {
		t.Fatalf("KeyUnder = %q, want %q", runLevel, want)
	}

	for _, segments := range [][]string{
		{},
		{".."},
		{"TC-001", "../../etc/passwd"},
		{"TC-001", ""},
		{"TC-001", ".hidden"},
		{"TC-001", "a/b"},
	} {
		if got, err := KeyUnder(testPrefix, segments...); err == nil {
			t.Fatalf("KeyUnder(%v) = %q, want an error", segments, got)
		}
	}
}

func TestKeyWithinPrefix(t *testing.T) {
	if err := KeyWithinPrefix(testPrefix, testPrefix+"TC-001/trace.zip"); err != nil {
		t.Fatalf("KeyWithinPrefix: %v", err)
	}
	outside := []string{
		"orgs/other/runs/2026-09-04/2f1c9d6e/TC-001/trace.zip",
		testPrefix,
		testPrefix + "../escape.zip",
		testPrefix + "TC-001/",
	}
	for _, key := range outside {
		if err := KeyWithinPrefix(testPrefix, key); !errors.Is(err, ErrOutsidePrefix) {
			t.Fatalf("KeyWithinPrefix(%q) = %v, want ErrOutsidePrefix", key, err)
		}
	}
}

func TestUploadPutsFileAndReportsDigest(t *testing.T) {
	var (
		mu       sync.Mutex
		gotPath  string
		gotQuery string
		gotType  string
		gotBody  string
		gotLen   int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotQuery, gotType, gotBody, gotLen = r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), string(body), r.ContentLength
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	up, err := NewUploader(testSpec(srv.URL+"/qa-artifacts?X-Amz-Signature=abc"),
		fixedClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}

	path := writeTempFile(t, "trace.zip", "trace-bytes")
	key := testPrefix + "TC-001/trace.zip"
	artifact, err := up.Upload(t.Context(), Upload{Key: key, Path: path, Kind: qaschema.ArtifactKindTrace})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := "/qa-artifacts/" + key; gotPath != want {
		t.Fatalf("PUT path = %q, want %q", gotPath, want)
	}
	if gotQuery != "X-Amz-Signature=abc" {
		t.Fatalf("presigned query lost: %q", gotQuery)
	}
	if gotBody != "trace-bytes" {
		t.Fatalf("body = %q", gotBody)
	}
	if gotLen != int64(len("trace-bytes")) {
		t.Fatalf("Content-Length = %d, want %d", gotLen, len("trace-bytes"))
	}
	if gotType != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", gotType)
	}
	if artifact.Key != key || artifact.Kind != qaschema.ArtifactKindTrace {
		t.Fatalf("artifact = %+v", artifact)
	}
	if artifact.Sha256 == nil || len(*artifact.Sha256) != 64 {
		t.Fatalf("artifact sha256 = %v", artifact.Sha256)
	}
	if artifact.SizeBytes == nil || *artifact.SizeBytes != len("trace-bytes") {
		t.Fatalf("artifact size = %v", artifact.SizeBytes)
	}
	if artifact.ID == "" {
		t.Fatal("artifact id is empty")
	}

	// The record must satisfy the contract the backend validates against.
	result, err := qaschema.Validate("execution-result@1", map[string]any{
		"version":    1,
		"testCaseId": "TC-001",
		"result":     "pass",
		"steps":      []any{},
		"artifacts":  []any{artifactAsMap(t, artifact)},
		"startedAt":  "2026-09-04T12:00:00Z",
		"endedAt":    "2026-09-04T12:00:01Z",
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !result.Valid {
		t.Fatalf("artifact record is not contract-valid: %v", result.Errors)
	}
}

func TestUploadRetriesServerErrors(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "png-bytes" {
			t.Errorf("retry sent %q, want the whole file", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	up, err := NewUploader(testSpec(srv.URL+"/bucket"),
		fixedClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)),
		WithRetry(3, time.Millisecond))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}

	path := writeTempFile(t, "shot.png", "png-bytes")
	if _, err := up.Upload(t.Context(), Upload{
		Key: testPrefix + "TC-001/shot.png", Path: path, Kind: qaschema.ArtifactKindScreenshot,
	}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestUploadDoesNotRetryClientErrors(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, "signature expired", http.StatusForbidden)
	}))
	defer srv.Close()

	up, err := NewUploader(testSpec(srv.URL+"/bucket"),
		fixedClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)),
		WithRetry(3, time.Millisecond))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}

	path := writeTempFile(t, "console.json", "{}")
	_, err = up.Upload(t.Context(), Upload{
		Key: testPrefix + "TC-001/console.json", Path: path, Kind: qaschema.ArtifactKindConsole,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v, want it to name the status", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (a 403 is not retryable)", attempts)
	}
}

func TestUploadRefusesExpiredWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("expired credentials must not reach the network")
	}))
	defer srv.Close()

	up, err := NewUploader(testSpec(srv.URL+"/bucket"),
		fixedClock(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}

	path := writeTempFile(t, "shot.png", "x")
	_, err = up.Upload(t.Context(), Upload{
		Key: testPrefix + "TC-001/shot.png", Path: path, Kind: qaschema.ArtifactKindScreenshot,
	})
	if !errors.Is(err, ErrPrefixExpired) {
		t.Fatalf("error = %v, want ErrPrefixExpired", err)
	}
}

func TestUploadRefusesKeyOutsidePrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a key outside the run prefix must never be uploaded")
	}))
	defer srv.Close()

	up, err := NewUploader(testSpec(srv.URL+"/bucket"),
		fixedClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}

	path := writeTempFile(t, "shot.png", "x")
	_, err = up.Upload(t.Context(), Upload{
		Key:  "orgs/someone-else/runs/2026-09-04/other/TC-001/shot.png",
		Path: path,
		Kind: qaschema.ArtifactKindScreenshot,
	})
	if !errors.Is(err, ErrOutsidePrefix) {
		t.Fatalf("error = %v, want ErrOutsidePrefix", err)
	}
}

func TestUploadAllStopsAtFirstFailure(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "second.json") {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	up, err := NewUploader(testSpec(srv.URL+"/bucket"),
		fixedClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}

	dir := t.TempDir()
	uploads := make([]Upload, 0, 3)
	for _, name := range []string{"first.json", "second.json", "third.json"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		uploads = append(uploads, Upload{
			Key: testPrefix + "TC-001/" + name, Path: path, Kind: qaschema.ArtifactKindConsole,
		})
	}

	done, err := up.UploadAll(t.Context(), uploads)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(done) != 1 {
		t.Fatalf("returned %d artifacts, want the one that succeeded", len(done))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("requests = %v, want the run to stop after the failure", seen)
	}
}

func TestNewUploaderRejectsBadSpec(t *testing.T) {
	tests := []struct {
		name string
		spec qaschema.ArtifactUpload
	}{
		{"bad prefix", qaschema.ArtifactUpload{PresignedPutBase: "https://s3/bucket", KeyPrefix: "runs/x/", ExpiresAt: "2026-09-04T23:00:00Z"}},
		{"non-http base", qaschema.ArtifactUpload{PresignedPutBase: "file:///tmp", KeyPrefix: testPrefix, ExpiresAt: "2026-09-04T23:00:00Z"}},
		{"bad expiry", qaschema.ArtifactUpload{PresignedPutBase: "https://s3/bucket", KeyPrefix: testPrefix, ExpiresAt: "soon"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewUploader(tt.spec); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestUploadHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Bounded, so the test server can always shut down: the client-side
		// cancellation is what this test asserts on, not the server's.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	up, err := NewUploader(testSpec(srv.URL+"/bucket"),
		fixedClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	path := writeTempFile(t, "shot.png", "x")
	_, err = up.Upload(ctx, Upload{Key: testPrefix + "TC-001/shot.png", Path: path, Kind: qaschema.ArtifactKindScreenshot})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestURLForEscapesKeySegments(t *testing.T) {
	up, err := NewUploader(testSpec("https://storage.example/bucket?sig=1"),
		fixedClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}
	got, err := up.urlFor(testPrefix + "TC-001/trace.zip")
	if err != nil {
		t.Fatalf("urlFor: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.RawQuery != "sig=1" {
		t.Fatalf("query = %q, want the presigned one preserved", parsed.RawQuery)
	}
	if want := "/bucket/" + testPrefix + "TC-001/trace.zip"; parsed.Path != want {
		t.Fatalf("path = %q, want %q", parsed.Path, want)
	}
}

func artifactAsMap(t *testing.T, a qaschema.Artifact) map[string]any {
	t.Helper()
	m := map[string]any{"id": a.ID, "kind": string(a.Kind), "key": a.Key}
	if a.ContentType != nil {
		m["contentType"] = *a.ContentType
	}
	if a.SizeBytes != nil {
		m["sizeBytes"] = float64(*a.SizeBytes)
	}
	if a.Sha256 != nil {
		m["sha256"] = *a.Sha256
	}
	return m
}
