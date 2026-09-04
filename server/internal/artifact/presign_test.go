package artifact

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The signing implementation is checked against AWS's own documented example
// for a presigned GET ("Authenticating Requests: Using Query Parameters").
// Getting a signature almost right produces a URL that looks fine and is
// rejected by the store, so a fixed vector is the only assertion worth making
// here — a self-consistency test would pass with a wrong canonical request.
func TestPresignMatchesTheDocumentedAWSVector(t *testing.T) {
	endpoint, err := url.Parse("https://s3.amazonaws.com")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	p := &presigner{
		endpoint: endpoint,
		region:   "us-east-1",
		bucket:   "examplebucket",
		creds: Credentials{
			AccessKeyID: "AKIAIOSFODNN7EXAMPLE",
			// The documented example key. It is a published test vector, not a
			// credential: it has never been valid for anything.
			SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
	}

	got, err := p.presign("GET", "test.txt", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC), 24*time.Hour)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	const want = "https://examplebucket.s3.amazonaws.com/test.txt?" +
		"X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z" +
		"&X-Amz-Expires=86400" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404" +
		"&X-Amz-SignedHeaders=host"
	if got != want {
		t.Fatalf("presigned url does not match the documented vector\n got: %s\nwant: %s", got, want)
	}
}

// Path style puts the bucket in the path and leaves the host alone. It is what
// MinIO and every bare-IP endpoint require, and it is the default.
func TestPresignPathStyleAddressesTheBucketInThePath(t *testing.T) {
	svc := testService(t, true)
	orgID, runID := uuid.New(), uuid.New()
	day := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	key, err := ObjectKey(orgID, runID, day, "TC-001", "shot.png")
	if err != nil {
		t.Fatalf("object key: %v", err)
	}
	signed, err := svc.PutURL(orgID, runID, day, key)
	if err != nil {
		t.Fatalf("put url: %v", err)
	}

	wantPath := "http://127.0.0.1:9000/qa-artifacts/" + key
	if !strings.HasPrefix(signed.URL, wantPath+"?") {
		t.Fatalf("got %s, want a signature on %s", signed.URL, wantPath)
	}
	if signed.Key != key {
		t.Fatalf("got key %q, want %q", signed.Key, key)
	}
}

// The acceptance criterion: a URL this backend issues can only write inside the
// prefix of the org and run it was issued for.
func TestPutURLRefusesKeysOutsideTheRunPrefix(t *testing.T) {
	svc := testService(t, true)
	orgID, runID := uuid.New(), uuid.New()
	otherOrg, otherRun := uuid.New(), uuid.New()
	day := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	prefix := KeyPrefix(orgID, runID, day)
	tests := []struct {
		name string
		key  string
	}{
		{"another organization", KeyPrefix(otherOrg, runID, day) + "shot.png"},
		{"another run", KeyPrefix(orgID, otherRun, day) + "shot.png"},
		{"another day", KeyPrefix(orgID, runID, day.AddDate(0, 0, 1)) + "shot.png"},
		{"traversal out of the prefix", prefix + "../../../etc/passwd"},
		{"traversal as a single segment", prefix + "../shot.png"},
		{"a dotfile", prefix + ".ssh"},
		{"too deep", prefix + "TC-001/nested/shot.png"},
		{"no prefix at all", "shot.png"},
		{"prefix as a substring, not a prefix", "evil/" + prefix + "shot.png"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.PutURL(orgID, runID, day, tc.key); err == nil {
				t.Fatalf("signed %q, which is outside %q", tc.key, prefix)
			}
		})
	}
}

// A run-level artifact has no test-case segment; an execution-level one has
// exactly one. Anything deeper is not a key this layout describes.
func TestObjectKeyLayout(t *testing.T) {
	orgID, runID := uuid.New(), uuid.New()
	day := time.Date(2026, 9, 4, 23, 59, 0, 0, time.UTC)
	prefix := KeyPrefix(orgID, runID, day)

	runLevel, err := ObjectKey(orgID, runID, day, "", "discovery.har")
	if err != nil {
		t.Fatalf("run-level key: %v", err)
	}
	if runLevel != prefix+"discovery.har" {
		t.Fatalf("got %q, want %q", runLevel, prefix+"discovery.har")
	}

	perCase, err := ObjectKey(orgID, runID, day, "TC-001", "trace.zip")
	if err != nil {
		t.Fatalf("per-case key: %v", err)
	}
	if perCase != prefix+"TC-001/trace.zip" {
		t.Fatalf("got %q, want %q", perCase, prefix+"TC-001/trace.zip")
	}

	// A file name can be derived from something the daemon read off the page
	// under test, so a separator or a traversal segment is rejected rather than
	// escaped.
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "-leading-dash", ".dotfile"} {
		if _, err := ObjectKey(orgID, runID, day, "", bad); err == nil {
			t.Errorf("accepted the file name %q", bad)
		}
	}
}

// A download URL is scoped to the caller's own organization, so a storage key
// that arrived from anywhere else cannot be turned into a readable link.
func TestGetURLRefusesAnotherOrganizationsKey(t *testing.T) {
	svc := testService(t, true)
	orgID, otherOrg, runID := uuid.New(), uuid.New(), uuid.New()
	day := time.Now()

	if _, err := svc.GetURL(orgID, KeyPrefix(otherOrg, runID, day)+"shot.png"); err == nil {
		t.Fatal("signed a download for another organization's key")
	}
	if _, err := svc.GetURL(orgID, KeyPrefix(orgID, runID, day)+"shot.png"); err != nil {
		t.Fatalf("refused this organization's own key: %v", err)
	}
}

// A deployment with no object storage answers 503 rather than handing out URLs
// that point nowhere.
func TestDisabledServiceIsUnavailableRatherThanBroken(t *testing.T) {
	svc := Disabled()
	if svc.Enabled() {
		t.Fatal("a disabled service reports itself enabled")
	}
	if _, err := svc.PutURL(uuid.New(), uuid.New(), time.Now(), "orgs/x/runs/y/z"); err == nil {
		t.Fatal("a disabled service signed an upload")
	}
}

// The upload window is bounded by the contract at six hours however it is
// configured: a grant that outlives the run it was issued for is a standing
// capability.
func TestUploadWindowIsCappedAtTheContractCeiling(t *testing.T) {
	svc, err := NewService(Config{
		Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "qa-artifacts",
		Credentials:  Credentials{AccessKeyID: "key", SecretAccessKey: "secret"},
		PathStyle:    true,
		UploadWindow: 48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if got := svc.UploadWindow(); got != MaxUploadWindow {
		t.Fatalf("got an upload window of %s, want it capped at %s", got, MaxUploadWindow)
	}
}

func testService(t *testing.T, pathStyle bool) *Service {
	t.Helper()
	svc, err := NewService(Config{
		Endpoint: "http://127.0.0.1:9000",
		Region:   "us-east-1",
		Bucket:   "qa-artifacts",
		// Not a credential: these values only ever sign URLs that are compared
		// to a string in this test.
		Credentials: Credentials{AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-key"},
		PathStyle:   pathStyle,
		PresignTTL:  15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}
