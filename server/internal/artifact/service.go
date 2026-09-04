package artifact

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// MaxUploadWindow caps how long a run's upload window may stay open, per the
// task contract: long enough for a whole run, never long enough to be a
// standing capability. It bounds Config.UploadWindow, not the lifetime of an
// individual signed URL.
const MaxUploadWindow = 6 * time.Hour

// Config is the object-storage half of the process configuration. Everything
// here comes from the environment; nothing is defaulted to a real endpoint, so
// a deployment that forgets to configure storage gets a clear 503 rather than
// URLs pointing at somebody else's bucket.
type Config struct {
	// Endpoint is the storage API base, e.g. http://127.0.0.1:9000 for MinIO.
	Endpoint string
	// PublicEndpoint is the base a DAEMON can reach, when that differs from
	// the one the backend uses (compose network vs. developer machine). Empty
	// means "the same as Endpoint".
	PublicEndpoint string
	Region         string
	Bucket         string
	Credentials    Credentials
	// PathStyle addresses the bucket as {endpoint}/{bucket}/{key}. Required by
	// MinIO and by any endpoint that is a bare IP.
	PathStyle bool
	// PresignTTL is the lifetime of one signed URL. It has to outlive a slow
	// upload of a multi-megabyte trace over a customer's uplink.
	PresignTTL time.Duration
	// UploadWindow is how long after assignment a run may keep minting upload
	// URLs. Bounded by MaxUploadWindow.
	UploadWindow time.Duration
}

// Service mints the presigned URLs. A zero Service is a valid, disabled one:
// every method returns a 503 and no caller has to nil-check.
type Service struct {
	presigner    *presigner
	public       *presigner
	presignTTL   time.Duration
	uploadWindow time.Duration
	// now is overridable so tests can assert on an exact signature.
	now func() time.Time
}

// Disabled returns a Service for a deployment with no object storage
// configured. Report and run.assign degrade to "no upload URLs" instead of
// failing, which is what keeps the API usable in a test or a demo without
// MinIO.
func Disabled() *Service { return &Service{} }

// NewService validates cfg and returns the service it describes.
func NewService(cfg Config) (*Service, error) {
	endpoint, err := parseEndpoint("S3_ENDPOINT", cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	publicEndpoint := endpoint
	if cfg.PublicEndpoint != "" {
		if publicEndpoint, err = parseEndpoint("S3_PUBLIC_ENDPOINT", cfg.PublicEndpoint); err != nil {
			return nil, err
		}
	}
	switch {
	case cfg.Bucket == "":
		return nil, fmt.Errorf("S3_BUCKET is required")
	case cfg.Region == "":
		return nil, fmt.Errorf("S3_REGION is required")
	case cfg.Credentials.AccessKeyID == "" || cfg.Credentials.SecretAccessKey == "":
		return nil, fmt.Errorf("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required")
	}

	presignTTL := cfg.PresignTTL
	if presignTTL <= 0 {
		presignTTL = 15 * time.Minute
	}
	uploadWindow := cfg.UploadWindow
	if uploadWindow <= 0 || uploadWindow > MaxUploadWindow {
		uploadWindow = MaxUploadWindow
	}

	return &Service{
		presigner:    &presigner{endpoint: endpoint, region: cfg.Region, bucket: cfg.Bucket, creds: cfg.Credentials, pathStyle: cfg.PathStyle},
		public:       &presigner{endpoint: publicEndpoint, region: cfg.Region, bucket: cfg.Bucket, creds: cfg.Credentials, pathStyle: cfg.PathStyle},
		presignTTL:   presignTTL,
		uploadWindow: uploadWindow,
		now:          time.Now,
	}, nil
}

func parseEndpoint(name, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%s must be an absolute http(s) url, got %q", name, raw)
	}
	return u, nil
}

// Enabled reports whether object storage is configured.
func (s *Service) Enabled() bool { return s != nil && s.presigner != nil }

// UploadWindow is how long a run may keep asking for upload URLs.
func (s *Service) UploadWindow() time.Duration {
	if !s.Enabled() {
		return 0
	}
	return s.uploadWindow
}

// SignedURL is one capability: a URL, the method it authorises, and when it
// stops working.
type SignedURL struct {
	URL       string    `json:"url"`
	Key       string    `json:"key"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// PutURL signs an upload of exactly one object inside one run's prefix.
//
// The key is checked against the prefix derived from orgID and runID here, not
// taken on trust, which is the tenant boundary this whole package exists to
// hold: the caller passes the org and run it authenticated, and a key naming
// any other org or run cannot be signed.
func (s *Service) PutURL(orgID, runID uuid.UUID, day time.Time, key string) (SignedURL, error) {
	if !s.Enabled() {
		return SignedURL{}, errStorageDisabled()
	}
	if err := CheckKeyUnderPrefix(KeyPrefix(orgID, runID, day), key); err != nil {
		return SignedURL{}, httpx.Forbidden("%s", err.Error())
	}

	now := s.now()
	signed, err := s.public.presign("PUT", key, now, s.presignTTL)
	if err != nil {
		return SignedURL{}, fmt.Errorf("presign put %s: %w", key, err)
	}
	return SignedURL{URL: signed, Key: key, ExpiresAt: now.Add(s.presignTTL).UTC()}, nil
}

// GetURL signs a download for the web app.
//
// orgID is the caller's organization, and the key must live under it. An
// artifact row is already org-scoped by the query that read it, so this is a
// second gate rather than the only one — but it is the gate that makes a
// storage key handed in from anywhere else unusable.
func (s *Service) GetURL(orgID uuid.UUID, key string) (SignedURL, error) {
	if !s.Enabled() {
		return SignedURL{}, errStorageDisabled()
	}
	if !strings.HasPrefix(key, "orgs/"+orgID.String()+"/") {
		return SignedURL{}, httpx.Forbidden("that artifact does not belong to this organization")
	}

	now := s.now()
	signed, err := s.public.presign("GET", key, now, s.presignTTL)
	if err != nil {
		return SignedURL{}, fmt.Errorf("presign get %s: %w", key, err)
	}
	return SignedURL{URL: signed, Key: key, ExpiresAt: now.Add(s.presignTTL).UTC()}, nil
}

func errStorageDisabled() error {
	return &httpx.Error{
		Status:  503,
		Code:    httpx.CodeUnavailable,
		Message: "object storage is not configured on this deployment",
	}
}
