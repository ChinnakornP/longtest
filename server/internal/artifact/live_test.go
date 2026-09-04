package artifact

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This test drives a real S3-compatible store, because the fixed vector in
// presign_test.go proves the signature matches AWS's own example and nothing
// more: whether the URL we hand a daemon is actually accepted depends on the
// host we address, the path style, and the payload mode as well.
//
// It is opt-in. `make up` starts MinIO; point the variables at it to run this:
//
//	TEST_S3_ENDPOINT=http://127.0.0.1:9000 TEST_S3_BUCKET=qa-artifacts \
//	TEST_S3_ACCESS_KEY_ID=... TEST_S3_SECRET_ACCESS_KEY=... \
//	go test ./internal/artifact -run Live -count=1
func TestLivePresignedRoundTrip(t *testing.T) {
	svc, bucket := liveService(t)

	orgID, runID := uuid.New(), uuid.New()
	day := time.Now()
	key, err := ObjectKey(orgID, runID, day, "TC-001", "shot.png")
	if err != nil {
		t.Fatalf("object key: %v", err)
	}
	const content = "evidence-bytes"

	put, err := svc.PutURL(orgID, runID, day, key)
	if err != nil {
		t.Fatalf("put url: %v", err)
	}
	if status, body := do(t, http.MethodPut, put.URL, content); status != http.StatusOK {
		t.Fatalf("presigned PUT was rejected: %d %s", status, body)
	}

	get, err := svc.GetURL(orgID, key)
	if err != nil {
		t.Fatalf("get url: %v", err)
	}
	status, body := do(t, http.MethodGet, get.URL, "")
	if status != http.StatusOK || body != content {
		t.Fatalf("presigned GET returned %d %q, want 200 %q", status, body, content)
	}

	// The capability is for one object. Reusing the signature on a different
	// key is the failure mode a prefix-wide grant would have; this is the
	// evidence that the store, not just this package, enforces the bound.
	other, err := ObjectKey(orgID, runID, day, "TC-002", "shot.png")
	if err != nil {
		t.Fatalf("object key: %v", err)
	}
	swapped := strings.Replace(put.URL, "/"+bucket+"/"+key+"?", "/"+bucket+"/"+other+"?", 1)
	if status, _ := do(t, http.MethodPut, swapped, "x"); status < 400 {
		t.Fatalf("a signature for %s was accepted for %s (status %d)", key, other, status)
	}
}

func liveService(t *testing.T) (*Service, string) {
	t.Helper()

	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	bucket := os.Getenv("TEST_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set TEST_S3_ENDPOINT and TEST_S3_BUCKET to run the live object-storage test")
	}

	svc, err := NewService(Config{
		Endpoint: endpoint,
		Region:   stringOr(os.Getenv("TEST_S3_REGION"), "us-east-1"),
		Bucket:   bucket,
		Credentials: Credentials{
			AccessKeyID:     os.Getenv("TEST_S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("TEST_S3_SECRET_ACCESS_KEY"),
		},
		PathStyle:  true,
		PresignTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, bucket
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("build %s request: %v", method, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	return resp.StatusCode, string(payload)
}

func stringOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
